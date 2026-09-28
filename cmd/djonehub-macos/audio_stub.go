//go:build (!darwin || !cgo) && !windows && !linux

package main

import "fmt"

// audioRouter is a no-op on platforms without CoreAudio (e.g. the Windows
// experimental build). Voice routing is macOS-only for now.
type audioRouter struct{}

func newAudioRouter() *audioRouter { return &audioRouter{} }

func (r *audioRouter) start() error {
	return fmt.Errorf("通话音频仅在 macOS 版本可用")
}

func (r *audioRouter) stop() {}

func (r *audioRouter) setMuted(bool) {}

func (r *audioRouter) isRunning() bool { return false }

func (r *audioRouter) state() (bool, float64, float64, string) {
	return false, 0, 0, "通话音频仅在 macOS 版本可用"
}

func (r *audioRouter) audioStats() map[string]int64 { return map[string]int64{} }

func (r *audioRouter) audioDevices() map[string]string { return map[string]string{} }

func (r *audioRouter) live() (float64, float64, float64, float64) { return 0, 0, 0, 0 }

func (r *audioRouter) formats() string { return "" }

func (r *audioRouter) formatChanges() string { return "" }

func (r *audioRouter) startRecording() (string, error) {
	return "", fmt.Errorf("通话音频仅在 macOS 版本可用")
}
func (r *audioRouter) stopRecording() (string, error) { return "", nil }
func (r *audioRouter) isRecording() bool              { return false }
func (r *audioRouter) recordingPath() string          { return "" }

func logAudioRouterState(*audioRouter) {}
