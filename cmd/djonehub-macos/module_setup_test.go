package main

import (
	"path/filepath"
	"testing"
)

func TestParseUSBCompositionAndClassification(t *testing.T) {
	factory, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,0`)
	if err != nil || !factory.isFactoryDJI() || factory.hasUAC() {
		t.Fatalf("factory parse = %#v, %v", factory, err)
	}
	uac, err := parseUSBComposition("AT+QCFG=\"USBCFG\"\r\n+QCFG: \"usbcfg\",0x2C7C,0x125,1,1,1,1,1,1,1\r\nOK")
	if err != nil || !uac.hasUAC() || !uac.hasADB() || !uac.isUACTarget() {
		t.Fatalf("UAC parse = %#v, %v", uac, err)
	}
	djiUAC, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,1,1`)
	if err != nil || !djiUAC.hasUAC() || !djiUAC.hasADB() || !djiUAC.isUACTarget() {
		t.Fatalf("DJI full UAC parse = %#v, %v", djiUAC, err)
	}
	legacyUAC, err := parseUSBComposition(`+QCFG: "usbcfg",0x2C7C,0x125,1,1,1,1,1,0,1`)
	if err != nil || !legacyUAC.hasUAC() || legacyUAC.isCallAudioCapable() || legacyUAC.hasADB() {
		t.Fatalf("legacy UAC parse = %#v, %v", legacyUAC, err)
	}
	if got := factory.command(); got != `AT+QCFG="USBCFG",0x2CA3,0x4006,1,1,1,1,1,0,0` {
		t.Fatalf("command = %q", got)
	}
	modified, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,0,1,0,1,0,1`)
	if err != nil || !modified.isRecoverable() {
		t.Fatalf("complete third-party configuration must remain recoverable: %#v, %v", modified, err)
	}
	broken, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,2,0,1,0,1`)
	if err != nil || broken.isRecoverable() {
		t.Fatalf("non-binary configuration must remain blocked: %#v, %v", broken, err)
	}
}

func TestModuleSetupRollbackIsNotTransient(t *testing.T) {
	for _, state := range []string{"initializing", "restarting", "verifying"} {
		if !moduleSetupIsTransient(state) {
			t.Fatalf("%q should be transient", state)
		}
	}
	for _, state := range []string{"rolled_back", "failed", "ready", "needs_initialization"} {
		if moduleSetupIsTransient(state) {
			t.Fatalf("%q must return to inspection instead of remaining in progress", state)
		}
	}
}

func TestModuleSetupTerminalStatesAreServedFromCache(t *testing.T) {
	for _, state := range []string{"ready", "failed", "rolled_back"} {
		if !moduleSetupIsCachedTerminal(state) {
			t.Fatalf("%q should be served without a synchronous USB query", state)
		}
	}
	if moduleSetupIsCachedTerminal("needs_initialization") {
		t.Fatal("an uninitialized module must still be inspected on first use")
	}
}

func TestUSBCFGErrorIsTransientDuringReenumeration(t *testing.T) {
	if !atResponseIsError("AT+QCFG=\"USBCFG\"\r\nERROR") {
		t.Fatal("USBCFG ERROR must be detected as a transient AT response")
	}
	if atResponseIsError(`+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1\r\nOK`) {
		t.Fatal("a valid USBCFG response must not be classified as an error")
	}
}

func TestReadyModuleSetupInvalidatesAfterUSBReenumeration(t *testing.T) {
	a := &app{}
	a.setModuleSetup(moduleSetupStatus{State: "ready", Summary: "ready"})
	a.invalidateReadyModuleSetup()
	a.moduleSetupMu.RLock()
	state := a.moduleSetup.State
	a.moduleSetupMu.RUnlock()
	if state != "" {
		t.Fatalf("ready state after re-enumeration = %q, want cleared", state)
	}

	a.setModuleSetup(moduleSetupStatus{State: "restarting", Summary: "restart"})
	a.invalidateReadyModuleSetup()
	a.moduleSetupMu.RLock()
	state = a.moduleSetup.State
	a.moduleSetupMu.RUnlock()
	if state != "restarting" {
		t.Fatalf("in-progress state after re-enumeration = %q, want restarting", state)
	}
}

func TestParseIMSConfiguration(t *testing.T) {
	configuration, capability, err := parseIMSConfiguration("AT+QCFG=\"ims\"\r\n+QCFG: \"ims\",1,1\r\nOK")
	if err != nil || configuration != 1 || capability != 1 {
		t.Fatalf("IMS parse = %d,%d err=%v", configuration, capability, err)
	}
	configuration, capability, err = parseIMSConfiguration(`+QCFG: "ims",2,0`)
	if err != nil || configuration != 2 || capability != 0 {
		t.Fatalf("disabled IMS parse = %d,%d err=%v", configuration, capability, err)
	}
}

func TestUSBProfileIntentDefaultsToNoAutomaticRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usb-profile-intent.json")
	a := &app{usbProfileIntentPath: path}
	if err := a.loadUSBProfileIntentLocked(); err != nil {
		t.Fatalf("load blank intent: %v", err)
	}
	if a.usbProfileMobileArmed {
		t.Fatal("a fresh installation must not automatically change a non-UAC module")
	}
	a.usbProfileMobileArmed = true
	if err := a.persistUSBProfileIntentLocked(); err != nil {
		t.Fatalf("persist mobile intent: %v", err)
	}
	reloaded := &app{usbProfileIntentPath: path}
	if err := reloaded.loadUSBProfileIntentLocked(); err != nil {
		t.Fatalf("reload intent: %v", err)
	}
	if !reloaded.usbProfileMobileArmed {
		t.Fatal("an explicit iPhone/iPad selection must survive the reconnect")
	}
}
