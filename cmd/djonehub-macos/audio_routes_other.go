//go:build !windows && !linux

package main

import "net/http"

// registerPlatformAudioRoutes has no extra audio routes outside Windows, where
// the uplink is always this machine's microphone.
func (a *app) registerPlatformAudioRoutes(_ *http.ServeMux) {}
