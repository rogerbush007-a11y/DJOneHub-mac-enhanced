//go:build darwin && cgo

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	voiceRemoteDir        = "/tmp/mavo-call"
	voiceRoutePIDFile     = "/run/mavo-voice-route.pid"
	voiceRouteLogFile     = "/run/mavo-voice-route.log"
	voiceCalibrationPID   = "/run/mavo-alsaucm.pid"
	voiceCalibrationLog   = "/run/mavo-alsaucm.log"
	voiceRouteRetryWindow = 30 * time.Second
)

type voiceRuntimeManifest struct {
	FormatVersion  int    `json:"formatVersion"`
	RuntimeVersion string `json:"runtimeVersion"`
	KernelRelease  string `json:"kernelRelease"`
	CardName       string `json:"cardName"`
	Helper         string `json:"helper"`
	Files          []struct {
		Name string `json:"name"`
		Mode uint32 `json:"mode"`
	} `json:"files"`
	Modules []struct {
		File string `json:"file"`
		Name string `json:"name"`
	} `json:"modules"`
	RequiredDevices []string `json:"requiredDevices"`
}

// moduleVoiceSession keeps the prepared ADB transport alive across ATD/ATA.
// This module stops accepting a fresh ADB CNXN once a voice call becomes
// active, while an already connected transport remains usable. The media
// route is still activated only after CLCC reports an active voice call.
type moduleVoiceSession struct {
	adb      *adbClient
	manifest *voiceRuntimeManifest
}

func (a *app) voiceStatus() map[string]any {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	installed, installDetail := upstreamVoiceRuntimeInstalled()
	return map[string]any{
		"prepared":          a.moduleVoicePrepared,
		"ready":             a.moduleVoiceReady,
		"last_attempt":      a.moduleVoiceLast,
		"last_error":        a.moduleVoiceErr,
		"detail":            a.moduleVoiceDetail,
		"runtime_included":  false,
		"runtime_installed": installed,
		"runtime_source":    upstreamVoiceRuntimeSource,
		"runtime_detail":    installDetail,
	}
}

// kickModuleVoice prepares a persistent ADB session and the module runtime,
// but deliberately leaves the D4/UAC media route disabled. Keeping the ADB
// transport alive is necessary on firmware that will not accept a new ADB
// CNXN after a call becomes active.
func (a *app) kickModuleVoice() {
	if err := a.prepareModuleVoiceSession(); err != nil {
		log.Printf("module voice: background preparation failed: %v", err)
		return
	}
	log.Printf("module voice: runtime prepared; media route deferred until call becomes active")
}

func (a *app) prepareModuleVoiceSession() error {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	return a.prepareModuleVoiceSessionLocked()
}

func (a *app) prepareModuleVoiceSessionBudgeted(budget time.Duration) error {
	a.moduleVoiceMu.Lock()
	prepared := a.moduleVoicePrepared
	a.moduleVoiceMu.Unlock()
	if prepared {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- a.prepareModuleVoiceSession() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		return errors.New("语音运行时仍在准备中")
	}
}

func (a *app) prepareModuleVoiceSessionLocked() error {
	if a.moduleVoiceSession != nil && a.moduleVoiceSession.adb != nil {
		return nil
	}
	manifest, err := loadVoiceManifest()
	if err != nil {
		return err
	}
	adb, err := openDJIUSBADB()
	if err != nil {
		return err
	}
	if err := prepareModuleVoiceRuntime(adb, manifest); err != nil {
		adb.Close()
		return err
	}
	a.moduleVoiceSession = &moduleVoiceSession{adb: adb, manifest: manifest}
	a.moduleVoiceMu.Lock()
	a.moduleVoicePrepared = true
	a.moduleVoiceMu.Unlock()
	return nil
}

func (a *app) resetModuleVoiceSession() {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	a.closeModuleVoiceSessionLocked()
}

func (a *app) closeModuleVoiceSessionLocked() {
	if a.moduleVoiceSession != nil && a.moduleVoiceSession.adb != nil {
		a.moduleVoiceSession.adb.Close()
	}
	a.moduleVoiceSession = nil
	a.moduleVoiceMu.Lock()
	a.moduleVoicePrepared = false
	a.moduleVoiceReady = false
	a.moduleVoiceLast = time.Time{}
	a.moduleVoiceMu.Unlock()
}

func (a *app) setVoiceStatus(ready bool, err error, detail string) {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	a.moduleVoiceReady = ready
	a.moduleVoiceLast = time.Now()
	if err != nil {
		a.moduleVoiceErr = err.Error()
	} else {
		a.moduleVoiceErr = ""
	}
	if len(detail) > 2000 {
		detail = detail[len(detail)-2000:]
	}
	a.moduleVoiceDetail = detail
}

// ensureModuleVoiceRoute enables the module-side voice route over ADB so the
// module's USB audio (UAC) carries real call voice. Idempotent and cached;
// failures are cached for voiceRouteRetryWindow to avoid stalling every call.
func (a *app) ensureModuleVoiceRoute() error {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	return a.ensureModuleVoiceRouteLocked()
}

// ensureModuleVoiceRouteLocked runs with moduleVoiceOpMu held so concurrent
// callers (background prepare + dial/answer) never duplicate the full prep.
func (a *app) ensureModuleVoiceRouteLocked() error {
	a.moduleVoiceMu.Lock()
	if a.moduleVoiceReady {
		a.moduleVoiceMu.Unlock()
		return nil
	}
	if time.Since(a.moduleVoiceLast) < voiceRouteRetryWindow {
		errText := a.moduleVoiceErr
		a.moduleVoiceMu.Unlock()
		if errText != "" {
			return fmt.Errorf("语音路由暂不可用：%s", errText)
		}
		return nil
	}
	a.moduleVoiceMu.Unlock()

	log.Printf("module voice: starting route")
	if err := a.prepareModuleVoiceSessionLocked(); err != nil {
		log.Printf("module voice: preparation failed: %v", err)
		a.setVoiceStatus(false, err, "")
		return err
	}
	err := a.startModuleVoiceRouteLocked()
	if err != nil {
		log.Printf("module voice: start failed: %v", err)
		a.setVoiceStatus(false, err, "")
		return err
	}
	a.setVoiceStatus(true, nil, "")
	log.Printf("module voice: route is ready (UAC voice enabled)")
	return nil
}

// ensureModuleVoiceRouteBudgeted waits up to budget for the voice route;
// used before dial/answer so the HTTP request stays within the client timeout.
// The background prep keeps running even when the budget expires.
func (a *app) ensureModuleVoiceRouteBudgeted(budget time.Duration) error {
	a.moduleVoiceMu.Lock()
	ready := a.moduleVoiceReady
	a.moduleVoiceMu.Unlock()
	if ready {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- a.ensureModuleVoiceRoute() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		return errors.New("语音路由仍在准备中")
	}
}

// stopModuleVoiceRoute tears down the module-side voice route after a call.
func (a *app) stopModuleVoiceRoute() {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	defer a.closeModuleVoiceSessionLocked()

	a.moduleVoiceMu.Lock()
	if !a.moduleVoiceReady {
		a.moduleVoiceMu.Unlock()
		return
	}
	// Publish the transition before taking the potentially slow ADB stop path.
	// A concurrent dial/answer will wait on moduleVoiceOpMu and restart after
	// teardown instead of accepting stale "ready" state.
	a.moduleVoiceReady = false
	a.moduleVoiceLast = time.Time{}
	a.moduleVoiceMu.Unlock()

	if err := a.stopModuleVoiceRouteInnerLocked(); err != nil {
		log.Printf("module voice: stop failed: %v", err)
		return
	}
	log.Printf("module voice: route stopped")
}

func prepareModuleVoiceRuntime(adb *adbClient, manifest *voiceRuntimeManifest) error {
	// prepare(): root check, kernel check, deploy runtime, load drivers,
	// calibrate VoLTE ACDB, verify voice endpoints and helper self-test.
	out, status, err := adb.shellChecked("id -u", 8*time.Second)
	if err != nil {
		return fmt.Errorf("ADB 探测失败: %w", err)
	}
	isRoot := false
	for _, field := range strings.Fields(out) {
		if field == "0" {
			isRoot = true
		}
	}
	if status != 0 || !isRoot {
		return fmt.Errorf("模块 ADB 没有 root 控制权限（id -u 返回 %q）", strings.TrimSpace(out))
	}
	release, status, err := adb.shellChecked("uname -r", 8*time.Second)
	if err != nil {
		return err
	}
	if status != 0 || !strings.Contains(release, manifest.KernelRelease) {
		return fmt.Errorf("模块内核版本与通话驱动不匹配：需要 %s，实际 %s", manifest.KernelRelease, strings.TrimSpace(release))
	}
	if err := voiceShell(adb, "mkdir -p '"+voiceRemoteDir+"' && chmod 700 '"+voiceRemoteDir+"'", 8*time.Second); err != nil {
		return err
	}
	for _, entry := range manifest.Files {
		data, rerr := readUpstreamVoiceRuntimeFile(entry.Name)
		if rerr != nil {
			return fmt.Errorf("缺少通话组件: %s", entry.Name)
		}
		if err := adb.push(data, voiceRemoteDir+"/"+entry.Name, 0o100000|entry.Mode, 30*time.Second); err != nil {
			return fmt.Errorf("推送 %s 失败: %w", entry.Name, err)
		}
		log.Printf("module voice: pushed %s (%d bytes)", entry.Name, len(data))
	}

	soundReady, err := voiceSoundDevicesReady(adb, manifest)
	if err != nil {
		return err
	}
	if !soundReady {
		if _, status, _ := adb.shellChecked("grep -q '^qdc507_voice ' /proc/modules", 8*time.Second); status == 0 {
			return errors.New("检测到旧版 qdc507_voice 声卡仍在内核中；为避免热切换语音驱动，请重启模块后再试")
		}
		for _, mod := range manifest.Modules {
			_, present, _ := adb.shellChecked("grep -q '^"+mod.Name+" ' /proc/modules", 8*time.Second)
			if present == 0 {
				continue
			}
			if err := voiceShell(adb, "insmod '"+voiceRemoteDir+"/"+mod.File+"'", 20*time.Second); err != nil {
				dmesg, _, _ := adb.shellChecked("dmesg | tail -n 80", 8*time.Second)
				detail := strings.TrimSpace(dmesg)
				if detail == "" {
					detail = err.Error()
				}
				return fmt.Errorf("模块音频驱动加载失败：%s", detail)
			}
			log.Printf("module voice: insmod %s", mod.File)
		}
	}
	ok, err := voiceWaitSoundDevices(adb, manifest)
	if err != nil {
		return err
	}
	if !ok {
		dmesg, _, _ := adb.shellChecked("dmesg | tail -n 80", 8*time.Second)
		return fmt.Errorf("音频驱动已加载，但 ALSA 设备没有出现：%s", strings.TrimSpace(dmesg))
	}
	if err := voiceEnsureCalibration(adb); err != nil {
		return err
	}
	_, status, err = adb.shellChecked("test -c /dev/ttyGS0 && test -p /run/voc_svr", 8*time.Second)
	if err != nil {
		return err
	}
	if status != 0 {
		return errors.New("模块缺少 ttyGS0 或 voc_svr，无法建立 USB 通话桥")
	}
	if err := voiceShell(adb, "'"+voiceRemoteDir+"/"+manifest.Helper+"' --check", 15*time.Second); err != nil {
		return fmt.Errorf("模块 PCM 桥自检失败: %w", err)
	}
	log.Printf("module voice: runtime prepared (%s)", manifest.RuntimeVersion)
	return nil
}

func (a *app) startModuleVoiceRouteLocked() error {
	if a.moduleVoiceSession == nil || a.moduleVoiceSession.adb == nil || a.moduleVoiceSession.manifest == nil {
		return errors.New("模块语音 ADB session 尚未准备")
	}
	adb := a.moduleVoiceSession.adb
	manifest := a.moduleVoiceSession.manifest

	// startRouteOnly(): launch the voice-route-session helper and wait until
	// the UAC route is RUNNING.
	ready, err := voiceRouteIsReady(adb, manifest)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	helperPath := voiceRemoteDir + "/" + manifest.Helper
	launch := "rm -f '" + voiceRoutePIDFile + "' '" + voiceRouteLogFile + "'; " +
		"nohup '" + helperPath + "' --voice-route-session --verbose " +
		"</dev/null >> '" + voiceRouteLogFile + "' 2>&1 & pid=$!; " +
		"starttime=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"case \"$pid:$starttime\" in :*|*:|*[!0-9:]*) false;; *) " +
		"printf '%s %s\\n' \"$pid\" \"$starttime\" > '" + voiceRoutePIDFile + "';; esac"
	launchErr := voiceShell(adb, launch, 8*time.Second)
	// audio_enable=1 can momentarily re-enumerate the USB gadget; reconnect
	// and verify instead of treating a lost reply as a failed start.
	if launchErr != nil {
		adb.Close()
		newADB, err := openDJIUSBADB()
		if err != nil {
			a.moduleVoiceSession.adb = nil
			return fmt.Errorf("路由启动后 ADB 重连失败: %w（启动错误: %v）", err, launchErr)
		}
		adb = newADB
		a.moduleVoiceSession.adb = newADB
	}
	for i := 0; i < 30; i++ {
		ready, rerr := voiceRouteIsReady(adb, manifest)
		if rerr != nil {
			return rerr
		}
		if ready {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	routeLog, _, _ := adb.shellChecked("test ! -f '"+voiceRouteLogFile+"' || tail -n 160 '"+voiceRouteLogFile+"'", 8*time.Second)
	detail := strings.TrimSpace(routeLog)
	if launchErr != nil {
		detail = launchErr.Error() + "\n" + detail
	}
	return fmt.Errorf("模块 D4/UAC 语音路由没有进入 RUNNING：%s", detail)
}

// stopModuleVoiceRouteInnerLocked runs with moduleVoiceOpMu held by the caller.
func (a *app) stopModuleVoiceRouteInnerLocked() error {
	if a.moduleVoiceSession == nil || a.moduleVoiceSession.adb == nil || a.moduleVoiceSession.manifest == nil {
		return errors.New("模块语音 ADB session 不可用")
	}
	manifest := a.moduleVoiceSession.manifest
	adb := a.moduleVoiceSession.adb
	helperPath := voiceRemoteDir + "/" + manifest.Helper
	stopCommand := "helper_stopped=1; " +
		"is_owned() { " +
		"current_start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"argv0=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p'); " +
		"args=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null); " +
		"test \"$current_start\" = \"$expected_start\" && " +
		"test \"$argv0\" = '" + helperPath + "' && " +
		"printf '%s\\n' \"$args\" | grep -q '^--voice-route-session$'; }; " +
		"if test -s '" + voiceRoutePIDFile + "'; then " +
		"read pid expected_start < '" + voiceRoutePIDFile + "' || true; " +
		"case \"$pid:$expected_start\" in :*|*:|*[!0-9:]*) true;; *) " +
		"if is_owned; then kill -TERM \"$pid\" 2>/dev/null || true; " +
		"n=0; while is_owned && test \"$n\" -lt 50; do " +
		"sleep 0.1; n=$((n+1)); done; is_owned && helper_stopped=0 || true; fi;; esac; fi; " +
		"test \"$helper_stopped\" -eq 1 && rm -f '" + voiceRoutePIDFile + "'"
	_, _, _ = adb.shellChecked(stopCommand, 8*time.Second)

	stopped := false
	for i := 0; i < 20; i++ {
		ok, serr := voiceRouteIsStopped(adb, manifest)
		if serr != nil {
			return serr
		}
		if ok {
			stopped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !stopped {
		return errors.New("D4 语音 helper 未确认正常退出；为保留 mixer 回滚，没有发送 SIGKILL")
	}
	var cleanupErr error
	for i := 0; i < 5; i++ {
		if err := voiceShell(adb,
			"echo 0 > /sys/class/android_usb/f_audio/audio_enable; "+
				"if test -p /run/voc_svr; then "+
				"printf 'T\\n' > /run/voc_svr; "+
				"printf 'T\\n' > /run/voc_svr; "+
				"printf 'B\\n' > /run/voc_svr; fi; "+
				"test \"$(cat /sys/class/android_usb/f_audio/audio_enable)\" = 0",
			8*time.Second); err == nil {
			return nil
		} else {
			cleanupErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cleanupErr != nil {
		return fmt.Errorf("T/T/B 路由回滚未确认: %w", cleanupErr)
	}
	return errors.New("D4 helper 已退出，但 T/T/B 路由回滚未确认")
}

func voiceShell(adb *adbClient, cmd string, timeout time.Duration) error {
	out, status, err := adb.shellChecked(cmd, timeout)
	if err != nil {
		return err
	}
	if status != 0 {
		msg := strings.TrimSpace(out)
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		if msg == "" {
			msg = fmt.Sprintf("返回状态 %d", status)
		}
		return fmt.Errorf("模块命令执行失败：%s", msg)
	}
	return nil
}

func voiceSoundDevicesReady(adb *adbClient, m *voiceRuntimeManifest) (bool, error) {
	cmd := voiceDeviceChecks(m)
	out, status, err := adb.shellChecked(cmd, 8*time.Second)
	if err != nil {
		return false, err
	}
	_ = out
	return status == 0, nil
}

func voiceWaitSoundDevices(adb *adbClient, m *voiceRuntimeManifest) (bool, error) {
	cmd := "ready=0; n=0; while test \"$n\" -lt 100; do " +
		"if " + voiceDeviceChecks(m) + "; then ready=1; break; fi; " +
		"sleep 0.2; n=$((n+1)); done; test \"$ready\" -eq 1"
	out, status, err := adb.shellChecked(cmd, 25*time.Second)
	if err != nil {
		return false, err
	}
	_ = out
	return status == 0, nil
}

func voiceDeviceChecks(m *voiceRuntimeManifest) string {
	parts := make([]string, 0, len(m.RequiredDevices)+1)
	for _, dev := range m.RequiredDevices {
		parts = append(parts, "test -c '"+dev+"'")
	}
	parts = append(parts, "grep -Fq '"+m.CardName+"' /proc/asound/cards")
	return strings.Join(parts, " && ")
}

func voiceEnsureCalibration(adb *adbClient) error {
	command := "owned=0; " +
		"if test -s '" + voiceCalibrationPID + "'; then " +
		"read pid expected_start < '" + voiceCalibrationPID + "' || true; " +
		"current_start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"argv0=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p'); " +
		"test \"$current_start\" = \"$expected_start\" && " +
		"test \"$argv0\" = /usr/bin/alsaucm_test && owned=1 || true; fi; " +
		"if test \"$owned\" -eq 0; then " +
		"for proc in /proc/[0-9]*; do " +
		"test -r \"$proc/cmdline\" || continue; " +
		"argv0=$(tr '\\000' '\\n' < \"$proc/cmdline\" 2>/dev/null | sed -n '1p'); " +
		"test \"$argv0\" = /usr/bin/alsaucm_test || continue; " +
		"oldpid=${proc##*/}; kill -TERM \"$oldpid\" 2>/dev/null || true; " +
		"n=0; while kill -0 \"$oldpid\" 2>/dev/null && test \"$n\" -lt 30; do " +
		"sleep 0.1; n=$((n+1)); done; " +
		"kill -0 \"$oldpid\" 2>/dev/null && exit 71 || true; done; " +
		"rm -f /run/alsaucm_test '" + voiceCalibrationPID + "' '" + voiceCalibrationLog + "'; " +
		"nohup /usr/bin/alsaucm_test </dev/null >> '" + voiceCalibrationLog + "' 2>&1 & pid=$!; " +
		"starttime=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"printf '%s %s\\n' \"$pid\" \"$starttime\" > '" + voiceCalibrationPID + "'; " +
		"n=0; while test \"$n\" -lt 50 && test ! -p /run/alsaucm_test; do " +
		"kill -0 \"$pid\" 2>/dev/null || exit 72; sleep 0.1; n=$((n+1)); done; " +
		"test -p /run/alsaucm_test || exit 73; fi; " +
		"if ! grep -q 'ACDB -> Sent VocProc Cal!' '" + voiceCalibrationLog + "' 2>/dev/null; then " +
		"printf 'open snd_soc_msm_9x07_Tomtom_I2S\\n' > /run/alsaucm_test; " +
		"printf 'set _verb VoLTE\\n' > /run/alsaucm_test; " +
		"printf 'set _enadev Auxpcm Rx\\n' > /run/alsaucm_test; " +
		"printf 'set _enadev Auxpcm Tx\\n' > /run/alsaucm_test; " +
		"n=0; while test \"$n\" -lt 100; do " +
		"grep -q 'ACDB -> Sent VocProc Cal!' '" + voiceCalibrationLog + "' 2>/dev/null && break; " +
		"sleep 0.1; n=$((n+1)); done; fi; " +
		"grep -q 'ACDB -> Sent VocProc Cal!' '" + voiceCalibrationLog + "'"
	out, status, err := adb.shellChecked(command, 25*time.Second)
	if err != nil {
		return err
	}
	if status != 0 {
		detail, _, _ := adb.shellChecked("test ! -f '"+voiceCalibrationLog+"' || tail -n 100 '"+voiceCalibrationLog+"'", 8*time.Second)
		msg := strings.TrimSpace(detail)
		if msg == "" {
			msg = strings.TrimSpace(out)
		}
		return fmt.Errorf("模块 VoLTE ACDB 校准服务没有就绪：%s", msg)
	}
	return nil
}

func voiceRouteIsReady(adb *adbClient, m *voiceRuntimeManifest) (bool, error) {
	helperPath := voiceRemoteDir + "/" + m.Helper
	command := "test -s '" + voiceRoutePIDFile + "' && " +
		"read pid expected_start < '" + voiceRoutePIDFile + "' && " +
		"test \"$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null)\" = \"$expected_start\" && " +
		"test \"$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p')\" = '" + helperPath + "' && " +
		"tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | grep -q '^--voice-route-session$' && " +
		"grep -q 'VoLTE route session active on hw:0,4' '" + voiceRouteLogFile + "' && " +
		"test \"$(cat /sys/class/android_usb/f_audio/audio_enable)\" = 1 && " +
		"grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status && " +
		"grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status"
	out, status, err := adb.shellChecked(command, 8*time.Second)
	if err != nil {
		return false, err
	}
	_ = out
	return status == 0, nil
}

func voiceRouteIsStopped(adb *adbClient, m *voiceRuntimeManifest) (bool, error) {
	helperPath := voiceRemoteDir + "/" + m.Helper
	command := "owned=0; if test -s '" + voiceRoutePIDFile + "'; then " +
		"read pid expected_start < '" + voiceRoutePIDFile + "' || true; " +
		"current_start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"argv0=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p'); " +
		"args=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null); " +
		"if test \"$current_start\" = \"$expected_start\" && " +
		"test \"$argv0\" = '" + helperPath + "' && " +
		"printf '%s\\n' \"$args\" | grep -q '^--voice-route-session$'; " +
		"then owned=1; else rm -f '" + voiceRoutePIDFile + "'; fi; fi; " +
		"test \"$owned\" -eq 0"
	out, status, err := adb.shellChecked(command, 8*time.Second)
	if err != nil {
		return false, err
	}
	_ = out
	return status == 0, nil
}

func (a *app) voiceProvisionAPI(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if !request.Confirm {
		writeError(w, http.StatusBadRequest, "需要确认后才会从上游获取模块侧语音运行时")
		return
	}
	if err := provisionUpstreamVoiceRuntime(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

// ---- HTTP API ----

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStartAPI(w http.ResponseWriter, _ *http.Request) {
	err := a.ensureModuleVoiceRoute()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"started": true, "status": a.voiceStatus()})
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	a.stopModuleVoiceRoute()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "status": a.voiceStatus()})
}
