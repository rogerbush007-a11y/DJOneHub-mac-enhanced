//go:build !darwin || !cgo

package main

import "net/http"

func enableModuleNetworkWake() error    { return nil }
func disableModuleNetworkWake() error   { return nil }
func uninstallModuleNetworkWake() error { return nil }
func (a *app) uninstallModuleNetworkWakeAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}
