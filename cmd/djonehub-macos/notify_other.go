//go:build !linux

package main

// startDesktopNotifier is Linux-only: macOS uses the native App and Windows
// the browser console for call and SMS alerts.
func (a *app) startDesktopNotifier(string) {}
