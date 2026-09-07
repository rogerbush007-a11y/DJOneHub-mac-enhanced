package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// moduleSetupStatus is intentionally a small state machine. It separates a
// harmless inspection from the one explicit action which may write the
// module's USB composition and cause a re-enumeration.
type moduleSetupStatus struct {
	State                string `json:"state"`
	Summary              string `json:"summary"`
	Detail               string `json:"detail,omitempty"`
	CanInitialize        bool   `json:"can_initialize"`
	RequiresConfirmation bool   `json:"requires_confirmation"`
	BackupPath           string `json:"backup_path,omitempty"`
	UpdatedAt            string `json:"updated_at"`
}

type usbComposition struct {
	VendorID  int
	ProductID int
	Flags     []int
}

func (c usbComposition) command() string {
	parts := []string{fmt.Sprintf("0x%04X", c.VendorID), fmt.Sprintf("0x%04X", c.ProductID)}
	for _, flag := range c.Flags {
		parts = append(parts, strconv.Itoa(flag))
	}
	return `AT+QCFG="USBCFG",` + strings.Join(parts, ",")
}

func (c usbComposition) hasUAC() bool {
	return len(c.Flags) >= 1 && c.Flags[len(c.Flags)-1] == 1
}

func (c usbComposition) hasADB() bool {
	return len(c.Flags) >= 2 && c.Flags[len(c.Flags)-2] == 1
}

func (c usbComposition) isUACTarget() bool {
	// Both identities are observed valid complete-audio compositions. A module
	// may retain DJI's original VID/PID while exposing the full UAC + ADB flag
	// set; it must be treated as ready-capable rather than offered a destructive
	// initialization flow merely to rewrite its identity.
	if len(c.Flags) != 7 || strings.Join(intSliceStrings(c.Flags), ",") != "1,1,1,1,1,1,1" {
		return false
	}
	return (c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID) ||
		(c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID)
}

// UAC alone is insufficient: the module-side voice route also needs ADB.
// Locked firmware may acknowledge USBCFG but leave the ADB bit disabled.
func (c usbComposition) isCallAudioCapable() bool {
	return c.isUACTarget()
}

func (c usbComposition) isFactoryDJI() bool {
	return c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID &&
		len(c.Flags) == 7 && strings.Join(intSliceStrings(c.Flags), ",") == "1,1,1,1,1,0,0"
}

// isRecoverable accepts any complete, binary USB composition returned by the
// supported module. It intentionally does not require a known VID/PID or flag
// pattern: another tool may have changed those values, and the explicit setup
// flow can safely back them up and restore them if validation fails.
func (c usbComposition) isRecoverable() bool {
	if c.VendorID < 0 || c.VendorID > 0xFFFF || c.ProductID < 0 || c.ProductID > 0xFFFF || len(c.Flags) != 7 {
		return false
	}
	for _, flag := range c.Flags {
		if flag != 0 && flag != 1 {
			return false
		}
	}
	return true
}

func intSliceStrings(values []int) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strconv.Itoa(value))
	}
	return result
}

func parseUSBComposition(response string) (usbComposition, error) {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), `+qcfg: "usbcfg",`) {
			continue
		}
		comma := strings.Index(line, ",")
		if comma < 0 {
			break
		}
		fields := strings.Split(line[comma+1:], ",")
		if len(fields) < 3 {
			break
		}
		parse := func(raw string) (int, error) {
			value, err := strconv.ParseInt(strings.TrimSpace(raw), 0, 32)
			return int(value), err
		}
		vendorID, err := parse(fields[0])
		if err != nil {
			return usbComposition{}, fmt.Errorf("parse USB vendor: %w", err)
		}
		productID, err := parse(fields[1])
		if err != nil {
			return usbComposition{}, fmt.Errorf("parse USB product: %w", err)
		}
		flags := make([]int, 0, len(fields)-2)
		for _, field := range fields[2:] {
			value, err := parse(field)
			if err != nil {
				return usbComposition{}, fmt.Errorf("parse USB flag: %w", err)
			}
			flags = append(flags, value)
		}
		return usbComposition{VendorID: vendorID, ProductID: productID, Flags: flags}, nil
	}
	return usbComposition{}, errors.New("模块没有返回可识别的 USBCFG")
}

func (a *app) inspectModuleSetup() moduleSetupStatus {
	if a.demo {
		return moduleSetupStatus{State: "ready", Summary: "演示模块已完成初始化", UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	response, err := a.runATCommand(`AT+QCFG="USBCFG"`, 4*time.Second)
	if err != nil {
		return moduleSetupStatus{State: "disconnected", Summary: "等待 4G 模块连接", Detail: err.Error(), UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	composition, err := parseUSBComposition(response)
	if err != nil {
		// A USB-profile write is followed by a modem reboot and a period where
		// the AT channel is already visible but QCFG has not become readable.
		// ERROR at this point is transient; presenting it as an unsupported
		// composition wrongly blocks the user from simply waiting for the
		// expected re-enumeration to finish.
		if atResponseIsError(response) {
			return moduleSetupStatus{State: "reconnecting", Summary: "正在重新连接并读取 USB 配置", Detail: "模块正处于 USB 模式切换阶段，请稍候自动重试", UpdatedAt: time.Now().Format(time.RFC3339)}
		}
		return moduleSetupStatus{State: "unsupported", Summary: "无法识别模块 USB 配置", Detail: err.Error(), UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	if composition.isCallAudioCapable() {
		imsResponse, imsErr := a.runATCommand(`AT+QCFG="ims"`, 3*time.Second)
		imsConfig, volteCapability, parseErr := parseIMSConfiguration(imsResponse)
		if imsErr == nil && parseErr == nil && imsConfig == 1 && volteCapability == 1 {
			return moduleSetupStatus{State: "ready", Summary: "模块已具备通话音频与 VoLTE 能力", Detail: composition.command(), UpdatedAt: time.Now().Format(time.RFC3339)}
		}
		return moduleSetupStatus{State: "needs_initialization", Summary: "模块音频已就绪，需要启用 VoLTE", Detail: composition.command(), CanInitialize: true, RequiresConfirmation: true, UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	if composition.isFactoryDJI() {
		return moduleSetupStatus{State: "needs_initialization", Summary: "发现原始 USB 配置，可启用通话支持", Detail: composition.command(), CanInitialize: true, RequiresConfirmation: true, UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	if composition.isRecoverable() {
		return moduleSetupStatus{State: "needs_initialization", Summary: "发现可恢复 USB 配置，可重新启用通话支持", Detail: composition.command(), CanInitialize: true, RequiresConfirmation: true, UpdatedAt: time.Now().Format(time.RFC3339)}
	}
	return moduleSetupStatus{State: "unsupported", Summary: "模块 USB 配置不是可安全初始化的原始状态", Detail: composition.command(), UpdatedAt: time.Now().Format(time.RFC3339)}
}

func (a *app) moduleSetupStatusAPI(w http.ResponseWriter, _ *http.Request) {
	a.moduleSetupMu.RLock()
	current := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	// Setup is an adoption state machine, not a live modem health probe. Once
	// it has reached a terminal state in this process, return that result
	// immediately. Re-running synchronous USB AT inspection here makes the App
	// appear to time out during the normal USB re-enumeration window.
	if moduleSetupIsTransient(current.State) || moduleSetupIsCachedTerminal(current.State) {
		writeJSON(w, http.StatusOK, current)
		return
	}
	status := a.inspectModuleSetup()
	if current.State == "rolled_back" && status.CanInitialize {
		status.Summary = "上次启用未验证，已恢复原始配置；可手动重试"
		status.Detail = current.Detail
		status.BackupPath = current.BackupPath
	}
	writeJSON(w, http.StatusOK, status)
}

func moduleSetupIsTransient(state string) bool {
	return state == "initializing" || state == "restarting" || state == "verifying"
}

func moduleSetupIsCachedTerminal(state string) bool {
	return state == "ready" || state == "failed" || state == "rolled_back"
}

func (a *app) setModuleSetup(status moduleSetupStatus) {
	status.UpdatedAt = time.Now().Format(time.RFC3339)
	a.moduleSetupMu.Lock()
	a.moduleSetup = status
	a.moduleSetupMu.Unlock()
}

// invalidateReadyModuleSetup makes a later status request inspect USB again
// after a physical disconnect/re-enumeration. It deliberately leaves an
// in-progress setup untouched: the worker owns that state through reboot and
// validation.
func (a *app) invalidateReadyModuleSetup() {
	a.moduleSetupMu.Lock()
	if a.moduleSetup.State == "ready" {
		a.moduleSetup = moduleSetupStatus{}
	}
	a.moduleSetupMu.Unlock()
}

func (a *app) moduleSetupStartAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "需要确认后才会修改已识别配置并启用通话支持")
		return
	}
	a.moduleSetupMu.RLock()
	running := a.moduleSetup.State == "initializing" || a.moduleSetup.State == "restarting" || a.moduleSetup.State == "verifying"
	a.moduleSetupMu.RUnlock()
	if running {
		writeError(w, http.StatusConflict, "模块通话支持正在启用")
		return
	}
	inspection := a.inspectModuleSetup()
	if !inspection.CanInitialize {
		writeError(w, http.StatusConflict, inspection.Summary)
		return
	}
	a.setModuleSetup(moduleSetupStatus{State: "initializing", Summary: "正在备份并启用通话支持", Detail: inspection.Detail})
	go a.runModuleSetup()
	a.moduleSetupMu.RLock()
	status := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	writeJSON(w, http.StatusAccepted, status)
}

func (a *app) runModuleSetup() {
	response, err := a.runATCommand(`AT+QCFG="USBCFG"`, 5*time.Second)
	if err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法读取模块 USB 配置", Detail: err.Error()})
		return
	}
	original, err := parseUSBComposition(response)
	if err != nil || !original.isRecoverable() {
		detail := "模块 USB 配置格式不完整，停止启用"
		if err != nil {
			detail = err.Error()
		}
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "启用前校验未通过", Detail: detail})
		return
	}
	backupPath, err := saveModuleSetupBackup(original)
	if err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法保存模块回滚备份", Detail: err.Error()})
		return
	}
	if err := prepareModuleCallUSB(original, a.runModuleSetupAT); err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "通话接口准备未完成", Detail: err.Error(), BackupPath: backupPath})
		return
	}
	volteResponse, err := a.runATCommand(`AT+QCFG="volte_disable"`, 5*time.Second)
	if err != nil || !strings.Contains(strings.ReplaceAll(strings.ToLower(volteResponse), "_", "/"), `"volte/disable",0`) {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "无法确认 VoLTE 已启用", Detail: firstNonEmpty(errString(err), volteResponse), BackupPath: backupPath})
		return
	}
	imsWrite, err := a.runATCommand(`AT+QCFG="ims",1`, 5*time.Second)
	if err != nil || atResponseIsError(imsWrite) {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "模块拒绝启用 IMS", Detail: firstNonEmpty(errString(err), imsWrite), BackupPath: backupPath})
		return
	}
	imsReadBack, err := a.runATCommand(`AT+QCFG="ims"`, 5*time.Second)
	imsConfig, _, imsParseErr := parseIMSConfiguration(imsReadBack)
	if err != nil || imsParseErr != nil || imsConfig != 1 {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "IMS 写入回读未确认，未重启模块", Detail: firstNonEmpty(errString(err), errString(imsParseErr), imsReadBack), BackupPath: backupPath})
		return
	}
	a.setModuleSetup(moduleSetupStatus{State: "restarting", Summary: "模块正在重启并重新识别", BackupPath: backupPath})
	if _, err := a.runATCommand("AT+CFUN=1,1", 4*time.Second); err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "模块重启命令失败", Detail: err.Error(), BackupPath: backupPath})
		return
	}
	a.markUSBATDetached("first-use module setup reboot")
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if err := a.ensureUSBAT(); err != nil {
			continue
		}
		imsResponse, imsErr := a.runATCommand(`AT+QCFG="ims"`, 4*time.Second)
		imsConfig, volteCapability, imsParseErr := parseIMSConfiguration(imsResponse)
		if imsErr != nil || imsParseErr != nil || imsConfig != 1 || volteCapability != 1 {
			continue
		}
		a.setModuleSetup(moduleSetupStatus{State: "verifying", Summary: "正在验证 4G、短信与通话路由", BackupPath: backupPath})
		if err := a.ensureModuleVoiceRouteBudgeted(35 * time.Second); err == nil {
			a.stopModuleVoiceRoute()
			a.setModuleSetup(moduleSetupStatus{State: "ready", Summary: "通话支持已启用，可直接使用", BackupPath: backupPath})
			return
		} else {
			a.rollbackModuleSetup(original, backupPath, "通话路由验证失败："+err.Error())
			return
		}
	}
	a.rollbackModuleSetup(original, backupPath, "通话路由未在 90 秒内验证")
}

func parseIMSConfiguration(response string) (configuration int, volteCapability int, err error) {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), `+qcfg: "ims",`) {
			continue
		}
		comma := strings.Index(line, ",")
		if comma < 0 {
			break
		}
		fields := strings.Split(line[comma+1:], ",")
		if len(fields) < 2 {
			break
		}
		configuration, err = strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return 0, 0, fmt.Errorf("parse IMS configuration: %w", err)
		}
		volteCapability, err = strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("parse VoLTE capability: %w", err)
		}
		return configuration, volteCapability, nil
	}
	return 0, 0, errors.New("模块没有返回可识别的 IMS 状态")
}

func (a *app) rollbackModuleSetup(original usbComposition, backupPath, reason string) {
	a.setModuleSetup(moduleSetupStatus{State: "verifying", Summary: "初始化未完成，正在恢复原始模块配置", Detail: reason, BackupPath: backupPath})
	response, err := a.runATCommand(original.command(), 8*time.Second)
	if err != nil || atResponseIsError(response) {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "初始化未完成，自动回滚失败", Detail: firstNonEmpty(errString(err), response), BackupPath: backupPath})
		return
	}
	readBack, err := a.runATCommand(`AT+QCFG="USBCFG"`, 5*time.Second)
	actual, parseErr := parseUSBComposition(readBack)
	if err != nil || parseErr != nil || actual.command() != original.command() {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "初始化未完成，回滚回读失败", Detail: firstNonEmpty(errString(err), errString(parseErr), readBack), BackupPath: backupPath})
		return
	}
	if _, err := a.runATCommand("AT+CFUN=1,1", 4*time.Second); err != nil {
		a.setModuleSetup(moduleSetupStatus{State: "failed", Summary: "原始配置已写回，但模块重启失败", Detail: err.Error(), BackupPath: backupPath})
		return
	}
	a.markUSBATDetached("module setup automatic rollback reboot")
	a.setModuleSetup(moduleSetupStatus{State: "rolled_back", Summary: "通话支持未验证，已恢复原始模块配置", Detail: reason, BackupPath: backupPath})
}

func saveModuleSetupBackup(composition usbComposition) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "Application Support", "DJOneHub", "module-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	payload := struct {
		SavedAt string         `json:"saved_at"`
		USB     usbComposition `json:"usb"`
	}{SavedAt: time.Now().Format(time.RFC3339), USB: composition}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "usb-before-v1.2.1-"+time.Now().Format("20060102-150405")+".json")
	return path, os.WriteFile(path, data, 0o600)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "未知错误"
}
