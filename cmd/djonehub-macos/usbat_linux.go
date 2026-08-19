//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// On Linux the DJI / Quectel module exposes standard USB serial ports via the
// option / cdc-acm driver (e.g. /dev/ttyUSBx or /dev/ttyACMx). The AT command
// port answers "AT" with "OK". We probe candidates to locate the right one,
// which mirrors the macOS libusb bulk-interface probe but uses a real tty.
const djiSerialBaudRate = 115200

type usbAT struct {
	port     serial.Port
	portName string
	mu       sync.Mutex
}

// enumerateSerialCandidates lists USB serial ports. If DJONEHUB_USB_AT_PORT is
// set it is used verbatim (handy when the AT port is a non-standard name).
func enumerateSerialCandidates() ([]string, error) {
	if p := os.Getenv("DJONEHUB_USB_AT_PORT"); p != "" {
		return []string{p}, nil
	}
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, fmt.Errorf("read /dev: %w", err)
	}
	var candidates []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "ttyUSB") || strings.HasPrefix(name, "ttyACM") {
			candidates = append(candidates, filepath.Join("/dev", name))
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("no /dev/ttyUSB* or /dev/ttyACM* found; is the DJI module plugged in?")
	}
	return candidates, nil
}

func openDJIUSBAT() (*usbAT, error) {
	candidates, err := enumerateSerialCandidates()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, name := range candidates {
		p, err := serial.Open(name, &serial.Mode{
			BaudRate: djiSerialBaudRate,
			DataBits: 8,
			StopBits: serial.OneStopBit,
			Parity:   serial.NoParity,
		})
		if err != nil {
			lastErr = fmt.Errorf("open %s: %w", name, err)
			continue
		}
		if err := p.SetReadTimeout(900 * time.Millisecond); err != nil {
			p.Close()
			lastErr = fmt.Errorf("set read timeout %s: %w", name, err)
			continue
		}
		dev := &usbAT{port: p, portName: name}
		if resp, err := dev.Command("AT", 900*time.Millisecond); err == nil && atProbeSucceeded(resp) {
			return dev, nil
		} else {
			if err == nil {
				err = fmt.Errorf("unexpected AT probe response %q", resp)
			}
			lastErr = fmt.Errorf("probe %s: %w", name, err)
		}
		p.Close()
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no serial AT port responded for DJI module (2ca3:4006)")
}

func (u *usbAT) Close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.port != nil {
		_ = u.port.Close()
		u.port = nil
	}
}

func (u *usbAT) Command(cmd string, timeout time.Duration) (string, error) {
	if u == nil || u.port == nil {
		return "", errors.New("USB AT device is not open")
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", errors.New("AT command is empty")
	}
	if !strings.HasPrefix(strings.ToUpper(cmd), "AT") {
		return "", errors.New("command must start with AT")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.drainLocked()
	if err := u.writeLine(cmd+"\r", timeout); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var chunks []string
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 900*time.Millisecond {
			remaining = 900 * time.Millisecond
		}
		data, err := u.readAvailable(remaining)
		if err != nil {
			continue
		}
		if len(data) == 0 {
			continue
		}
		chunks = append(chunks, string(data))
		joined := strings.Join(chunks, "")
		if atResponseComplete(joined) {
			return normalizeATResponse(joined), nil
		}
	}
	if len(chunks) == 0 {
		return "", errors.New("USB AT command timed out without response")
	}
	return normalizeATResponse(strings.Join(chunks, "")), nil
}

// CommandWithPrompt sends an AT command that enters an interactive input state,
// then submits followUp after the modem returns its ">" prompt.
func (u *usbAT) CommandWithPrompt(cmd string, followUp []byte, timeout time.Duration) (string, error) {
	if u == nil || u.port == nil {
		return "", errors.New("USB AT device is not open")
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", errors.New("AT command is empty")
	}
	if !strings.HasPrefix(strings.ToUpper(cmd), "AT") {
		return "", errors.New("command must start with AT")
	}
	if len(followUp) == 0 {
		return "", errors.New("interactive AT follow-up is empty")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.drainLocked()
	if err := u.writeLine(cmd+"\r", timeout); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var response strings.Builder
	promptReceived := false
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 900*time.Millisecond {
			remaining = 900 * time.Millisecond
		}
		data, err := u.readAvailable(remaining)
		if err != nil {
			continue
		}
		if len(data) == 0 {
			continue
		}
		response.Write(data)
		joined := response.String()

		if !promptReceived {
			if atResponseIsError(joined) {
				return normalizeATResponse(joined), nil
			}
			if !atResponseHasPrompt(joined) {
				continue
			}
			if _, err := u.port.Write(followUp); err != nil {
				return normalizeATResponse(joined), err
			}
			promptReceived = true
			continue
		}

		if atResponseComplete(joined) {
			return normalizeATResponse(joined), nil
		}
	}

	if promptReceived {
		// ESC cancels a pending message editor on modems that still accept input.
		_, _ = u.port.Write([]byte{0x1b})
	}
	if response.Len() == 0 {
		return "", errors.New("USB interactive AT command timed out without response")
	}
	return normalizeATResponse(response.String()), errors.New("USB interactive AT command timed out before completion")
}

func (u *usbAT) Description() string {
	if u == nil || u.port == nil {
		return "USB AT"
	}
	return fmt.Sprintf("USB AT · serial %s", u.portName)
}

func (u *usbAT) readAvailable(timeout time.Duration) ([]byte, error) {
	if err := u.port.SetReadTimeout(timeout); err != nil {
		return nil, err
	}
	buf := make([]byte, 512)
	n, err := u.port.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (u *usbAT) writeLine(line string, timeout time.Duration) error {
	if err := u.port.SetReadTimeout(timeout); err != nil {
		return err
	}
	_, err := u.port.Write([]byte(line))
	return err
}

func (u *usbAT) drainLocked() {
	for {
		if _, err := u.readAvailable(80 * time.Millisecond); err != nil {
			return
		}
	}
}

func atResponseComplete(resp string) bool {
	normalized := strings.ReplaceAll(resp, "\r\n", "\n")
	return strings.Contains(normalized, "\nOK\n") ||
		strings.HasSuffix(normalized, "\nOK") ||
		atResponseIsError(normalized)
}

func atResponseIsError(resp string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(resp, "\r\n", "\n"))
	return strings.Contains(normalized, "\nERROR\n") ||
		strings.HasSuffix(normalized, "\nERROR") ||
		strings.Contains(normalized, "+CME ERROR:") ||
		strings.Contains(normalized, "+CMS ERROR:")
}

func atResponseHasPrompt(resp string) bool {
	trimmed := strings.TrimRight(resp, " \t\r\n")
	return strings.HasSuffix(trimmed, ">")
}

func atProbeSucceeded(resp string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(resp), "\r\n", "\n")
	return normalized == "OK" || strings.HasSuffix(normalized, "\nOK")
}

func normalizeATResponse(resp string) string {
	resp = strings.ReplaceAll(resp, "\r\r\n", "\r\n")
	resp = strings.TrimSpace(resp)
	lines := strings.Split(resp, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\r\n")
}
