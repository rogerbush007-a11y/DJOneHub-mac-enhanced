//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func windowsLogDir() string {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "DJOneHub", "logs")
}

func windowsLogFile() string {
	return filepath.Join(windowsLogDir(), "djonehub.log")
}

func initPlatformRuntime() func() {
	logDir := windowsLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return func() {}
	}
	file, err := os.OpenFile(windowsLogFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return func() {}
	}
	log.SetOutput(io.MultiWriter(file))
	log.Printf("DJOneHub Windows is starting")
	return func() { _ = file.Close() }
}

func withPlatformLifecycle(parent context.Context, uiURL string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stopTray := startWindowsTray(ctx, cancel, uiURL)
	return ctx, func() {
		stopTray()
		cancel()
	}
}

func newHiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func resolveDialTarget(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err == nil && strings.TrimSpace(parsed.Host) != "" {
		return parsed.Host
	}
	return "127.0.0.1:7575"
}

func openPlatformUI(rawURL string) {
	if focusExistingPlatformUIWindow() {
		return
	}
	target := resolveDialTarget(rawURL)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", target, 250*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		if focusExistingPlatformUIWindow() {
			return
		}
		if startEdgeApp(rawURL) {
			return
		}
		if err := newHiddenCommand("rundll32.exe", "url.dll,FileProtocolHandler", rawURL).Start(); err != nil {
			log.Printf("open desktop UI: %v", err)
		}
	}()
}

func platformOpenExistingUI(rawURL string) bool {
	target := resolveDialTarget(rawURL)
	conn, err := net.DialTimeout("tcp", target, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	if focusExistingPlatformUIWindow() {
		return true
	}
	if startEdgeApp(rawURL) {
		return true
	}
	if err := newHiddenCommand("rundll32.exe", "url.dll,FileProtocolHandler", rawURL).Start(); err != nil {
		log.Printf("open existing desktop UI: %v", err)
	}
	return true
}

func startEdgeApp(rawURL string) bool {
	var candidates []string
	for _, base := range []string{os.Getenv("PROGRAMFILES(X86)"), os.Getenv("PROGRAMFILES"), os.Getenv("LOCALAPPDATA")} {
		if strings.TrimSpace(base) == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
		)
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			continue
		}
		if err := newHiddenCommand(path, "--app="+rawURL, "--start-windowed").Start(); err == nil {
			return true
		}
	}
	return false
}

func openWindowsLogDir() {
	dir := windowsLogDir()
	_ = os.MkdirAll(dir, 0o755)
	logFile := windowsLogFile()
	if _, err := os.Stat(logFile); err == nil {
		if shellOpenPath(logFile, dir) {
			return
		}
	}
	if !shellOpenPath(dir, "") {
		log.Printf("open log path failed: %s", dir)
	}
}
