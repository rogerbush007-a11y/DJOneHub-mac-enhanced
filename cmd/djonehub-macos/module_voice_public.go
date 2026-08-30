//go:build darwin && cgo && public_runtime_stub

package main

import (
	"errors"
	"net/http"
	"time"
)

type moduleVoiceSession struct{}

// This public-source adapter intentionally does not contain module-side voice
// binaries. It keeps call control and diagnostics buildable without claiming
// that a Mac can route call audio.
func (a *app) kickModuleVoice() {}

func (a *app) prepareModuleVoiceSessionBudgeted(_ time.Duration) error {
	return errors.New("公开源码版未包含模块侧语音运行时")
}

func (a *app) resetModuleVoiceSession() {}

func (a *app) ensureModuleVoiceRoute() error {
	return errors.New("公开源码版未包含模块侧语音运行时")
}

func (a *app) ensureModuleVoiceRouteBudgeted(_ time.Duration) error {
	return errors.New("公开源码版未包含模块侧语音运行时")
}

func (a *app) stopModuleVoiceRoute() {}

func (a *app) callAudioAvailable() bool { return false }

func (a *app) voiceStatus() map[string]any {
	return map[string]any{
		"ready":            false,
		"runtime_included": false,
		"detail":           "公开源码版未包含模块侧语音运行时",
	}
}

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStartAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "公开源码版未包含模块侧语音运行时")
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "runtime_included": false})
}
