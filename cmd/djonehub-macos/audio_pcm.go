//go:build windows || linux

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Shared by the Windows and Linux routers: both carry call audio over Quectel's
// Voice over USB (AT+QPCMV=1,0), a fixed 8 kHz / 16-bit / mono PCM stream on the
// module's NMEA interface, and differ only in how they reach this machine's
// microphone and speakers.

// modulePCM is the fixed stream format of Quectel's Voice over USB.
const (
	modulePCMRate     = 8000
	modulePCMChannels = 1
)

// farLatencyBudget caps how much decoded downlink may wait for the speakers.
//
// The module starts streaming when the route is armed, which is before the call
// is answered, so by the time the audio path opens the driver is holding
// whatever accumulated in between. Queuing that backlog would play it out in
// real time and leave the delay in place for the rest of the call — the module
// produces exactly as fast as the speakers consume, so nothing ever catches up.
// Dropping the oldest audio instead costs one glitch and keeps the conversation
// in sync.
const farLatencyBudget = 120 * time.Millisecond

// atomicFloat stores a float64 in an atomic uint64 so meters can be read from
// HTTP handlers while the pumps keep writing.
type atomicFloat struct{ bits atomic.Uint64 }

func (a *atomicFloat) set(v float64) { a.bits.Store(math.Float64bits(v)) }
func (a *atomicFloat) get() float64  { return math.Float64frombits(a.bits.Load()) }
func (a *atomicFloat) max(v float64) {
	for {
		old := a.bits.Load()
		if math.Float64frombits(old) >= v {
			return
		}
		if a.bits.CompareAndSwap(old, math.Float64bits(v)) {
			return
		}
	}
}

// pcmStream turns the module's raw byte stream into 8 kHz mono samples.
//
// Nothing in the transport marks where a 16-bit sample begins, and the stream
// is already running by the time the port is opened, so reading from an odd
// offset is a coin flip. One byte out of phase keeps the timing and the
// envelope intact but decorrelates the waveform — the call still has the rhythm
// of speech while being pure noise. The phase is therefore measured from the
// audio itself rather than assumed.
type pcmStream struct {
	phase     int // -1 until enough signal has arrived to decide
	warm      []byte
	undecided int // bytes seen while still waiting for something to measure
	// score0/score1 record the last roughness measurement, for logging.
	score0, score1 float64
	carry          []byte
	samples        []float32
}

const (
	// 200 ms is long enough to hold several voiced segments at 8 kHz.
	pcmWarmupBytes = 3200
	// Give up and take phase 0 if the line stays quiet this long: with nothing
	// to measure, silence sounds the same either way.
	pcmWarmupLimit = 8000 * 2 * 6

	// pcmPhaseMinRMS is the level below which a window must not be used to
	// decide. The gaps between spoken phrases still carry line noise at roughly
	// -43 dBFS, and noise is by definition equally rough at either alignment —
	// measured across a real call, windows that quiet picked the wrong phase
	// about one time in six, while windows above this threshold separated the
	// two alignments by 0.2 against 1.1 every single time.
	pcmPhaseMinRMS = 2000
	// pcmPhaseMaxRough is the most a genuinely aligned window may score.
	pcmPhaseMaxRough = 0.6
	// pcmPhaseMargin is how much better the winner has to be than the loser.
	// Two similar scores mean the window says nothing, not that the lower one
	// is right.
	pcmPhaseMargin = 0.6
)

func newPCMStream() *pcmStream { return &pcmStream{phase: -1} }

// push decodes one read from the module. While the phase is still unknown it
// buffers instead of emitting; once decided, the buffered window is decoded
// too, which both avoids dropping the first moment of the call and — more
// importantly — carries the chosen alignment forward. Discarding the window
// instead would re-introduce the very bug this type exists for, because the
// next read would resume at whatever parity the discarded bytes happened to
// leave behind.
func (p *pcmStream) push(raw []byte) []float32 {
	p.samples = p.samples[:0]
	if p.phase >= 0 {
		return p.decode(raw)
	}

	p.warm = append(p.warm, raw...)
	p.undecided += len(raw)
	if len(p.warm) < pcmWarmupBytes {
		return p.samples
	}

	phase, s0, s1 := choosePCMPhase(p.warm)
	p.score0, p.score1 = s0, s1
	if phase < 0 {
		if p.undecided < pcmWarmupLimit {
			// Still silent. Keep only a recent window, dropping an even number
			// of bytes so the retained bytes sit on the same sample grid.
			if drop := (len(p.warm) - pcmWarmupBytes) &^ 1; drop > 0 {
				p.warm = append(p.warm[:0], p.warm[drop:]...)
			}
			return p.samples
		}
		// The line has been quiet long enough that waiting no longer helps.
		phase = 0
	}

	p.phase = phase
	warm := p.warm[phase:]
	p.warm = nil
	return p.decode(warm)
}

func (p *pcmStream) decode(raw []byte) []float32 {
	buf := raw
	if len(p.carry) > 0 {
		// A fresh buffer: p.carry is rewritten below and must not alias buf.
		buf = append(append(make([]byte, 0, len(p.carry)+len(raw)), p.carry...), raw...)
	}
	usable := len(buf) - len(buf)%2
	for i := 0; i < usable; i += 2 {
		p.samples = append(p.samples, float32(int16(binary.LittleEndian.Uint16(buf[i:])))/32768)
	}
	p.carry = append(p.carry[:0], buf[usable:]...)
	return p.samples
}

// choosePCMPhase picks the 16-bit alignment that yields the smoother waveform,
// or -1 when the window does not answer the question clearly enough. Refusing
// to decide is always safe: the caller keeps listening. Deciding from a weak
// window is not, because the choice is made once and holds for the whole call.
func choosePCMPhase(buf []byte) (int, float64, float64) {
	s0, ok0 := pcmRoughness(buf)
	s1, ok1 := pcmRoughness(buf[1:])
	if !ok0 || !ok1 {
		return -1, s0, s1
	}
	best, other := 0, 1
	if s1 < s0 {
		best, other = 1, 0
	}
	scores := [2]float64{s0, s1}
	if scores[best] > pcmPhaseMaxRough || scores[best] > scores[other]*pcmPhaseMargin {
		return -1, s0, s1
	}
	return best, s0, s1
}

// pcmRoughness is the mean absolute difference between consecutive samples
// divided by their RMS. Narrowband speech is heavily oversampled at 8 kHz and
// scores around 0.2; the same bytes read one offset over decorrelate into noise
// and score near 1. ok is false when the window carries no measurable signal.
func pcmRoughness(buf []byte) (float64, bool) {
	n := len(buf) / 2
	if n < 2 {
		return 0, false
	}
	prev := float64(int16(binary.LittleEndian.Uint16(buf)))
	sumSq := prev * prev
	var sumDiff float64
	for i := 1; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(buf[i*2:])))
		sumSq += v * v
		sumDiff += math.Abs(v - prev)
		prev = v
	}
	rms := math.Sqrt(sumSq / float64(n))
	if rms < pcmPhaseMinRMS {
		return 0, false
	}
	return sumDiff / float64(n-1) / rms, true
}

func clampUnit(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

func levels(samples []float32) (peak float64, rms float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range samples {
		f := float64(v)
		sum += f * f
		if a := math.Abs(f); a > peak {
			peak = a
		}
	}
	return peak, math.Sqrt(sum / float64(len(samples)))
}

// converter turns one side's interleaved frames into the other's, keeping the
// fractional resampler position between calls so successive packets join
// without a click.
type converter struct {
	inRate, inCh   int
	outRate, outCh int
	pos            float64
	last           float32
	mono           []float32
	out            []float32
}

func newConverter(inRate, inCh, outRate, outCh int) *converter {
	return &converter{inRate: inRate, inCh: inCh, outRate: outRate, outCh: outCh}
}

// toMono averages the interleaved input channels into a scratch buffer.
func (c *converter) toMono(src []float32) []float32 {
	if c.inCh <= 1 {
		c.mono = append(c.mono[:0], src...)
		return c.mono
	}
	frames := len(src) / c.inCh
	c.mono = c.mono[:0]
	for i := 0; i < frames; i++ {
		var sum float32
		for j := 0; j < c.inCh; j++ {
			sum += src[i*c.inCh+j]
		}
		c.mono = append(c.mono, sum/float32(c.inCh))
	}
	return c.mono
}

// convert resamples mono input to the output rate and fans it out to the output
// channel count. Downsampling averages across the source window so a 48 kHz
// microphone does not alias into the module's 8 kHz uplink; upsampling uses
// linear interpolation, which is adequate for narrowband voice.
func (c *converter) convert(mono []float32) []float32 {
	c.out = c.out[:0]
	if len(mono) == 0 {
		return c.out
	}
	ratio := float64(c.inRate) / float64(c.outRate)

	switch {
	case c.inRate == c.outRate:
		for _, v := range mono {
			c.emit(v)
		}
	case ratio > 1: // downsample: box-average each source window
		for c.pos < float64(len(mono)) {
			start := int(c.pos)
			end := int(c.pos + ratio)
			if end > len(mono) {
				end = len(mono)
			}
			if start >= end {
				start = end - 1
			}
			if start < 0 {
				break
			}
			var sum float32
			for i := start; i < end; i++ {
				sum += mono[i]
			}
			c.emit(sum / float32(end-start))
			c.pos += ratio
		}
		c.pos -= float64(len(mono))
	default: // upsample: linear interpolation against the previous tail sample
		for c.pos < float64(len(mono)) {
			i := int(c.pos)
			frac := float32(c.pos - float64(i))
			a := c.last
			if i > 0 {
				a = mono[i-1]
			}
			c.emit(a + (mono[i]-a)*frac)
			c.pos += ratio
		}
		c.pos -= float64(len(mono))
	}
	if c.pos < 0 {
		c.pos = 0
	}
	c.last = mono[len(mono)-1]
	return c.out
}

func (c *converter) emit(v float32) {
	for i := 0; i < c.outCh; i++ {
		c.out = append(c.out, v)
	}
}

// ---- recording ----

type sampleRing struct {
	mu   sync.Mutex
	buf  []float32
	size int
}

func newSampleRing(size int) *sampleRing { return &sampleRing{size: size} }

func (s *sampleRing) push(samples []float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, samples...)
	if len(s.buf) > s.size {
		s.buf = append(s.buf[:0], s.buf[len(s.buf)-s.size:]...)
	}
}

func (s *sampleRing) take(n int) []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]float32, n)
	got := n
	if got > len(s.buf) {
		got = len(s.buf)
	}
	copy(out, s.buf[:got])
	s.buf = append(s.buf[:0], s.buf[got:]...)
	return out
}

// tapRecording writes the live call into the WAV file when one is open. Both
// directions are already at the module's 8 kHz rate here, so the far direction
// can drive the file clock while near samples come from a short ring.
func (r *audioRouter) tapRecording(samples []float32, isFar bool) {
	if !isFar {
		r.nearTap.push(samples)
		return
	}
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if r.rec == nil {
		return
	}
	r.rec.WriteStereo(samples, r.nearTap.take(len(samples)))
}

func (r *audioRouter) startRecording() (string, error) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if r.rec != nil {
		return r.recPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "DJOneHub", "recordings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("call-%s.wav", time.Now().Format("20060102-150405")))
	writer, err := newWAVWriter(path, modulePCMRate, 2)
	if err != nil {
		return "", err
	}
	r.rec, r.recPath = writer, path
	return path, nil
}

func (r *audioRouter) stopRecording() (string, error) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if r.rec == nil {
		return r.recPath, nil
	}
	err := r.rec.Close()
	r.rec = nil
	return r.recPath, err
}

func (r *audioRouter) isRecording() bool {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	return r.rec != nil
}

func (r *audioRouter) recordingPath() string {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	return r.recPath
}

// wavWriter streams 16-bit PCM and fixes the RIFF sizes on close.
type wavWriter struct {
	file     *os.File
	channels int
	rate     int
	frames   int
}

func newWAVWriter(path string, rate, channels int) (*wavWriter, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(make([]byte, 44)); err != nil {
		file.Close()
		return nil, err
	}
	return &wavWriter{file: file, channels: channels, rate: rate}, nil
}

func (w *wavWriter) WriteStereo(left, right []float32) {
	if w == nil || w.file == nil {
		return
	}
	buf := make([]byte, len(left)*4)
	for i := range left {
		var rv float32
		if i < len(right) {
			rv = right[i]
		}
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(int16(clampUnit(left[i])*32767)))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(int16(clampUnit(rv)*32767)))
	}
	if _, err := w.file.Write(buf); err == nil {
		w.frames += len(left)
	}
}

func (w *wavWriter) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	dataBytes := w.frames * w.channels * 2
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataBytes))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(w.channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(w.rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(w.rate*w.channels*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(w.channels*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataBytes))
	if _, err := w.file.WriteAt(h, 0); err != nil {
		w.file.Close()
		w.file = nil
		return err
	}
	err := w.file.Close()
	w.file = nil
	return err
}
