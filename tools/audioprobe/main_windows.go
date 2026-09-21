//go:build windows

// Command audioprobe is a Windows-only diagnostic for a module's USB Audio
// Class endpoints. It lists endpoints with the USB path each is wired to, and
// can meter one while writing it to a WAV file — never to the speakers, so a
// live call can be inspected without putting it on the desk.
//
//	go run ./tools/audioprobe                                 # list endpoints
//	go run ./tools/audioprobe -capture "EC20" -out call.wav   # meter + record
//	go run ./tools/audioprobe -tone "EC20" -seconds 5         # play a tone TO the module
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/winaudio"
)

func main() {
	capture := flag.String("capture", "", "substring of a capture endpoint name to meter")
	tone := flag.String("tone", "", "substring of a render endpoint name to play a 440 Hz tone to")
	out := flag.String("out", "", "write the captured audio to this WAV file")
	seconds := flag.Int("seconds", 10, "how long to run")
	flag.Parse()

	runtime.LockOSThread()
	winaudio.InitCOM()

	switch {
	case *capture != "":
		meter(*capture, *seconds, *out)
	case *tone != "":
		playTone(*tone, *seconds)
	default:
		list()
	}
}

func list() {
	for _, spec := range []struct {
		name string
		flow winaudio.Flow
	}{{"CAPTURE", winaudio.Capture}, {"RENDER", winaudio.Render}} {
		devices, err := winaudio.Enumerate(spec.flow)
		if err != nil {
			fmt.Printf("%s: enumerate failed: %v\n", spec.name, err)
			continue
		}
		fmt.Printf("\n== %s ==\n", spec.name)
		for _, d := range devices {
			hw := d.HardwareID
			if hw == "" {
				hw = "(topology unavailable)"
			}
			fmt.Printf("  %-46s  EC20=%v  DJI=%v\n      %s\n",
				d.Name, d.MatchesUSB(0x2c7c, 0x0125), d.MatchesUSB(0x2ca3, 0x4006), hw)
		}
		if def, err := winaudio.Default(spec.flow); err == nil {
			fmt.Printf("  [default] %s\n", def.Name)
		}
	}
}

func find(flow winaudio.Flow, needle string) (winaudio.Device, error) {
	devices, err := winaudio.Enumerate(flow)
	if err != nil {
		return winaudio.Device{}, err
	}
	for _, d := range devices {
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(needle)) {
			return d, nil
		}
	}
	return winaudio.Device{}, fmt.Errorf("no endpoint matching %q", needle)
}

func meter(needle string, seconds int, outPath string) {
	dev, err := find(winaudio.Capture, needle)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	stream, err := winaudio.OpenCapture(dev, 100)
	if err != nil {
		fmt.Printf("open capture %q failed: %v\n", dev.Name, err)
		os.Exit(1)
	}
	defer stream.Close()

	format := stream.Format()
	fmt.Printf("capturing %q  format=%s\n", dev.Name, format)
	fmt.Printf("hardware: %s\n\n", dev.HardwareID)

	var wav *os.File
	var written int
	if outPath != "" {
		wav, err = os.Create(outPath)
		if err != nil {
			fmt.Println("create wav:", err)
			os.Exit(1)
		}
		defer wav.Close()
		wav.Write(make([]byte, 44))
	}

	fmt.Printf("%-7s %-10s %-9s %-9s %s\n", "t", "frames", "rms", "peak", "bar")
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	start := time.Now()
	scratch := make([]float32, 0, 16384)
	total, crossings := 0, 0
	prev := float32(0)
	var sessionPeak float64
	var windowRMS []float64

	for time.Now().Before(deadline) {
		buf, frames, err := stream.Read(scratch[:0], 100)
		if err != nil {
			fmt.Println("read error:", err)
			break
		}
		if frames == 0 {
			continue
		}
		total += frames

		var sum, peak float64
		for _, v := range buf {
			f := float64(v)
			sum += f * f
			if a := math.Abs(f); a > peak {
				peak = a
			}
			if (prev < 0) != (v < 0) {
				crossings++
			}
			prev = v
		}
		rms := math.Sqrt(sum / float64(len(buf)))
		windowRMS = append(windowRMS, rms)
		if peak > sessionPeak {
			sessionPeak = peak
		}
		fmt.Printf("%-7.1f %-10d %-9.5f %-9.5f %s\n",
			time.Since(start).Seconds(), total, rms, peak, strings.Repeat("#", int(peak*60)))

		if wav != nil {
			pcm := make([]byte, 0, len(buf)*2)
			for _, v := range buf {
				s := int16(clamp(v) * 32767)
				pcm = append(pcm, byte(uint16(s)), byte(uint16(s)>>8))
			}
			wav.Write(pcm)
			written += len(buf)
		}
	}

	elapsed := time.Since(start).Seconds()
	fmt.Printf("\nframes=%d  session peak=%.5f  zero-crossing=%.0f Hz  frame rate=%.0f Hz\n",
		total, sessionPeak, float64(crossings)/2/elapsed, float64(total)/elapsed)

	if len(windowRMS) > 1 {
		lo, hi := windowRMS[0], windowRMS[0]
		for _, v := range windowRMS {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		fmt.Printf("RMS range: %.6f .. %.6f  (dynamic range %.0fx)\n", lo, hi, hi/math.Max(lo, 1e-9))
		if hi/math.Max(lo, 1e-9) > 20 {
			fmt.Println("verdict: level varies strongly over time -> looks like real speech")
		} else if hi > 0.001 {
			fmt.Println("verdict: constant level -> a steady tone or noise, NOT call audio")
		} else {
			fmt.Println("verdict: silence -> this endpoint carries nothing")
		}
	}

	if wav != nil {
		finishWAV(wav, format.SampleRate, format.Channels, written/maxInt(format.Channels, 1))
		fmt.Printf("wrote %s\n", outPath)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clamp(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

func finishWAV(f *os.File, rate, channels, frames int) {
	dataBytes := frames * channels * 2
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataBytes))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataBytes))
	f.WriteAt(h, 0)
}

func playTone(needle string, seconds int) {
	dev, err := find(winaudio.Render, needle)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	stream, err := winaudio.OpenRender(dev, 100)
	if err != nil {
		fmt.Printf("open render %q failed: %v\n", dev.Name, err)
		os.Exit(1)
	}
	defer stream.Close()

	format := stream.Format()
	fmt.Printf("playing 440 Hz tone to %q  format=%s\n", dev.Name, format)

	phase := 0.0
	step := 2 * math.Pi * 440 / float64(format.SampleRate)
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	chunk := make([]float32, 0, 4096)
	for time.Now().Before(deadline) {
		free, err := stream.Available()
		if err != nil {
			fmt.Println("available error:", err)
			return
		}
		if free <= 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		chunk = chunk[:0]
		for i := 0; i < free; i++ {
			v := float32(math.Sin(phase) * 0.3)
			phase += step
			if phase > 2*math.Pi {
				phase -= 2 * math.Pi
			}
			for c := 0; c < format.Channels; c++ {
				chunk = append(chunk, v)
			}
		}
		if _, err := stream.Write(chunk, 100); err != nil {
			fmt.Println("write error:", err)
			return
		}
	}
	fmt.Println("done")
}
