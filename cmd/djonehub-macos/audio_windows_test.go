//go:build windows

package main

import (
	"encoding/binary"
	"math"
	"testing"
)

// speechLikePCM builds a smooth, voiced-sounding waveform as little-endian
// 16-bit samples: a 300 Hz tone with a slow amplitude envelope, which has the
// same "heavily oversampled at 8 kHz" character as narrowband call audio.
func speechLikePCM(samples int) []byte {
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		t := float64(i) / modulePCMRate
		env := 0.4 + 0.5*math.Sin(2*math.Pi*3*t)
		v := env * math.Sin(2*math.Pi*300*t) * 20000
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(v)))
	}
	return buf
}

func drain(t *testing.T, s *pcmStream, raw []byte, chunk int) []float32 {
	t.Helper()
	var out []float32
	for off := 0; off < len(raw); off += chunk {
		end := off + chunk
		if end > len(raw) {
			end = len(raw)
		}
		out = append(out, s.push(raw[off:end])...)
	}
	return out
}

// roughness mirrors pcmRoughness for decoded float samples: smooth audio scores
// well below 1, decorrelated noise scores around 1.
func roughness(samples []float32) float64 {
	if len(samples) < 2 {
		return math.Inf(1)
	}
	var sumSq, sumDiff float64
	prev := float64(samples[0])
	sumSq = prev * prev
	for _, s := range samples[1:] {
		v := float64(s)
		sumSq += v * v
		sumDiff += math.Abs(v - prev)
		prev = v
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	if rms == 0 {
		return math.Inf(1)
	}
	return sumDiff / float64(len(samples)-1) / rms
}

func TestPCMStreamKeepsAlignedStream(t *testing.T) {
	raw := speechLikePCM(8000)
	got := drain(t, newPCMStream(), raw, 512)
	if len(got) == 0 {
		t.Fatal("no samples decoded")
	}
	if r := roughness(got); r > 0.5 {
		t.Fatalf("aligned stream decoded as noise: roughness %.3f", r)
	}
}

// TestPCMStreamRecoversOddAlignment is the regression this type exists for: the
// module is already streaming when the port is opened, so the first byte read
// can land in the middle of a sample. One byte out of phase preserves the
// timing and the envelope while turning the waveform into noise, which sounds
// like a call with the rhythm of speech and none of the speech.
func TestPCMStreamRecoversOddAlignment(t *testing.T) {
	raw := append([]byte{0x7f}, speechLikePCM(8000)...)

	naive := make([]float32, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		naive = append(naive, float32(int16(binary.LittleEndian.Uint16(raw[i:])))/32768)
	}
	naiveRoughness := roughness(naive)
	if naiveRoughness < 0.5 {
		t.Fatalf("test data is not actually misaligned: roughness %.3f", naiveRoughness)
	}

	stream := newPCMStream()
	got := drain(t, stream, raw, 512)
	if len(got) == 0 {
		t.Fatal("no samples decoded")
	}
	if stream.phase != 1 {
		t.Fatalf("expected phase 1 to be selected, got %d", stream.phase)
	}
	if r := roughness(got); r > 0.5 {
		t.Fatalf("misaligned stream not recovered: roughness %.3f (naive %.3f)", r, naiveRoughness)
	}
}

func TestPCMStreamHoldsPhaseUntilThereIsSignal(t *testing.T) {
	silence := make([]byte, pcmWarmupBytes*2)
	stream := newPCMStream()
	if out := drain(t, stream, silence, 512); len(out) != 0 {
		t.Fatalf("silence should not be emitted while undecided, got %d samples", len(out))
	}
	if stream.phase != -1 {
		t.Fatalf("phase should stay undecided on silence, got %d", stream.phase)
	}
	// The buffered window must not grow without bound while waiting.
	if len(stream.warm) > pcmWarmupBytes {
		t.Fatalf("warm-up buffer grew to %d bytes", len(stream.warm))
	}
	// Once audio arrives the phase locks and samples flow.
	if out := drain(t, stream, speechLikePCM(4000), 512); len(out) == 0 {
		t.Fatal("no samples after signal arrived")
	}
	if stream.phase != 0 {
		t.Fatalf("expected phase 0 on aligned signal, got %d", stream.phase)
	}
}

func TestPCMStreamSplitsSampleAcrossReads(t *testing.T) {
	raw := speechLikePCM(8000)
	// Odd-sized chunks force a sample to straddle two reads on every boundary.
	got := drain(t, newPCMStream(), raw, 511)
	if r := roughness(got); r > 0.5 {
		t.Fatalf("odd-sized reads corrupted the stream: roughness %.3f", r)
	}
}

// quietNoisePCM is the line noise that fills the gaps between spoken phrases:
// low level and, being noise, equally rough at either sample alignment.
func quietNoisePCM(samples int, amplitude int) []byte {
	buf := make([]byte, samples*2)
	state := uint32(0x12345678)
	for i := 0; i < samples; i++ {
		state = state*1664525 + 1013904223
		v := int16(int32(state>>16)%int32(2*amplitude) - int32(amplitude))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return buf
}

// TestPCMStreamIgnoresQuietNoise is the regression for a call that alternated
// between clear audio and noise between runs. The phase was decided from the
// first window that carried any level at all, and the gaps between IVR phrases
// carry line noise at roughly -43 dBFS. Noise scores the same at both
// alignments, so those windows picked a winner essentially at random and the
// choice then held for the whole call.
func TestPCMStreamIgnoresQuietNoise(t *testing.T) {
	stream := newPCMStream()
	if out := drain(t, stream, quietNoisePCM(8000, 300), 512); len(out) != 0 {
		t.Fatalf("line noise must not be decoded before the phase is known, got %d samples", len(out))
	}
	if stream.phase != -1 {
		t.Fatalf("line noise decided the phase (%d): scores %.2f / %.2f",
			stream.phase, stream.score0, stream.score1)
	}
	// Real audio still locks the phase.
	if out := drain(t, stream, speechLikePCM(8000), 512); len(out) == 0 {
		t.Fatal("no samples once real audio arrived")
	}
	if stream.phase != 0 {
		t.Fatalf("expected phase 0 on aligned audio, got %d", stream.phase)
	}
}

// TestPCMStreamNeedsAClearWinner keeps the detector from acting on a window
// where the two alignments score alike.
func TestPCMStreamNeedsAClearWinner(t *testing.T) {
	// Loud noise passes the level gate but says nothing about alignment.
	phase, s0, s1 := choosePCMPhase(quietNoisePCM(4000, 12000))
	if phase != -1 {
		t.Fatalf("loud noise should not decide the phase, got %d (%.2f / %.2f)", phase, s0, s1)
	}
}
