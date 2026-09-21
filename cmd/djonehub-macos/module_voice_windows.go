//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// On Windows the module-side voice route is Quectel's "Voice over USB": the
// module decodes the call and streams raw 8 kHz PCM on its NMEA interface,
// which audio_windows.go bridges to this machine's microphone and speakers.
// Unlike the macOS path there is nothing to install on the module — no ADB, no
// kernel modules — so preparing the route is two AT commands.
//
// Not every module can do it. A firmware without the audio subsystem answers
// the test form (AT+QPCMV=?) with a full parameter list but rejects every set
// form with a bare ERROR, which is why the error text below points at the
// firmware rather than at the wiring.

const voiceRouteRetryWindow = 30 * time.Second

// gpsOutportSaved remembers the GNSS output port so stopping the voice route
// can hand the NMEA interface back to GNSS rather than leaving it disabled.
var gpsOutportSaved struct {
	sync.Mutex
	value string
}

func (a *app) kickModuleVoice() {
	log.Printf("module voice: Windows Voice over USB is armed when a call becomes active")
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

func atSaidError(response string) bool {
	return strings.Contains(strings.ToUpper(response), "ERROR")
}

// releaseNMEAForPCM stops GNSS from writing to the interface Voice over USB
// streams PCM on. Without this the two interleave and the audio is unusable.
func (a *app) releaseNMEAForPCM() {
	current, err := a.runATCommand(`AT+QGPSCFG="outport"`, 5*time.Second)
	if err == nil && !atSaidError(current) {
		if idx := strings.LastIndex(current, ","); idx >= 0 {
			value := strings.TrimSpace(current[idx+1:])
			value = strings.Trim(strings.SplitN(value, "\n", 2)[0], "\r\" ")
			if value != "" && !strings.EqualFold(value, "none") {
				gpsOutportSaved.Lock()
				gpsOutportSaved.value = value
				gpsOutportSaved.Unlock()
			}
		}
	}
	if _, err := a.runATCommand(`AT+QGPSCFG="outport","none"`, 5*time.Second); err != nil {
		log.Printf("module voice: could not free the NMEA port from GNSS: %v", err)
	}
}

func (a *app) restoreNMEAForGNSS() {
	gpsOutportSaved.Lock()
	value := gpsOutportSaved.value
	gpsOutportSaved.Unlock()
	if value == "" {
		return
	}
	if _, err := a.runATCommand(fmt.Sprintf(`AT+QGPSCFG="outport","%s"`, value), 5*time.Second); err != nil {
		log.Printf("module voice: could not restore GNSS output port: %v", err)
	}
}

func (a *app) ensureModuleVoiceRoute() error {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()

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

	a.releaseNMEAForPCM()

	// The module rejects a switch between Voice over USB and UAC while either
	// is already enabled, so any previous mode has to be torn down first —
	// AT+QPCMV=1,0 answers ERROR if AT+QPCMV? still reports 1,2. Anything that
	// left the module in UAC mode, including a previous run, would otherwise
	// make the route permanently unavailable.
	if _, err := a.runATCommand("AT+QPCMV=0", 5*time.Second); err != nil {
		log.Printf("module voice: pre-emptive AT+QPCMV=0 reported %v (continuing)", err)
	}

	response, err := a.runATCommand("AT+QPCMV=1,0", 6*time.Second)
	if err != nil {
		a.setVoiceStatus(false, err, "")
		return err
	}
	if atSaidError(response) {
		failure := errors.New("模块拒绝 AT+QPCMV=1,0：该固件没有实现音频子系统（测试形式可用但设置形式被拒）")
		a.setVoiceStatus(false, failure, strings.TrimSpace(response))
		return failure
	}

	// The state is deliberately not read back. internal/modem treats "+QPCMV"
	// as a URC prefix — it is the flow-control notification Voice over USB
	// raises (0 = module busy, 1 = ready, see Manager.GetQPCMVChan) — so the
	// reply to AT+QPCMV? is consumed as a URC and the command returns empty.
	// AT+QPCMV=1,0 answering OK is the authoritative signal.
	detail := "Voice over USB enabled (AT+QPCMV=1,0)"
	a.setVoiceStatus(true, nil, detail)
	log.Printf("module voice: %s", detail)
	return nil
}

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

// stopModuleVoiceRoute disables Voice over USB after a call and hands the NMEA
// interface back to GNSS.
func (a *app) stopModuleVoiceRoute() {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()

	a.moduleVoiceMu.Lock()
	wasReady := a.moduleVoiceReady
	a.moduleVoiceReady = false
	a.moduleVoiceLast = time.Time{}
	a.moduleVoiceMu.Unlock()
	if !wasReady {
		return
	}

	if _, err := a.runATCommand("AT+QPCMV=0", 6*time.Second); err != nil {
		log.Printf("module voice: stop failed: %v", err)
	}
	a.restoreNMEAForGNSS()
	log.Printf("module voice: route stopped")
}

func (a *app) voiceStatus() map[string]any {
	a.moduleVoiceMu.Lock()
	status := map[string]any{
		"ready":             a.moduleVoiceReady,
		"last_attempt":      a.moduleVoiceLast,
		"last_error":        a.moduleVoiceErr,
		"detail":            a.moduleVoiceDetail,
		"transport":         "voice-over-usb",
		"runtime_included":  false,
		"runtime_installed": false,
		"runtime_detail":    "Windows 走 Quectel Voice over USB，不需要模块侧 ADB 运行时",
	}
	a.moduleVoiceMu.Unlock()

	if port, err := modulePCMPort(); err == nil {
		status["pcm_port"] = port
	} else {
		status["pcm_port_error"] = err.Error()
	}
	return status
}

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

// voiceProvisionAPI reports whether this module can carry call audio at all.
// Nothing is installed on Windows, so "provisioning" is a capability probe.
func (a *app) voiceProvisionAPI(w http.ResponseWriter, _ *http.Request) {
	response, err := a.runATCommand("AT+QPCMV=?", 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if atSaidError(response) {
		writeError(w, http.StatusNotImplemented, "模块不支持 AT+QPCMV，无法通过 USB 传输通话音频")
		return
	}
	if err := a.ensureModuleVoiceRoute(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.stopModuleVoiceRoute()
	writeJSON(w, http.StatusOK, map[string]any{
		"supported": true,
		"transport": "voice-over-usb",
		"detail":    "AT+QPCMV=1,0 可用；通话开始时会自动启用",
	})
}

func (a *app) voiceStartAPI(w http.ResponseWriter, _ *http.Request) {
	if err := a.ensureModuleVoiceRoute(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	a.stopModuleVoiceRoute()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}
