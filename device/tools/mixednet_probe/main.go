// mixednet_probe answers on an Echo what the host tests cannot: do the
// Voice Incubator wake word front end (internal/microfeatures) and model
// stream (internal/mixednet) give the same answers on the real 32-bit ARM
// target, and what do they cost in the shape they would run?
//
// It touches no microphone, speaker, LEDs or mixer, and leaves the server
// running: it reads one model blob (and optionally a PCM file) and prints.
//
//	Phase 1, correctness: seeded audio through the front end and one stream;
//	prints a hash of every feature frame and stream output, which -expect
//	compares with the host's (exit 1 on a mismatch). -pcm runs a file too.
//	Phase 2, cost: the same chain paced in real time, 80 ms of audio a tick
//	as the scorer receives it, with 1, 2 and 4 streams; reports process CPU
//	from getrusage as a share of one core, and the longest tick.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/wilbowes/EchoMuse/internal/microfeatures"
	"github.com/wilbowes/EchoMuse/internal/mixednet"
)

// probeAudio is 10 s of seeded audio: noise whose level ramps up and down
// three times, with a triangle tone sweeping the band, so every channel and
// the front end's noise and gain adaptation are exercised. Integer
// arithmetic only: Go's float math (Pow, Sin, NormFloat64) rounds
// differently on 32-bit ARM, and the input must be the same bytes on the
// Echo as on the host for the hashes to compare the front end at all.
func probeAudio() []int16 {
	r := rand.New(rand.NewPCG(0x70726f62, 1))
	n := 10 * microfeatures.SampleRate
	out := make([]int16, n)
	var phase uint32 // 2^32 per cycle
	for i := range out {
		// 100 Hz to 7.1 kHz: step = f * 2^32 / 16000
		f := uint64(100 + 7000*i/n)
		phase += uint32(f << 32 / microfeatures.SampleRate)
		tri := int32(phase>>16) - 32768 // sawtooth, -32768..32767
		if tri < 0 {
			tri = -tri
		}
		tri = 2*tri - 32768 // triangle
		// level: 0..1023, three ramps up and down over the clip
		pos := (i * 6 / (n / 1024)) % 2048
		level := int32(pos)
		if pos >= 1024 {
			level = int32(2047 - pos)
		}
		level = 8 + level*level/256 // 8..4100, a steep rise
		noise := int32(r.Uint32()>>17) - 16384
		v := (int64(noise)*int64(level))>>12 + (int64(tri)*int64(level))>>14
		out[i] = int16(max(-32768, min(32767, v)))
	}
	return out
}

type result struct {
	hash       uint32
	frames     int
	detections []int
	report     []string
}

// run feeds pcm through a fresh front end and stream, hashing every frame
// and every stream output (FNV-1a).
func run(m *mixednet.Model, pcm []int16, cutoff, window uint8) (result, error) {
	fe, err := microfeatures.New(microfeatures.DefaultStepMs)
	if err != nil {
		return result{}, err
	}
	defer fe.Close()
	s, err := mixednet.NewStream(m, cutoff, window)
	if err != nil {
		return result{}, err
	}
	res := result{hash: 2166136261}
	mix := func(b byte) { res.hash = (res.hash ^ uint32(b)) * 16777619 }
	fe.Feed(pcm, func(fr *[microfeatures.FeatureSize]int8) {
		res.frames++
		for _, v := range fr {
			mix(byte(v))
		}
		det, avg, top := s.Step(fr[:])
		mix(s.Latest())
		if det {
			mix(1)
			mix(avg)
			mix(top)
			res.detections = append(res.detections, res.frames)
			res.report = append(res.report, fmt.Sprintf("%d/%d", avg, top))
		}
	})
	return res, nil
}

// trace prints, per second of audio (100 frames), a hash of the front
// end's frames alone and of the stream's outputs alone, and with -frame the
// 40 values of one frame: what locates a difference between two machines.
func trace(m *mixednet.Model, pcm []int16, cutoff, window uint8, frame int) {
	fe, _ := microfeatures.New(microfeatures.DefaultStepMs)
	defer fe.Close()
	s, _ := mixednet.NewStream(m, cutoff, window)
	n := 0
	hf, hs := uint32(2166136261), uint32(2166136261)
	fe.Feed(pcm, func(fr *[microfeatures.FeatureSize]int8) {
		n++
		for _, v := range fr {
			hf = (hf ^ uint32(uint8(v))) * 16777619
		}
		s.Step(fr[:])
		hs = (hs ^ uint32(s.Latest())) * 16777619
		if n == frame {
			fmt.Printf("frame %d: %v\n", n, fr[:])
		}
		if frame < 0 && n <= -frame {
			h := uint32(2166136261)
			for _, v := range fr {
				h = (h ^ uint32(uint8(v))) * 16777619
			}
			fmt.Printf("fh %d %08x\n", n, h)
		}
		if n%100 == 0 {
			fmt.Printf("second %d: frames %08x stream %08x\n", n/100, hf, hs)
		}
	})
}

func cost(m *mixednet.Model, pcm []int16, streams int, seconds int, cutoff, window uint8) (pct float64, worst time.Duration, late int, err error) {
	fe, err := microfeatures.New(microfeatures.DefaultStepMs)
	if err != nil {
		return 0, 0, 0, err
	}
	defer fe.Close()
	ss := make([]*mixednet.Stream, streams)
	for i := range ss {
		if ss[i], err = mixednet.NewStream(m, cutoff, window); err != nil {
			return 0, 0, 0, err
		}
	}
	emit := func(fr *[microfeatures.FeatureSize]int8) {
		for _, s := range ss {
			s.Step(fr[:])
		}
	}
	const tick = 80 * time.Millisecond
	chunk := microfeatures.SampleRate * 80 / 1000
	ticks := seconds * 1000 / 80
	var ru0, ru1 syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru0); err != nil {
		return 0, 0, 0, err
	}
	start := time.Now()
	for i := 0; i < ticks; i++ {
		off := (i * chunk) % (len(pcm) - chunk)
		t0 := time.Now()
		fe.Feed(pcm[off:off+chunk], emit)
		if d := time.Since(t0); d > worst {
			worst = d
		}
		next := start.Add(time.Duration(i+1) * tick)
		if wait := time.Until(next); wait > 0 {
			time.Sleep(wait)
		} else {
			late++
		}
	}
	wall := time.Since(start)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru1); err != nil {
		return 0, 0, 0, err
	}
	tv := func(t syscall.Timeval) time.Duration { return time.Duration(t.Nano()) }
	used := tv(ru1.Utime) + tv(ru1.Stime) - tv(ru0.Utime) - tv(ru0.Stime)
	return 100 * used.Seconds() / wall.Seconds(), worst, late, nil
}

func readPCM(path string) ([]int16, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 44 && string(raw[:4]) == "RIFF" {
		raw = raw[44:] // a canonical 16 kHz mono s16le WAV header
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return pcm, nil
}

func conditions() string {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return "?"
		}
		return strings.TrimSpace(string(b))
	}
	return fmt.Sprintf("cpus online %s, cpu0 %s kHz, thermal zone0 %s", read("/sys/devices/system/cpu/online"),
		read("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq"), read("/sys/class/thermal/thermal_zone0/temp"))
}

func main() {
	blobPath := flag.String("blob", "okay_nabu.blob", "MNB1 model blob")
	cutoffF := flag.Float64("cutoff", 0.85, "the model's probability cutoff (0-1)")
	window := flag.Uint("window", 5, "the model's sliding window")
	expect := flag.String("expect", "", "phase 1 hash to match (from a host run)")
	pcmPath := flag.String("pcm", "", "also run this 16 kHz mono s16le file (WAV or raw)")
	seconds := flag.Int("seconds", 30, "phase 2 duration per stream count")
	skipCost := flag.Bool("no-cost", false, "phase 1 only")
	traceOn := flag.Bool("trace", false, "print per-second front end and stream hashes, then exit")
	traceFrame := flag.Int("frame", 0, "with -trace, also print this frame's 40 values")
	flag.Parse()

	blob, err := os.ReadFile(*blobPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	m, err := mixednet.Parse(blob)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(2)
	}
	cutoff := mixednet.CutoffByte(float32(*cutoffF))
	fmt.Printf("model %s: arena %d, cutoff %d, window %d\n", *blobPath, m.ArenaSize(), cutoff, *window)

	audio := probeAudio()
	if *traceOn {
		ha := uint32(2166136261)
		for _, v := range audio {
			ha = (ha ^ uint32(uint16(v))&0xff) * 16777619
			ha = (ha ^ uint32(uint16(v))>>8) * 16777619
		}
		fmt.Printf("audio %08x\n", ha)
		trace(m, audio, cutoff, uint8(*window), *traceFrame)
		return
	}
	res, err := run(m, audio, cutoff, uint8(*window))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	hash := fmt.Sprintf("%08x", res.hash)
	fmt.Printf("phase 1: %d frames, detections %v, hash %s\n", res.frames, res.detections, hash)
	status := 0
	if *expect != "" {
		if hash == *expect {
			fmt.Println("phase 1: MATCHES the host")
		} else {
			fmt.Printf("phase 1: DIFFERS from the host (%s)\n", *expect)
			status = 1
		}
	}
	if *pcmPath != "" {
		pcm, err := readPCM(*pcmPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		r, _ := run(m, pcm, cutoff, uint8(*window))
		fmt.Printf("pcm %s: %d frames, detections at %v, reports %v, hash %08x\n",
			*pcmPath, r.frames, r.detections, r.report, r.hash)
	}
	if !*skipCost {
		fmt.Println("phase 2:", conditions())
		for _, n := range []int{1, 2, 4} {
			pct, worst, late, err := cost(m, audio, n, *seconds, cutoff, uint8(*window))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			fmt.Printf("phase 2: %d stream(s): %.2f%% of one core, longest tick %v, %d late ticks\n",
				n, pct, worst.Round(10*time.Microsecond), late)
		}
	}
	os.Exit(status)
}
