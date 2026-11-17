package mixednet

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Expected output from tools/mixednet_ref's detector_harness: the
// firmware's own loading, stop-word arming and process_features_.

// bandModel's probability follows the sum of channels [lo, hi) of its input
// frames, as streamCase's does for the whole frame.
func bandModel(lo, hi int) []byte {
	w := make([]int8, 3*40)
	for f := 0; f < 3; f++ {
		for c := lo; c < hi; c++ {
			w[f*40+c] = 2
		}
	}
	m := &tModel{inputFrames: 3, featureSz: 40, logit: 1,
		bufs:   []tBuf{{frames: 3, channels: 40, push: 3}, {frames: 1, channels: 1}},
		shifts: []int{0},
		ops: []tOp{
			{typ: opRingPush, src: srcExternal, dst: 0, nt: 3, nc: 40, stride: 40},
			{typ: opDense, src: 0, dst: 1, nt: 3, nc: 40, stride: 40, outCh: 1, actMin: -128, actMax: 127,
				weights: w, bias: []int32{0}, mult: []int32{1 << 30}, shift: []int32{-6}},
		},
	}
	for j := range m.sigmoid {
		m.sigmoid[j] = uint8(int8(j)) + 128
	}
	return m.blob()
}

// detectorFeatures: the low and high halves of each frame go quiet and loud
// independently, in stretches of random length.
func detectorFeatures(seed uint64, frames int, exact int8) []int8 {
	r := rand.New(rand.NewPCG(0x64657465, seed))
	out := make([]int8, 0, frames*40)
	lowLeft, highLeft := 0, 0
	var low, high int
	level := func() int {
		if r.IntN(2) == 0 {
			return -100 + r.IntN(50)
		}
		return 60 + r.IntN(60)
	}
	for f := 0; f < frames; f++ {
		if lowLeft == 0 {
			low, lowLeft = level(), 40+r.IntN(200)
		}
		if highLeft == 0 {
			high, highLeft = level(), 40+r.IntN(200)
		}
		lowLeft--
		highLeft--
		for c := 0; c < 40; c++ {
			v := high + r.IntN(31) - 15
			if c < 20 {
				v = low + r.IntN(31) - 15
				if exact != 0 {
					v = int(exact)
				}
			}
			out = append(out, int8(max(-128, min(127, v))))
		}
	}
	return out
}

type detSpec struct {
	cfg  ModelConfig
	blob []byte
}

type detCase struct {
	models       []detSpec
	arm, disarm  []int // frames at which SetStopArmed(true) / (false)
	frames, seed int
	exact        int8 // non-zero: the low band is this level, without noise
}

var (
	wakeHigh = bandModel(20, 40) // the wake words hear the high half
	vadLow   = bandModel(0, 20)  // the VAD hears the low half
)

// vadOn is the probability vadLow gives a constant, noise-free low band of
// level v: the VAD's cutoff for the exact case, so its window sum lands on
// cutoff x window and only the strict comparison decides.
func vadOn(v int8) uint8 {
	m, _ := Parse(vadLow)
	e := NewExecutor(m)
	in := make([]int8, 3*40)
	for f := 0; f < 3; f++ {
		for c := 0; c < 20; c++ {
			in[f*40+c] = v
		}
	}
	return e.Invoke(in)
}

const exactLevel = 40

func detCases() []detCase {
	wake := func(id string, cutoff, window uint8) detSpec {
		return detSpec{ModelConfig{ID: id, Cutoff: cutoff, Window: window, StepMs: 10}, wakeHigh}
	}
	vad := detSpec{ModelConfig{ID: "vad", IsVAD: true, Cutoff: 120, Window: 5, StepMs: 10}, vadLow}
	exactVAD := detSpec{ModelConfig{ID: "vad", IsVAD: true, Cutoff: vadOn(exactLevel), Window: 5, StepMs: 10}, vadLow}
	seven := []detSpec{}
	for i := 0; i < 7; i++ {
		seven = append(seven, wake(fmt.Sprintf("w%d", i), 150, 3))
	}
	seven = append(seven, wake("w2", 200, 2)) // replaces w2 in its slot
	return []detCase{
		{models: []detSpec{wake("okay", 150, 3)}, frames: 4000, seed: 1},
		{models: []detSpec{wake("okay", 150, 3), vad}, frames: 4000, seed: 1}, // case 0's audio, gated
		{models: []detSpec{wake("okay", 150, 3), wake(StopModelID, 170, 2), vad},
			arm: []int{300, 2600}, disarm: []int{1500}, frames: 4000, seed: 3}, // armed twice
		{models: []detSpec{wake(StopModelID, 170, 2), wake("okay", 150, 3)},
			arm: []int{0, 2100}, disarm: []int{2000}, frames: 4000, seed: 4}, // re-armed 100 frames later
		{models: seven, frames: 600, seed: 5},
		{models: []detSpec{wake("okay", 150, 3), exactVAD}, frames: 4000, seed: 6, exact: exactLevel},
	}
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func detRun(t *testing.T, c detCase) []byte {
	d := NewDetector(10)
	var out bytes.Buffer
	for _, m := range c.models {
		if err := d.Load(m.cfg, m.blob); err != nil {
			fmt.Fprintf(&out, "refused %s\n", m.cfg.ID)
		}
	}
	feats := detectorFeatures(uint64(c.seed), c.frames, c.exact)
	for f := 0; f < c.frames; f++ {
		if contains(c.arm, f) {
			d.SetStopArmed(true)
		}
		if contains(c.disarm, f) {
			d.SetStopArmed(false)
		}
		for _, det := range d.Process(feats[f*40 : f*40+40]) {
			fmt.Fprintf(&out, "D %d %d %d %d\n", f+1, det.Index, det.Average, det.Highest)
		}
		fmt.Fprintf(&out, "%d", f+1)
		for i := 0; i < d.Models(); i++ {
			if p, on := d.latest(i); on {
				fmt.Fprintf(&out, " %d", p)
			} else {
				out.WriteString(" -")
			}
		}
		out.WriteByte('\n')
		if (f+1)%7 == 0 {
			fmt.Fprintf(&out, "R %d", f+1)
			for _, r := range d.TakeReport() {
				fmt.Fprintf(&out, " %s %d %d", r.ID, r.Peak, r.Window)
			}
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// normalise keeps refusals (by ID only: the harness also prints the
// reason) and detections verbatim, and folds the per-frame probability
// lines into one FNV-1a hash per 100 frames, to keep the test data small.
func normalise(b []byte) []byte {
	var out bytes.Buffer
	h, n := uint32(2166136261), 0
	for _, line := range strings.SplitAfter(string(b), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "refused "):
			out.WriteString(strings.SplitN(strings.TrimSpace(line), ":", 2)[0] + "\n")
		case strings.HasPrefix(line, "D "):
			out.WriteString(line)
		default:
			for i := 0; i < len(line); i++ {
				h = (h ^ uint32(line[i])) * 16777619
			}
			if n++; n%100 == 0 {
				fmt.Fprintf(&out, "H %d %08x\n", n, h)
			}
		}
	}
	fmt.Fprintf(&out, "H %d %08x\n", n, h)
	return out.Bytes()
}

func detExpected(i int) string {
	return filepath.Join("testdata", fmt.Sprintf("detector_%d.expected", i))
}

func TestDetectorMatchesTheReference(t *testing.T) {
	cases := detCases()
	if *update {
		dir := os.Getenv("MIXEDNET_REF")
		tmp := t.TempDir()
		for i, c := range cases {
			args := []string{filepath.Join(tmp, "feats")}
			fb := make([]byte, 0, c.frames*40)
			for _, v := range detectorFeatures(uint64(c.seed), c.frames, c.exact) {
				fb = append(fb, byte(v))
			}
			os.WriteFile(args[0], fb, 0o644)
			for j, m := range c.models {
				bp := filepath.Join(tmp, fmt.Sprintf("m%d", j))
				os.WriteFile(bp, m.blob, 0o644)
				vad := 0
				if m.cfg.IsVAD {
					vad = 1
				}
				args = append(args, fmt.Sprintf("%s,%d,%d,%d,%s", m.cfg.ID, vad, m.cfg.Cutoff, m.cfg.Window, bp))
			}
			for _, f := range c.arm {
				args = append(args, fmt.Sprintf("arm=%d", f))
			}
			for _, f := range c.disarm {
				args = append(args, fmt.Sprintf("disarm=%d", f))
			}
			out, err := exec.Command(filepath.Join(dir, "detector_harness"), args...).Output()
			if err != nil {
				t.Fatalf("case %d: %v", i, err)
			}
			os.WriteFile(detExpected(i), normalise(out), 0o644)
		}
	}
	counts := map[string]int{}
	for i, c := range cases {
		want, err := os.ReadFile(detExpected(i))
		if err != nil {
			t.Fatal(err)
		}
		raw := detRun(t, c)
		if got := normalise(raw); !bytes.Equal(got, want) {
			t.Errorf("case %d: first difference at line %d", i, firstDiff(got, want))
		}
		counts["frames with a disabled model"] += strings.Count(string(raw), " -")
		for _, line := range strings.Split(string(want), "\n") {
			switch {
			case strings.HasPrefix(line, "D "):
				counts[fmt.Sprintf("detections in case %d", i)]++
			case strings.HasPrefix(line, "refused "):
				counts["refusals"]++
			}
		}
	}
	// What the cases are there to reach must happen.
	for _, k := range []string{"detections in case 0", "detections in case 1", "detections in case 2",
		"detections in case 3", "frames with a disabled model"} {
		if counts[k] == 0 {
			t.Errorf("no %s", k)
		}
	}
	if counts["refusals"] != 1 {
		t.Errorf("%d refusals, want exactly the seventh model", counts["refusals"])
	}
	if counts["detections in case 1"] >= counts["detections in case 0"] {
		t.Errorf("the VAD suppressed nothing (%d with it, %d without)",
			counts["detections in case 1"], counts["detections in case 0"])
	}
	t.Logf("%v", counts)
}

func TestProcessAllocatesNothing(t *testing.T) {
	d := NewDetector(10)
	d.Load(ModelConfig{ID: "okay", Cutoff: 150, Window: 3, StepMs: 10}, wakeHigh)
	d.Load(ModelConfig{ID: "vad", IsVAD: true, Cutoff: 120, Window: 5, StepMs: 10}, vadLow)
	frame := detectorFeatures(1, 1, 0)
	if n := testing.AllocsPerRun(100, func() { d.Process(frame) }); n != 0 {
		t.Errorf("Process allocates %v times per call", n)
	}
}

func TestLoadRefusesWhatTheFirmwareRefuses(t *testing.T) {
	d := NewDetector(10)
	for _, c := range []struct {
		name string
		cfg  ModelConfig
		blob []byte

		want error
	}{
		{"an empty blob", ModelConfig{ID: "a", Cutoff: 1, Window: 1, StepMs: 10}, nil, errModelSize},
		{"another feature step", ModelConfig{ID: "a", Cutoff: 1, Window: 1, StepMs: 20}, wakeHigh, errStep},
		{"a zero window", ModelConfig{ID: "a", Cutoff: 1, Window: 0, StepMs: 10}, wakeHigh, errWindow},
		{"a blob too large", ModelConfig{ID: "a", Cutoff: 1, Window: 1, StepMs: 10}, make([]byte, MaxModelBytes+1), errModelSize},
		{"a blob that does not parse", ModelConfig{ID: "a", Cutoff: 1, Window: 1, StepMs: 10}, []byte("MNB1"), errTruncatedHeader},
	} {
		if err := d.Load(c.cfg, c.blob); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if d.Models() != 0 {
		t.Errorf("a refused model was kept")
	}
}

// latest is model i's most recent probability, and whether it is enabled
// (for the parity test).
func (d *Detector) latest(i int) (uint8, bool) {
	m := d.models[i]
	return m.stream.Latest(), m.enabled
}
