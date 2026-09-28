//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// audioRouter carries call audio between the module and this machine's own
// microphone and speakers. It is the Linux counterpart of audio_windows.go:
// the module side is the same Voice over USB PCM stream, and the host side is
// PipeWire, reached through pw-cat in raw mode so no cgo audio binding is
// needed.
//
//	far:  module PCM port   -> pw-cat --playback (the remote party)
//	near: pw-cat --record   -> module PCM port   (our uplink)
//
// PipeWire resamples between the module's fixed 8 kHz mono and the devices.
// DJONEHUB_AUDIO_SINK / DJONEHUB_AUDIO_SOURCE pick a specific node (for
// example an echo-cancel sink/source pair); otherwise the default devices are
// used. The streams carry media.role=Communication so the session manager
// treats them as a call.
type audioRouter struct {
	mu        sync.Mutex
	running   bool
	lastError string
	stopCh    chan struct{}
	wg        sync.WaitGroup

	muted atomic.Bool

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
	nearDropped atomic.Int64

	devNames map[string]string
	fmtInfo  string

	port   *os.File
	portMu sync.Mutex
	player *exec.Cmd
	record *exec.Cmd

	recMu   sync.Mutex
	rec     *wavWriter
	recPath string
	nearTap *sampleRing
}

// pipeBufferBytes shrinks the pipe into pw-cat to one page (256 ms of 8 kHz
// PCM) so a slow consumer cannot silently queue seconds of downlink.
const pipeBufferBytes = 4096

func newAudioRouter() *audioRouter {
	return &audioRouter{
		devNames: map[string]string{"mod_in": "", "mod_out": "", "mac_in": "", "mac_out": ""},
		nearTap:  newSampleRing(modulePCMRate * 4),
	}
}

func pipewireArgs(mode, target string) []string {
	args := []string{
		mode,
		"--raw",
		"--rate", fmt.Sprint(modulePCMRate),
		"--channels", fmt.Sprint(modulePCMChannels),
		"--format", "s16",
		"--media-role", "Communication",
		"--latency", "40ms",
		"-P", fmt.Sprintf(`{ node.name = "djonehub-call-%s", node.description = "DJOneHub 通话" }`, mode[2:]),
	}
	if target != "" {
		args = append(args, "--target", target)
	}
	return append(args, "-")
}

func describeTarget(target string) string {
	if target == "" {
		return "PipeWire 默认设备"
	}
	return "PipeWire " + target
}

func (r *audioRouter) start() error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	if _, err := exec.LookPath("pw-cat"); err != nil {
		err = fmt.Errorf("缺少 pw-cat（请安装 pipewire-utils）")
		r.setError(err)
		return err
	}
	portName, err := modulePCMPort()
	if err != nil {
		r.setError(err)
		return err
	}
	port, err := openPCMPort(portName)
	if err != nil {
		err = fmt.Errorf("打开模块 PCM 端口 %s 失败: %w", portName, err)
		r.setError(err)
		return err
	}

	sink := strings.TrimSpace(os.Getenv("DJONEHUB_AUDIO_SINK"))
	source := strings.TrimSpace(os.Getenv("DJONEHUB_AUDIO_SOURCE"))

	player := exec.Command("pw-cat", pipewireArgs("--playback", sink)...)
	playerIn, err := player.StdinPipe()
	if err != nil {
		port.Close()
		r.setError(err)
		return err
	}
	if f, ok := playerIn.(*os.File); ok {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETPIPE_SZ, pipeBufferBytes)
	}
	record := exec.Command("pw-cat", pipewireArgs("--record", source)...)
	recordOut, err := record.StdoutPipe()
	if err != nil {
		port.Close()
		r.setError(err)
		return err
	}
	var playerErr, recordErr strings.Builder
	player.Stderr = &limitedWriter{w: &playerErr, n: 2000}
	record.Stderr = &limitedWriter{w: &recordErr, n: 2000}

	if err := player.Start(); err != nil {
		port.Close()
		err = fmt.Errorf("启动 pw-cat 失败: %w", err)
		r.setError(err)
		return err
	}
	if err := record.Start(); err != nil {
		_ = player.Process.Kill()
		_ = player.Wait()
		port.Close()
		err = fmt.Errorf("启动 pw-cat 录音失败: %w", err)
		r.setError(err)
		return err
	}

	r.mu.Lock()
	r.port = port
	r.player = player
	r.record = record
	r.stopCh = make(chan struct{})
	r.running = true
	r.lastError = ""
	r.fmtInfo = fmt.Sprintf("mod=%dHz/1ch/s16 host=PipeWire(resampled)", modulePCMRate)
	r.devNames = map[string]string{
		"mod_in": portName + " (PCM)", "mod_out": portName + " (PCM)",
		"mac_in": describeTarget(source), "mac_out": describeTarget(sink),
	}
	stop := r.stopCh
	r.mu.Unlock()

	r.wg.Add(2)
	go r.pumpFar(playerIn, stop)
	go r.pumpNear(recordOut, stop)

	// pw-cat exits at once when PipeWire is unreachable or the target is
	// wrong; catch that here instead of reporting a silent call as running.
	exited := make(chan string, 2)
	go func() {
		err := player.Wait()
		exited <- fmt.Sprintf("pw-cat 播放退出: %v %s", err, strings.TrimSpace(playerErr.String()))
	}()
	go func() {
		err := record.Wait()
		exited <- fmt.Sprintf("pw-cat 录音退出: %v %s", err, strings.TrimSpace(recordErr.String()))
	}()
	select {
	case msg := <-exited:
		r.stop()
		err := errors.New(msg)
		r.setError(err)
		return err
	case <-time.After(300 * time.Millisecond):
	}
	go func() {
		select {
		case msg := <-exited:
			select {
			case <-stop:
			default:
				r.setError(errors.New(msg))
				log.Printf("audio: %s", msg)
			}
		case <-stop:
		}
	}()
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
	port, player, record := r.port, r.player, r.record
	r.port, r.player, r.record = nil, nil, nil
	r.mu.Unlock()

	// Killing the helpers breaks their pipes and closing the port wakes any
	// pending read or write, so neither pump can outlive stop.
	for _, cmd := range []*exec.Cmd{player, record} {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	if port != nil {
		port.Close()
	}
	r.wg.Wait()

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
		"near_dropped":   r.nearDropped.Load(),
	}
}

func (r *audioRouter) audioDevices() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.devNames))
	for k, v := range r.devNames {
		out[k] = v
	}
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

func (r *audioRouter) formatChanges() string { return "" }

func (r *audioRouter) readPCM(buf []byte) (int, error) {
	r.mu.Lock()
	port := r.port
	r.mu.Unlock()
	if port == nil {
		return 0, fmt.Errorf("PCM 端口已关闭")
	}
	_ = port.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, err := port.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return n, nil
	}
	return n, err
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
	// The module only drains the uplink while a call carries audio. A frame
	// that cannot be delivered promptly is dropped rather than queued, so the
	// microphone never builds up delay and a stalled port never blocks stop.
	_ = port.SetWriteDeadline(time.Now().Add(pcmWriteBudget))
	_, err := port.Write(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		r.nearDropped.Add(1)
		return nil
	}
	return err
}

const pcmWriteBudget = 100 * time.Millisecond

// openPCMPort opens the module tty non-blocking so Go's poller can apply read
// and write deadlines and Close interrupts pending I/O, then puts it in raw
// mode: PCM is binary and must not be touched by the line discipline.
func openPCMPort(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	raw, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	var termErr error
	ctlErr := raw.Control(func(fd uintptr) {
		t, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
		if err != nil {
			termErr = err
			return
		}
		t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
		t.Oflag &^= unix.OPOST
		t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CRTSCTS | unix.CBAUD
		t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | unix.B115200
		t.Cc[unix.VMIN] = 1
		t.Cc[unix.VTIME] = 0
		if err := unix.IoctlSetTermios(int(fd), unix.TCSETS, t); err != nil {
			termErr = err
			return
		}
		// The module is already streaming; drop the backlog so pcmStream
		// locks the sample phase onto fresh data.
		termErr = unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH)
	})
	if ctlErr != nil || termErr != nil {
		f.Close()
		return nil, fmt.Errorf("配置 %s 为原始模式失败: %v", path, errors.Join(ctlErr, termErr))
	}
	return f, nil
}

func encodePCM(samples []float32) []byte {
	pcm := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(clampUnit(v)*32767)))
	}
	return pcm
}

// pumpFar moves the remote party's voice from the module to pw-cat. A writer
// goroutine feeds the pipe so a stalled player never stops the module from
// being drained; anything beyond farLatencyBudget is dropped oldest-first.
func (r *audioRouter) pumpFar(player io.WriteCloser, stop <-chan struct{}) {
	defer r.wg.Done()
	defer player.Close()

	var (
		qMu     sync.Mutex
		qCond   = sync.NewCond(&qMu)
		queue   []byte
		closed  bool
		maxQ    = int(farLatencyBudget.Seconds()*modulePCMRate) * 2
		writerW sync.WaitGroup
	)
	writerW.Add(1)
	go func() {
		defer writerW.Done()
		for {
			qMu.Lock()
			for len(queue) == 0 && !closed {
				qCond.Wait()
			}
			if closed {
				qMu.Unlock()
				return
			}
			chunk := append([]byte(nil), queue...)
			queue = queue[:0]
			qMu.Unlock()
			if _, err := player.Write(chunk); err != nil {
				select {
				case <-stop:
				default:
					r.setError(fmt.Errorf("扬声器写入失败: %w", err))
				}
				qMu.Lock()
				closed = true
				qMu.Unlock()
				return
			}
			r.macOutCalls.Add(1)
		}
	}()
	defer func() {
		qMu.Lock()
		closed = true
		qCond.Broadcast()
		qMu.Unlock()
		// Break a blocked write before waiting for the writer.
		player.Close()
		writerW.Wait()
	}()

	stream := newPCMStream()
	phaseLogged := false
	raw := make([]byte, 4096)
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
		if n == 0 {
			continue
		}
		r.modInCalls.Add(1)
		mono := stream.push(raw[:n])
		if !phaseLogged && stream.phase >= 0 {
			phaseLogged = true
			log.Printf("audio: locked PCM sample phase %d (roughness %.2f / %.2f)",
				stream.phase, stream.score0, stream.score1)
		}
		if len(mono) == 0 {
			continue
		}
		peak, rms := levels(mono)
		r.farPeak.max(peak)
		r.farLive.set(rms)
		r.farOutLive.set(peak)
		r.tapRecording(mono, true)

		qMu.Lock()
		if closed {
			qMu.Unlock()
			return
		}
		queue = append(queue, encodePCM(mono)...)
		if len(queue) > maxQ {
			drop := (len(queue) - maxQ) &^ 1
			queue = append(queue[:0], queue[drop:]...)
		}
		r.farRingUsed.Store(int64(len(queue) / 2))
		qCond.Signal()
		qMu.Unlock()
	}
}

// pumpNear moves the microphone into the module. pw-cat delivers 8 kHz
// mono s16 already, so the microphone paces the uplink.
func (r *audioRouter) pumpNear(record io.ReadCloser, stop <-chan struct{}) {
	defer r.wg.Done()
	buf := make([]byte, 320) // 20 ms
	var carry []byte
	samples := make([]float32, 0, len(buf)/2)
	for {
		n, err := record.Read(buf)
		if err != nil {
			select {
			case <-stop:
			default:
				r.setError(fmt.Errorf("麦克风中断: %w", err))
			}
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		r.macInCalls.Add(1)
		data := append(carry, buf[:n]...)
		usable := len(data) &^ 1
		samples = samples[:0]
		for i := 0; i < usable; i += 2 {
			samples = append(samples, float32(int16(binary.LittleEndian.Uint16(data[i:])))/32768)
		}
		carry = append([]byte(nil), data[usable:]...)
		if len(samples) == 0 {
			continue
		}
		if r.muted.Load() {
			for i := range samples {
				samples[i] = 0
			}
		}
		peak, rms := levels(samples)
		r.nearPeak.max(peak)
		r.nearLive.set(rms)
		r.nearOutLive.set(peak)
		r.tapRecording(samples, false)

		if err := r.writePCM(encodePCM(samples)); err != nil {
			select {
			case <-stop:
			default:
				r.setError(fmt.Errorf("模块 PCM 写入失败: %w", err))
			}
			return
		}
		r.modOutCalls.Add(1)
		r.nearRingUse.Store(int64(len(samples)))
	}
}

// limitedWriter keeps the first n bytes of a helper's stderr for diagnostics.
type limitedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
	n  int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
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

// ringtoneCandidates are freedesktop sound-theme files, most preferred first.
var ringtoneCandidates = []string{
	"/usr/share/sounds/freedesktop/stereo/phone-incoming-call.oga",
	"/usr/share/sounds/freedesktop/stereo/bell.oga",
	"/usr/share/sounds/freedesktop/stereo/message-new-instant.oga",
}

func (a *app) registerPlatformAudioRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ringtone", a.ringtoneAPI)
}

func (a *app) ringtoneAPI(w http.ResponseWriter, r *http.Request) {
	for _, path := range ringtoneCandidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		w.Header().Set("Content-Type", "audio/ogg")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
		return
	}
	writeError(w, http.StatusNotFound, "系统中未找到可用的提示音")
}
