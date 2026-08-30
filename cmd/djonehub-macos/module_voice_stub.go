//go:build !darwin || !cgo

package main

import (
	"errors"
	"net/http"
	"time"
)

type moduleVoiceSession struct{}

// Module-side voice route control is macOS-only for now.

func (a *app) kickModuleVoice() {}

func (a *app) prepareModuleVoiceSessionBudgeted(_ time.Duration) error {
	return errors.New("模块语音路由仅在 macOS 版本可用")
}

func (a *app) resetModuleVoiceSession() {}

func (a *app) ensureModuleVoiceRoute() error {
	return errors.New("模块语音路由仅在 macOS 版本可用")
}

func (a *app) ensureModuleVoiceRouteBudgeted(_ time.Duration) error {
	return errors.New("模块语音路由仅在 macOS 版本可用")
}

func (a *app) stopModuleVoiceRoute() {}

func (a *app) voiceStatus() map[string]any {
	return map[string]any{"ready": false, "detail": "macOS only"}
}

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceProvisionAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "模块语音运行时仅在 macOS 版本可用")
}

func (a *app) voiceStartAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusBadGateway, "模块语音路由仅在 macOS 版本可用")
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}
