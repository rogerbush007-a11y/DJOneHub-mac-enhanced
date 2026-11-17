package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// gpsFix contains only the fields returned by the module's documented QGPSLOC
// response. It remains in process memory and is exposed only by the local UI.
type gpsFix struct {
	UTC        string    `json:"utc"`
	Latitude   string    `json:"latitude"`
	Longitude  string    `json:"longitude"`
	HDOP       string    `json:"hdop"`
	Altitude   string    `json:"altitude"`
	Fix        string    `json:"fix"`
	Satellites string    `json:"satellites"`
	Timestamp  time.Time `json:"timestamp"`
}

func parseGPSLocation(response string) (*gpsFix, error) {
	line := ""
	for _, candidate := range strings.Split(response, "\n") {
		candidate = strings.TrimSpace(candidate)
		if strings.HasPrefix(candidate, "+QGPSLOC:") {
			line = strings.TrimSpace(strings.TrimPrefix(candidate, "+QGPSLOC:"))
			break
		}
	}
	if line == "" {
		return nil, fmt.Errorf("暂未获得定位，请移至窗边或室外后重试")
	}
	fields := strings.Split(line, ",")
	if len(fields) < 11 {
		return nil, fmt.Errorf("定位响应格式不完整")
	}
	return &gpsFix{
		UTC: strings.TrimSpace(fields[0]), Latitude: strings.TrimSpace(fields[1]), Longitude: strings.TrimSpace(fields[2]),
		HDOP: strings.TrimSpace(fields[3]), Altitude: strings.TrimSpace(fields[4]), Fix: strings.TrimSpace(fields[5]),
		Satellites: strings.TrimSpace(fields[10]), Timestamp: time.Now(),
	}, nil
}

func (a *app) setGPSResult(fix *gpsFix, err error) {
	a.gpsMu.Lock()
	defer a.gpsMu.Unlock()
	a.gpsLastChecked = time.Now()
	if err != nil {
		a.gpsLastError = err.Error()
		return
	}
	a.gpsLastFix = fix
	a.gpsLastError = ""
}

func hasCMEErrorCode(err error, response, code string) bool {
	if strings.Contains(response, "+CME ERROR: "+code) || strings.Contains(response, "+CME ERROR:"+code) {
		return true
	}
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "+CME ERROR: "+code) || strings.Contains(msg, "+CME ERROR:"+code)
}

func (a *app) readGPSLocation() (*gpsFix, error) {
	response, err := a.runATCommand("AT+QGPSLOC=2", 12*time.Second)
	if err != nil {
		if hasCMEErrorCode(err, response, "516") {
			return nil, fmt.Errorf("暂未获得定位，请移至窗边或室外后重试")
		}
		return nil, err
	}
	return parseGPSLocation(response)
}

func (a *app) refreshGPSOnce() (*gpsFix, error) {
	fix, err := a.readGPSLocation()
	a.setGPSResult(fix, err)
	return fix, err
}

func (a *app) startGPSPoller(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.gpsMu.RLock()
			enabled := a.gpsEnabled
			a.gpsMu.RUnlock()
			if enabled {
				_, _ = a.refreshGPSOnce()
			}
		}
	}
}

// syncGPSState restores the local service state after a DJOneHub restart.
// QGPS is a module runtime setting and can remain enabled across a local
// process restart, so the menu-bar indicator must read it back instead of
// assuming the zero-value state is correct.
func (a *app) syncGPSState() {
	response, err := a.runATCommand("AT+QGPS?", 5*time.Second)
	if err != nil {
		return
	}
	enabled := strings.Contains(response, "+QGPS: 1")
	a.gpsMu.Lock()
	a.gpsEnabled = enabled
	if !enabled {
		a.gpsLastFix = nil
	}
	a.gpsMu.Unlock()
}

func (a *app) gpsStatus(w http.ResponseWriter, _ *http.Request) {
	a.gpsMu.RLock()
	defer a.gpsMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": a.gpsEnabled, "last_fix": a.gpsLastFix, "last_checked": a.gpsLastChecked,
		"last_error": a.gpsLastError, "poll_interval_s": 15,
	})
}

func (a *app) startGPS(w http.ResponseWriter, _ *http.Request) {
	response, err := a.runATCommand("AT+QGPS=1", 8*time.Second)
	// +CME ERROR: 504 means a GNSS session is already active on the module.
	alreadyActive := hasCMEErrorCode(err, response, "504")
	if !alreadyActive && (err != nil || !strings.Contains(response, "OK")) {
		if err == nil {
			err = fmt.Errorf("模块未确认启动定位")
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.gpsMu.Lock()
	a.gpsEnabled = true
	a.gpsLastError = ""
	a.gpsMu.Unlock()
	_, _ = a.refreshGPSOnce()
	a.gpsMu.RLock()
	defer a.gpsMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "last_fix": a.gpsLastFix, "last_error": a.gpsLastError})
}

func (a *app) stopGPS(w http.ResponseWriter, _ *http.Request) {
	response, err := a.runATCommand("AT+QGPSEND", 8*time.Second)
	// +CME ERROR: 505 means no GNSS session is currently active.
	alreadyStopped := hasCMEErrorCode(err, response, "505")
	if !alreadyStopped && (err != nil || !strings.Contains(response, "OK")) {
		if err == nil {
			err = fmt.Errorf("模块未确认停止定位")
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.gpsMu.Lock()
	a.gpsEnabled = false
	a.gpsLastError = ""
	a.gpsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": false})
}

func (a *app) refreshGPS(w http.ResponseWriter, _ *http.Request) {
	a.gpsMu.RLock()
	enabled := a.gpsEnabled
	a.gpsMu.RUnlock()
	if !enabled {
		writeError(w, http.StatusConflict, "请先启动定位")
		return
	}
	fix, err := a.refreshGPSOnce()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, fix)
}
