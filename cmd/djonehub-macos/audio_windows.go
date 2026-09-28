//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vohive/internal/winaudio"
	"go.bug.st/serial"
)

// audioRouter carries call audio between the module and this machine's own
// microphone and speakers. It is the Windows counterpart of the CoreAudio
// router in audio_darwin.go.
//
// The module side uses Quectel's "Voice over USB" (AT+QPCMV=1,0), which streams
// raw PCM over the module's NMEA interface, rather than the UAC sound-card mode
// (AT+QPCMV=1,2). Both work, but UAC makes Windows promote the module to
// default communications device — silently moving the user's system audio into
// a phone modem — and it adds a format negotiation that Voice over USB does not
// need: the stream is always 8 kHz, 16-bit, mono.
//
//	far:  module PCM port -> speakers       (the remote party)
//	near: microphone      -> module PCM port (our uplink)
//
// Each pump runs on a locked OS thread so the WASAPI half keeps COM on one
// thread per direction. Levels and counters are atomics so HTTP status handlers
// never block a pump.
type audioRouter struct {
	mu        sync.Mutex
	running   bool
	lastError string
	stopCh    chan struct{}
	wg        sync.WaitGroup

	muted atomic.Bool
	// nearSrc selects what the remote party hears; see nearSource.
	nearSrc atomic.Int32
	// nearGain scales the system-audio contribution; see setNearGain.
	nearGain atomicFloat

	farPeak     atomicFloat
	nearPeak    atomicFloat
	farLive     atomicFloat
	nearLive    atomicFloat
	farOutLive  atomicFloat
	nearOutLive atomicFloat

	modInCalls  atomic.Int64
	modOutCalls atomic.Int64
	macInCalls  atomic.Int64
	macOutCalls atomic.Int64
	farRingUsed atomic.Int64
	nearRingUse atomic.Int64

	devNames map[string]string
	fmtInfo  string

	port   serial.Port
	portMu sync.Mutex

	recMu   sync.Mutex
	rec     *wavWriter
	recPath string
	nearTap *sampleRing
}

// nearSource selects what is sent up to the remote party.
//
// The microphone is always opened even when it is not the source: a loopback
// client produces no packets at all while its endpoint is idle, so it cannot
// pace the uplink, whereas a capture client always does. The microphone is
// therefore the clock and the source only decides what gets mixed into each
// frame it delivers.
type nearSource int32

const (
	nearSourceMic nearSource = iota
	nearSourceSystem
	nearSourceMix
)

func parseNearSource(name string) (nearSource, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "mic", "microphone", "":
		return nearSourceMic, true
	case "system", "loopback":
		return nearSourceSystem, true
	case "mix", "both":
		return nearSourceMix, true
	}
	return nearSourceMic, false
}

func (s nearSource) String() string {
	switch s {
	case nearSourceSystem:
		return "system"
	case nearSourceMix:
		return "mix"
	default:
		return "mic"
	}
}

func newAudioRouter() *audioRouter {
	r := &audioRouter{
		devNames: map[string]string{"mod_in": "", "mod_out": "", "mac_in": "", "mac_out": ""},
		nearTap:  newSampleRing(modulePCMRate * 4),
	}
	r.nearGain.set(1)
	return r
}

// moduleEndpointHints is only consulted when an endpoint's topology cannot be
// walked, which is why it stays narrow. "AC Interface" is what the in-box
// usbaudio driver names the module's audio control interface.
var moduleEndpointHints = []string{"ac interface", "qdc507"}

var moduleUSBIdentities = [][2]uint16{
	{quectelUSBVendorID, quectelUSBProductID},
	{djiUSBVendorID, djiUSBProductID},
}

// isModuleEndpoint identifies the module by the USB device an audio endpoint is
// wired to. Upstream's CUACProbe makes the same guarantee on macOS ("Device
// names are never used for binding"). It matters here even though the module
// side is a serial port: with UAC enabled the module also appears as a sound
// card, and it must never be picked as this machine's microphone or speakers.
func isModuleEndpoint(d winaudio.Device) bool {
	for _, id := range moduleUSBIdentities {
		if d.MatchesUSB(id[0], id[1]) {
			return true
		}
	}
	lower := strings.ToLower(d.Name)
	for _, hint := range moduleEndpointHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// pickSystem finds this machine's own endpoint for a direction, never the
// module's. Enabling UAC makes Windows promote the module to default
// communications device, so the default must be filtered rather than trusted.
func pickSystem(flow winaudio.Flow) (winaudio.Device, error) {
	if d, err := winaudio.Default(flow); err == nil && !isModuleEndpoint(d) {
		return d, nil
	}
	devices, err := winaudio.Enumerate(flow)
	if err != nil {
		return winaudio.Device{}, err
	}
	for _, d := range devices {
		if !isModuleEndpoint(d) {
			return d, nil
		}
	}
	return winaudio.Device{}, fmt.Errorf("没有可用的本机音频设备（除模块自身外）")
}

func (r *audioRouter) start() error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	portName, err := modulePCMPort()
	if err != nil {
		r.setError(err)
		return err
	}
	macIn, err := pickSystem(winaudio.Capture)
	if err != nil {
		r.setError(err)
		return err
	}
	macOut, err := pickSystem(winaudio.Render)
	if err != nil {
		r.setError(err)
		return err
	}

	port, err := serial.Open(portName, &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		err = fmt.Errorf("打开模块 PCM 端口 %s 失败: %w", portName, err)
		r.setError(err)
		return err
	}
	// A read must not block a teardown for long.
	_ = port.SetReadTimeout(100 * time.Millisecond)
	// The module starts streaming when AT+QPCMV=1,0 is accepted, which happens
	// before this port is opened, so the driver already holds a backlog whose
	// first byte can land anywhere inside a 16-bit sample. Drop it; pcmStream
	// then locks the sample phase onto the fresh data.
	_ = port.ResetInputBuffer()

	r.mu.Lock()
	r.port = port
	r.stopCh = make(chan struct{})
	r.running = true
	r.lastError = ""
	r.devNames = map[string]string{
		"mod_in": portName + " (PCM)", "mod_out": portName + " (PCM)",
		"mac_in": macIn.Name, "mac_out": macOut.Name,
	}
	stop := r.stopCh
	r.mu.Unlock()

	ready := make(chan error, 2)
	r.wg.Add(2)
	go r.pumpFar(macOut, stop, ready)
	go r.pumpNear(macIn, macOut, stop, ready)

	// Both directions must come up; otherwise the call is half-duplex and the
	// user gets silence in one ear with no explanation.
	var firstErr error
	for i := 0; i < 2; i++ {
		if err := <-ready; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		r.stop()
		r.setError(firstErr)
		return firstErr
	}
	return nil
}

func (r *audioRouter) setError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastError = err.Error()
}

func (r *audioRouter) stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	if r.stopCh != nil {
		close(r.stopCh)
		r.stopCh = nil
	}
	port := r.port
	r.port = nil
	r.mu.Unlock()

	r.wg.Wait()
	if port != nil {
		port.Close()
	}

	r.recMu.Lock()
	if r.rec != nil {
		r.rec.Close()
		r.rec = nil
	}
	r.recMu.Unlock()

	for _, m := range []*atomicFloat{&r.farPeak, &r.nearPeak, &r.farLive, &r.nearLive, &r.farOutLive, &r.nearOutLive} {
		m.set(0)
	}
}

func (r *audioRouter) setMuted(muted bool) { r.muted.Store(muted) }

func (r *audioRouter) nearSource() nearSource { return nearSource(r.nearSrc.Load()) }

func (r *audioRouter) setNearSource(s nearSource) { r.nearSrc.Store(int32(s)) }

// setNearGain scales system audio on its way to the remote party. A loopback
// stream is captured after the endpoint's volume has been applied, so a machine
// playing quietly sends a correspondingly quiet signal; the gain compensates
// without touching what the user actually hears.
func (r *audioRouter) setNearGain(g float64) {
	if g < 0 {
		g = 0
	} else if g > 32 {
		g = 32
	}
	r.nearGain.set(g)
}

func (r *audioRouter) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *audioRouter) state() (bool, float64, float64, string) {
	r.mu.Lock()
	running, errText := r.running, r.lastError
	r.mu.Unlock()
	return running, r.farPeak.get(), r.nearPeak.get(), errText
}

func (r *audioRouter) audioStats() map[string]int64 {
	return map[string]int64{
		"mod_in_calls":   r.modInCalls.Load(),
		"mod_out_calls":  r.modOutCalls.Load(),
		"mac_in_calls":   r.macInCalls.Load(),
		"mac_out_calls":  r.macOutCalls.Load(),
		"far_ring_used":  r.farRingUsed.Load(),
		"near_ring_used": r.nearRingUse.Load(),
	}
}

func (r *audioRouter) audioDevices() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.devNames)+1)
	for k, v := range r.devNames {
		out[k] = v
	}
	out["near_source"] = r.nearSource().String()
	return out
}

func (r *audioRouter) live() (float64, float64, float64, float64) {
	return r.farLive.get(), r.nearLive.get(), r.farOutLive.get(), r.nearOutLive.get()
}

func (r *audioRouter) formats() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fmtInfo
}

// formatChanges has no Windows equivalent: the module side is a fixed 8 kHz
// stream and the host endpoints are re-read whenever the route restarts.
func (r *audioRouter) formatChanges() string { return "" }

func (r *audioRouter) noteFormat(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fmtInfo == "" {
		r.fmtInfo = text
		return
	}
	r.fmtInfo += " " + text
}

// readPCM pulls whatever the module has queued without blocking teardown.
func (r *audioRouter) readPCM(buf []byte) (int, error) {
	r.mu.Lock()
	port := r.port
	r.mu.Unlock()
	if port == nil {
		return 0, fmt.Errorf("PCM 端口已关闭")
	}
	return port.Read(buf)
}

func (r *audioRouter) writePCM(buf []byte) error {
	r.mu.Lock()
	port := r.port
	r.mu.Unlock()
	if port == nil {
		return fmt.Errorf("PCM 端口已关闭")
	}
	r.portMu.Lock()
	defer r.portMu.Unlock()
	for len(buf) > 0 {
		n, err := port.Write(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("PCM 端口写入停滞")
		}
		buf = buf[n:]
	}
	return nil
}

// pumpFar moves the remote party's voice from the module to the speakers.
func (r *audioRouter) pumpFar(macOut winaudio.Device, stop <-chan struct{}, ready chan<- error) {
	defer r.wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	winaudio.InitCOM()

	render, err := winaudio.OpenRender(macOut, 60)
	if err != nil {
		ready <- fmt.Errorf("打开本机扬声器失败: %w", err)
		return
	}
	defer render.Close()

	outFmt := render.Format()
	r.noteFormat(fmt.Sprintf("mod_in=%dHz/1ch/int16 mac_out=%s", modulePCMRate, outFmt))
	ready <- nil

	conv := newConverter(modulePCMRate, modulePCMChannels, outFmt.SampleRate, outFmt.Channels)
	stream := newPCMStream()
	phaseLogged := false
	raw := make([]byte, 4096)
	var pending []float32
	maxPending := int(farLatencyBudget.Seconds()*float64(outFmt.SampleRate)) * outFmt.Channels

	for {
		select {
		case <-stop:
			return
		default:
		}

		n, err := r.readPCM(raw)
		if err != nil {
			select {
			case <-stop:
			default:
				r.setError(fmt.Errorf("模块 PCM 读取中断: %w", err))
			}
			return
		}
		if n > 0 {
			r.modInCalls.Add(1)
			mono := stream.push(raw[:n])
			if !phaseLogged && stream.phase >= 0 {
				phaseLogged = true
				log.Printf("audio: locked PCM sample phase %d (roughness %.2f / %.2f)",
					stream.phase, stream.score0, stream.score1)
			}
			if len(mono) > 0 {
				peak, rms := levels(mono)
				r.farPeak.max(peak)
				r.farLive.set(rms)
				r.tapRecording(mono, true)
				pending = append(pending, conv.convert(mono)...)
				if len(pending) > maxPending {
					pending = append(pending[:0], pending[len(pending)-maxPending:]...)
				}
			}
		}

		if len(pending) > 0 {
			outPeak, _ := levels(pending)
			r.farOutLive.set(outPeak)
			written, err := render.Write(pending, 10)
			if err != nil {
				r.setError(fmt.Errorf("扬声器写入失败: %w", err))
				return
			}
			consumed := written * outFmt.Channels
			if consumed >= len(pending) {
				pending = pending[:0]
			} else {
				pending = append(pending[:0], pending[consumed:]...)
			}
			r.macOutCalls.Add(1)
			r.farRingUsed.Store(int64(len(pending)))
		}
		if n == 0 && len(pending) == 0 {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// pumpNear moves the selected uplink source into the module.
func (r *audioRouter) pumpNear(macIn, macOut winaudio.Device, stop <-chan struct{}, ready chan<- error) {
	defer r.wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	winaudio.InitCOM()

	capture, err := winaudio.OpenCapture(macIn, 60)
	if err != nil {
		ready <- fmt.Errorf("打开本机麦克风失败: %w", err)
		return
	}
	defer capture.Close()

	inFmt := capture.Format()
	r.noteFormat(fmt.Sprintf("mac_in=%s mod_out=%dHz/1ch/int16", inFmt, modulePCMRate))

	// Opened up front and kept regardless of the current source so switching
	// is instant. Losing it is not fatal: the microphone still works, only the
	// system-audio sources become unavailable.
	var loopback *winaudio.CaptureStream
	var loopConv *converter
	loopRing := newSampleRing(modulePCMRate)
	if lb, err := winaudio.OpenLoopback(macOut, 60); err != nil {
		log.Printf("audio: system-audio capture unavailable: %v", err)
	} else {
		loopback = lb
		lf := lb.Format()
		loopConv = newConverter(lf.SampleRate, lf.Channels, modulePCMRate, modulePCMChannels)
		r.noteFormat("loopback=" + lf.String())
		defer loopback.Close()
	}
	ready <- nil

	conv := newConverter(inFmt.SampleRate, inFmt.Channels, modulePCMRate, modulePCMChannels)
	var in []float32
	var loopIn []float32

	for {
		select {
		case <-stop:
			return
		default:
		}

		buf, frames, err := capture.Read(in[:0], 20)
		if err != nil {
			r.setError(fmt.Errorf("麦克风中断: %w", err))
			return
		}
		if frames == 0 {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		r.macInCalls.Add(1)

		// Drain whatever the machine has played since the last pass. A loopback
		// endpoint delivers nothing while idle, so this simply adds no samples.
		if loopback != nil {
			lb, lbFrames, err := loopback.Read(loopIn[:0], 0)
			loopIn = lb
			if err == nil && lbFrames > 0 {
				loopRing.push(loopConv.convert(loopConv.toMono(lb)))
			}
		}

		mono := conv.toMono(buf)
		if r.muted.Load() {
			for i := range mono {
				mono[i] = 0
			}
		}
		out := conv.convert(mono)

		// The microphone paces the uplink; system audio is aligned to the frame
		// count it just produced, zero-padded when the machine is quiet.
		if src := r.nearSource(); src != nearSourceMic && loopback != nil {
			system := loopRing.take(len(out))
			gain := float32(r.nearGain.get())
			if src == nearSourceSystem {
				for i := range out {
					out[i] = clampUnit(system[i] * gain)
				}
			} else {
				for i := range out {
					out[i] = clampUnit(out[i] + system[i]*gain)
				}
			}
		}

		peak, rms := levels(out)
		r.nearPeak.max(peak)
		r.nearLive.set(rms)
		if len(out) == 0 {
			continue
		}
		outPeak, _ := levels(out)
		r.nearOutLive.set(outPeak)
		r.tapRecording(out, false)

		pcm := make([]byte, len(out)*2)
		for i, v := range out {
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(clampUnit(v)*32767)))
		}
		if err := r.writePCM(pcm); err != nil {
			select {
			case <-stop:
			default:
				r.setError(fmt.Errorf("模块 PCM 写入失败: %w", err))
			}
			return
		}
		r.modOutCalls.Add(1)
		r.nearRingUse.Store(int64(len(out)))
	}
}

func logAudioRouterState(r *audioRouter) {
	if r == nil {
		return
	}
	running, far, near, errText := r.state()
	d := r.audioDevices()
	log.Printf("audio router: running=%v far_peak=%.3f near_peak=%.3f mod=%q mac_out=%q err=%q",
		running, far, near, d["mod_in"], d["mac_out"], errText)
}

// registerPlatformAudioRoutes exposes the Windows-only uplink source control.
// The other platforms route only the microphone, so this is a no-op there.
func (a *app) registerPlatformAudioRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/calls/audio/source", a.audioSourceAPI)
	mux.HandleFunc("GET /api/calls/audio/source", a.audioSourceAPI)
	mux.HandleFunc("GET /api/ringtone", a.ringtoneAPI)
}

// ringtoneCandidates are Windows' own notification sounds, most preferred
// first. chord.wav is the one Windows calls "和弦"; the others only matter on
// an install where it is missing.
var ringtoneCandidates = []string{"chord.wav", "Ring01.wav", "notify.wav"}

// ringtoneAPI serves the incoming-call sound from Windows' own media folder.
//
// The file is read from disk rather than embedded: it belongs to Windows, and
// shipping a copy inside the binary would redistribute it. That also means the
// browser gets whatever the running system actually has.
func (a *app) ringtoneAPI(w http.ResponseWriter, r *http.Request) {
	media := filepath.Join(os.Getenv("SystemRoot"), "Media")
	if media == "Media" {
		media = `C:\Windows\Media`
	}
	for _, name := range ringtoneCandidates {
		path := filepath.Join(media, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
		return
	}
	writeError(w, http.StatusNotFound, "系统中未找到可用的提示音")
}

// audioSourceAPI selects what the remote party hears: this machine's
// microphone (the default), whatever it is playing, or both.
//
// Note that "system" and "mix" capture the same endpoint the call is played
// to, so if the call and the system audio share one output device the remote
// party will hear themselves. Sending the call to a separate device — for
// example headphones — avoids that entirely.
func (a *app) audioSourceAPI(w http.ResponseWriter, r *http.Request) {
	if a.audio == nil {
		writeError(w, http.StatusBadGateway, "通话音频不可用")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"source": a.audio.nearSource().String(),
			"gain":   a.audio.nearGain.get(),
		})
		return
	}
	var body struct {
		Source string   `json:"source"`
		Gain   *float64 `json:"gain"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	source, ok := parseNearSource(body.Source)
	if !ok {
		writeError(w, http.StatusBadRequest, "source 仅支持 mic / system / mix")
		return
	}
	a.audio.setNearSource(source)
	if body.Gain != nil {
		a.audio.setNearGain(*body.Gain)
	}
	gain := a.audio.nearGain.get()
	log.Printf("audio: uplink source set to %s (system gain %.1fx)", source, gain)
	writeJSON(w, http.StatusOK, map[string]any{"source": source.String(), "gain": gain})
}
