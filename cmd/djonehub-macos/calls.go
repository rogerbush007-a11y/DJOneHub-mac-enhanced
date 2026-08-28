package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type callRecord struct {
	ID        string     `json:"id"`
	Index     int        `json:"index"`
	Direction string     `json:"direction"`
	State     string     `json:"state"`
	Number    string     `json:"number,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Missed    bool       `json:"missed"`
}

type parsedCall struct {
	Index     int
	Direction string
	State     string
	Number    string
}

var clccPattern = regexp.MustCompile(`\+CLCC:\s*(\d+),(\d+),(\d+),(\d+),(\d+)(?:,"([^"]*)",(\d+))?`)

func parseCLCC(response string) []parsedCall {
	matches := clccPattern.FindAllStringSubmatch(response, -1)
	out := make([]parsedCall, 0, len(matches))
	for _, match := range matches {
		// CLCC mode 0 is voice. Mode 1 is a data session and must not surface
		// as a phone call in the macOS UI.
		if match[4] != "0" {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		out = append(out, parsedCall{
			Index:     index,
			Direction: mapCallDirection(match[2]),
			State:     mapCallState(match[3]),
			Number:    strings.TrimSpace(match[6]),
		})
	}
	return out
}

func mapCallDirection(raw string) string {
	if raw == "1" {
		return "incoming"
	}
	return "outgoing"
}

func mapCallState(raw string) string {
	switch raw {
	case "0":
		return "active"
	case "1":
		return "held"
	case "2":
		return "dialing"
	case "3":
		return "alerting"
	case "4":
		return "incoming"
	case "5":
		return "waiting"
	default:
		return "unknown"
	}
}

func (a *app) startCallPoller(ctx context.Context) {
	interval := a.callPollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			nextInterval := interval
			if a.currentUSBDevice() == nil {
				nextInterval = 6 * time.Second
			}
			if err := a.pollCallOnce(); err != nil {
				log.Printf("call poll failed: %v", err)
			}
			timer.Reset(nextInterval)
		}
	}
}

func (a *app) pollCallOnce() error {
	if a.demo {
		return nil
	}
	if a.modem == nil && a.currentUSBDevice() == nil {
		a.setCallPollStatus(fmt.Errorf("DJI USB device is not connected"))
		return nil
	}

	a.callMu.Lock()
	configured := a.callConfigured
	a.callMu.Unlock()
	if !configured {
		if _, err := a.runATCommand("AT+CLIP=1", 3*time.Second); err != nil {
			a.setCallPollStatus(err)
			return err
		}
		a.logModuleVoiceConfig()
		a.callMu.Lock()
		a.callConfigured = true
		a.callMu.Unlock()
	}

	response, err := a.runATCommand("AT+CLCC", 3*time.Second)
	if err != nil {
		a.setCallPollStatus(err)
		return err
	}
	a.applyCallPoll(parseCLCC(response), time.Now())
	a.setCallPollStatus(nil)
	return nil
}

// logModuleVoiceConfig 一次性记录模块的 USB 音频配置，便于排查通话无声。
func (a *app) logModuleVoiceConfig() {
	for _, cmd := range []string{`AT+QCFG="USBCFG"?`, "AT+QPCMV?", "AT+QDAI?"} {
		resp, err := a.runATCommand(cmd, 3*time.Second)
		if err != nil {
			log.Printf("voice usb diag: %s -> error: %v", cmd, err)
			continue
		}
		log.Printf("voice usb diag: %s -> %s", cmd, strings.TrimSpace(resp))
	}
}

func (a *app) applyCallPoll(calls []parsedCall, now time.Time) {
	var selected *parsedCall
	for i := range calls {
		candidate := &calls[i]
		if selected == nil || callStatePriority(candidate.State) > callStatePriority(selected.State) {
			selected = candidate
		}
	}

	a.callMu.Lock()
	var notify *callRecord
	callEnded := false
	if selected == nil {
		if a.activeCall != nil {
			ended := now
			a.activeCall.EndedAt = &ended
			a.activeCall.UpdatedAt = now
			a.activeCall.Missed = a.activeCall.Direction == "incoming" &&
				(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
			log.Printf("call ended: number=%q state=%q direction=%q duration=%s",
				a.activeCall.Number, a.activeCall.State, a.activeCall.Direction,
				now.Sub(a.activeCall.StartedAt).Round(time.Second))
			a.callHistory = append([]callRecord{*a.activeCall}, a.callHistory...)
			if len(a.callHistory) > 100 {
				a.callHistory = a.callHistory[:100]
			}
			a.activeCall = nil
			// Only tear down the module voice route when a call actually
			// ended; idle polls must not stop a route prepared in advance.
			callEnded = true
		}
		a.callMu.Unlock()
		a.callMu.RLock()
		swiftAudioHost := a.swiftAudioHost
		a.callMu.RUnlock()
		if a.audio != nil && !swiftAudioHost && !a.audioManualSet && a.audio.isRunning() {
			a.audio.stop()
			a.lastAudioHealthLog = time.Time{}
			log.Printf("voice audio routing stopped")
		}
		if callEnded {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				a.stopModuleVoiceRoute()
			}()
		}
		return
	}

	if a.activeCall == nil || a.activeCall.Index != selected.Index || a.activeCall.Direction != selected.Direction {
		record := &callRecord{
			ID:        fmt.Sprintf("%d-%d", now.UnixMilli(), selected.Index),
			Index:     selected.Index,
			Direction: selected.Direction,
			State:     selected.State,
			Number:    selected.Number,
			StartedAt: now,
			UpdatedAt: now,
		}
		a.activeCall = record
		log.Printf("call started: number=%q state=%q direction=%q", selected.Number, selected.State, selected.Direction)
		if selected.Direction == "incoming" && (selected.State == "incoming" || selected.State == "waiting") {
			copy := *record
			notify = &copy
		}
	} else {
		wasRinging := a.activeCall.State == "incoming" || a.activeCall.State == "waiting"
		prevState := a.activeCall.State
		a.activeCall.State = selected.State
		if prevState != selected.State {
			log.Printf("call state %q -> %q (number=%q)", prevState, selected.State, selected.Number)
		}
		a.activeCall.UpdatedAt = now
		if selected.Number != "" {
			a.activeCall.Number = selected.Number
		}
		if selected.Direction == "incoming" && !wasRinging &&
			(selected.State == "incoming" || selected.State == "waiting") {
			copy := *a.activeCall
			notify = &copy
		}
	}
	a.callMu.Unlock()

	if notify != nil {
		if a.callNotifier != nil {
			a.callNotifier(*notify)
		}
	}
	// Match MaVo's verified lifecycle: the call is established first and media
	// starts only after CLCC reports one active voice call.
	a.callMu.RLock()
	swiftAudioHost := a.swiftAudioHost
	a.callMu.RUnlock()
	if selected.State == "active" && swiftAudioHost {
		if err := a.ensureModuleVoiceRoute(); err != nil {
			log.Printf("module voice route start for native MaVo host failed: %v", err)
		} else {
			log.Printf("module voice route ready for native MaVo audio host")
		}
	} else if selected.State == "active" && a.audio != nil && !a.audioManualSet && !a.audio.isRunning() {
		if err := a.ensureModuleVoiceRoute(); err != nil {
			log.Printf("module voice route start after active CLCC failed: %v", err)
		} else if err := a.audio.start(); err != nil {
			log.Printf("voice audio start failed: %v", err)
		} else {
			log.Printf("voice audio routing started")
			if devs := a.audio.audioDevices(); len(devs) > 0 {
				log.Printf("voice audio devices: mod_in=%q mod_out=%q mac_in=%q mac_out=%q formats=%q",
					devs["mod_in"], devs["mod_out"], devs["mac_in"], devs["mac_out"],
					a.audio.formats())
			}
		}
	}
	// Throttled audio health snapshot while a call is up: whether the module's
	// USB audio is actually delivering voice (mod_in/far) and the Mac side is
	// consuming it (mac_out), so a silent path is easy to trace.
	if a.audio != nil && a.audio.isRunning() && time.Since(a.lastAudioHealthLog) >= 6*time.Second {
		a.lastAudioHealthLog = time.Now()
		_, farPeak, nearPeak, _ := a.audio.state()
		stats := a.audio.audioStats()
		farLive, nearLive, farOutLive, nearOutLive := a.audio.live()
		log.Printf("audio health: far_peak=%.4f near_peak=%.4f far_live=%.4f near_live=%.4f far_out=%.4f near_out=%.4f mod_in=%d mod_out=%d mac_in=%d mac_out=%d far_ring=%d fmt_chg=%q",
			farPeak, nearPeak, farLive, nearLive, farOutLive, nearOutLive,
			stats["mod_in_calls"], stats["mod_out_calls"], stats["mac_in_calls"], stats["mac_out_calls"],
			stats["far_ring_used"], a.audio.formatChanges())
	}
}

func callStatePriority(state string) int {
	switch state {
	case "incoming", "waiting":
		return 5
	case "active":
		return 4
	case "alerting":
		return 3
	case "dialing":
		return 2
	case "held":
		return 1
	default:
		return 0
	}
}

func (a *app) setCallPollStatus(err error) {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	a.callLastPoll = time.Now()
	if err != nil {
		a.callLastPollError = err.Error()
		return
	}
	a.callLastPollError = ""
}

func (a *app) callStatus(w http.ResponseWriter, _ *http.Request) {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	var active *callRecord
	if a.activeCall != nil {
		copy := *a.activeCall
		active = &copy
	}
	history := append([]callRecord(nil), a.callHistory...)
	audioRunning := false
	var audioFarPeak, audioNearPeak float64
	audioError := ""
	var audioStats map[string]int64
	audioDevices := map[string]string{}
	audioLive := map[string]float64{"far": 0, "near": 0, "far_out": 0, "near_out": 0}
	audioFormats := ""
	audioFmtLog := ""
	if a.audio != nil {
		audioRunning, audioFarPeak, audioNearPeak, audioError = a.audio.state()
		audioStats = a.audio.audioStats()
		audioDevices = a.audio.audioDevices()
		audioLive["far"], audioLive["near"], audioLive["far_out"], audioLive["near_out"] = a.audio.live()
		audioFormats = a.audio.formats()
		audioFmtLog = a.audio.formatChanges()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":          active,
		"history":         history,
		"polling":         !a.demo,
		"poll_interval_s": int(a.callPollInterval.Seconds()),
		"last_poll":       a.callLastPoll,
		"last_poll_error": a.callLastPollError,
		"audio": map[string]any{
			"running":   audioRunning,
			"far_peak":  audioFarPeak,
			"near_peak": audioNearPeak,
			"error":     audioError,
			"stats":     audioStats,
			"devices":   audioDevices,
			"live":      audioLive,
			"formats":   audioFormats,
			"fmt_log":   audioFmtLog,
		},
	})
}

func (a *app) rejectCall(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		a.applyCallPoll(nil, time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"rejected": true})
		return
	}
	response, err := a.runATCommand("AT+CHUP", 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rejected": true,
		"response": response,
	})
}

func (a *app) answerCall(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"answered": true})
		return
	}
	// Debounce duplicate answer requests: the incoming-call popup and the main
	// call screen can both fire ATA within a few hundred ms, and the second
	// ATA returns ERROR and can disturb call setup.
	a.callMu.Lock()
	if time.Since(a.lastAnswerAt) < 2*time.Second {
		a.callMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"answered": true})
		return
	}
	a.lastAnswerAt = time.Now()
	a.callMu.Unlock()

	response, err := a.runATCommand("ATA", 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("answer call: ATA -> %s", strings.TrimSpace(response))
	writeJSON(w, http.StatusOK, map[string]any{
		"answered": true,
		"response": response,
	})
}

func (a *app) hangupCall(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		a.applyCallPoll(nil, time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"hung_up": true})
		return
	}
	response, err := a.runATCommand("ATH", 5*time.Second)
	if err != nil || validateCallATResponse(response) != nil {
		// Some firmwares only accept AT+CHUP to end the current call.
		response, err = a.runATCommand("AT+CHUP", 5*time.Second)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("hangup call: -> %s", strings.TrimSpace(response))
	writeJSON(w, http.StatusOK, map[string]any{
		"hung_up":  true,
		"response": response,
	})
}

func (a *app) dtmfCall(w http.ResponseWriter, r *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
		return
	}
	var body struct {
		Digit string `json:"digit"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	digit := body.Digit
	if len(digit) != 1 || !strings.ContainsRune("0123456789*#", rune(digit[0])) {
		writeError(w, http.StatusBadRequest, "DTMF 仅支持 0-9 * #")
		return
	}
	response, err := a.runATCommand(fmt.Sprintf("AT+VTS=\"%s\"", digit), 3*time.Second)
	if err != nil || strings.Contains(response, "ERROR") {
		// Some firmwares only accept AT+CLDTMF=<onoff>,<digit>.
		response, err = a.runATCommand(fmt.Sprintf("AT+CLDTMF=1,%s", digit), 3*time.Second)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	log.Printf("dtmf: digit=%q -> %s", digit, strings.TrimSpace(response))
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (a *app) dialCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number string `json:"number"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	number := normalizeDialNumber(body.Number)
	if number == "" {
		writeError(w, http.StatusBadRequest, "号码为空或包含非法字符")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"dialing": true})
		return
	}
	response, err := a.runATCommand("ATD"+number+";", 8*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("dial call: ATD%s; -> %s", number, strings.TrimSpace(response))
	writeJSON(w, http.StatusOK, map[string]any{
		"dialing":  true,
		"number":   number,
		"response": response,
	})
}

// validateCallATResponse keeps command echoes and modem ERROR replies from
// being reported as successful button presses. usbAT returns a completed AT
// response for both OK and ERROR, so transport success alone is insufficient.
func validateCallATResponse(response string) error {
	if atResponseIsError(response) {
		return fmt.Errorf("模块拒绝通话命令（ERROR）")
	}
	return nil
}

// normalizeDialNumber keeps digits plus + * # and strips common formatting
// characters. Any other character makes the number invalid.
func normalizeDialNumber(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == '*' || r == '#':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')':
			// formatting only
		default:
			return ""
		}
	}
	return b.String()
}

func (a *app) audioStart(w http.ResponseWriter, _ *http.Request) {
	if a.audio == nil {
		writeError(w, http.StatusBadGateway, "通话音频不可用")
		return
	}
	a.callMu.Lock()
	hasActiveCall := a.activeCall != nil
	if !hasActiveCall {
		a.callMu.Unlock()
		writeError(w, http.StatusConflict, "当前没有通话；来电或拨号后会自动启动音频")
		return
	}
	a.audioManualSet = true
	a.audioManualOn = true
	a.callMu.Unlock()
	if err := a.audio.start(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if devs := a.audio.audioDevices(); len(devs) > 0 {
		log.Printf("voice audio started: mod_in=%q mod_out=%q mac_in=%q mac_out=%q",
			devs["mod_in"], devs["mod_out"], devs["mac_in"], devs["mac_out"])
	}
	writeJSON(w, http.StatusOK, map[string]bool{"audio_running": true})
}

func (a *app) audioStop(w http.ResponseWriter, _ *http.Request) {
	if a.audio != nil {
		a.callMu.Lock()
		a.audioManualSet = false
		a.audioManualOn = false
		a.callMu.Unlock()
		a.audio.stop()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"audio_running": false})
}

func (a *app) audioMute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Muted bool `json:"muted"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if a.audio != nil {
		a.audio.setMuted(body.Muted)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"muted": body.Muted})
}

func (a *app) audioRecord(w http.ResponseWriter, r *http.Request) {
	if a.swiftAudioHost {
		writeError(w, http.StatusConflict, "MaVo 音频策略已接管媒体；录音观察器尚未接入，避免影响实时音频")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if a.audio == nil {
		writeError(w, http.StatusBadGateway, "通话音频不可用")
		return
	}
	switch strings.ToLower(strings.TrimSpace(body.Action)) {
	case "start":
		if err := a.audio.start(); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		path, err := a.audio.startRecording()
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recording": true, "path": path})
	case "stop":
		path, err := a.audio.stopRecording()
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recording": false, "path": path})
	default:
		writeError(w, http.StatusBadRequest, "action must be start or stop")
	}
}

// audioHostRegister selects the exact MaVo-derived Swift host route.  It is
// opt-in so an older installed notifier continues to use the previous Go
// route until this app has registered successfully.
func (a *app) audioHostRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.callMu.Lock()
	a.swiftAudioHost = body.Enabled
	a.callMu.Unlock()
	if body.Enabled && a.audio != nil && a.audio.isRunning() {
		a.audio.stop()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": body.Enabled})
}

func (a *app) audioHostConfig(w http.ResponseWriter, _ *http.Request) {
	device := discoverDJIUSBDevice()
	if device == nil {
		writeError(w, http.StatusBadGateway, "未找到模块 USB 身份")
		return
	}
	parse := func(raw string) uint64 {
		value, _ := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(raw), "0x"), 16, 32)
		return value
	}
	a.moduleVoiceMu.Lock()
	routeReady := a.moduleVoiceReady
	routeError := a.moduleVoiceErr
	a.moduleVoiceMu.Unlock()
	a.callMu.RLock()
	hostEnabled := a.swiftAudioHost
	a.callMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"vendor_id":    uint16(parse(device.VendorID)),
		"product_id":   uint16(parse(device.ProductID)),
		"location_id":  uint32(parse(device.LocationID)),
		"route_ready":  routeReady,
		"route_error":  routeError,
		"host_enabled": hostEnabled,
	})
}
