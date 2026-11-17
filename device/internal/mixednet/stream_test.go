package mixednet

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Expected per-frame output from tools/mixednet_ref's stream_harness: the
// firmware's own ModelStream, cut verbatim from wake_detector.{h,cpp}.

var streamCases = []struct{ cutoff, window uint8 }{
	{100, 1}, {128, 3}, {160, 5}, {200, 2}, {60, 8}, {250, 3},
	{188, 1}, // the exact case below: probability equal to the cutoff
}

// exactCase is the last case: silence alternating with a level whose
// probability is exactly 188, the case's cutoff, with no noise, so the
// strict comparisons (sum > cutoff x window, probability < cutoff) are held.
var exactCase = len(streamCases) - 1

const streamFrames = 900

// streamCase is a model whose probability follows its input, fed quiet and
// loud stretches of random length, so detections, the mute window and its
// countdown all happen: one dense layer sums the three input frames into the
// logit, and the probability table is the identity (logit + 128).
func streamCase(i int) (blob []byte, feats []int8) {
	r := rand.New(rand.NewPCG(0x73747265, uint64(i)))
	ones := make([]int8, 3*40)
	for j := range ones {
		ones[j] = 1
	}
	m := &tModel{inputFrames: 3, featureSz: 40, logit: 1,
		bufs:   []tBuf{{frames: 3, channels: 40, push: 3}, {frames: 1, channels: 1}},
		shifts: []int{0},
		ops: []tOp{
			{typ: opRingPush, src: srcExternal, dst: 0, nt: 3, nc: 40, stride: 40},
			{typ: opDense, src: 0, dst: 1, nt: 3, nc: 40, stride: 40, outCh: 1, actMin: -128, actMax: 127,
				weights: ones, bias: []int32{0}, mult: []int32{1 << 30}, shift: []int32{-6}},
		},
	}
	for j := range m.sigmoid {
		m.sigmoid[j] = uint8(int8(j)) + 128
	}
	if i == exactCase {
		for len(feats) < streamFrames*40 {
			for _, level := range []int8{-128, 64} { // p = 8, then exactly 188
				for n := 0; n < 150*40; n++ {
					feats = append(feats, level)
				}
			}
		}
		return m.blob(), feats[:streamFrames*40]
	}
	for len(feats) < streamFrames*40 {
		level := -100 + r.IntN(60) // quiet
		if r.IntN(2) == 0 {
			level = 60 + r.IntN(67) // loud
		}
		for n := 30 + r.IntN(250); n > 0; n-- {
			for c := 0; c < 40; c++ {
				feats = append(feats, int8(max(-128, min(127, level+r.IntN(41)-20))))
			}
		}
	}
	return m.blob(), feats[:streamFrames*40]
}

func streamRun(t *testing.T, i int) []byte {
	blob, feats := streamCase(i)
	m, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	c := streamCases[i]
	s, err := NewStream(m, c.cutoff, c.window)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n := m.FeatureSize()
	for off := 0; off+n <= len(feats); off += n {
		det, avg, top := s.Step(feats[off : off+n])
		fmt.Fprintf(&out, "%d %d %d %d %d\n", s.Latest(), s.ignore, b2i(det), avg, top)
	}
	return out.Bytes()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func streamExpected(i int) string {
	return filepath.Join("testdata", fmt.Sprintf("stream_%d.expected", i))
}

func TestStreamMatchesTheReference(t *testing.T) {
	if *update {
		dir := os.Getenv("MIXEDNET_REF")
		tmp := t.TempDir()
		for i, c := range streamCases {
			blob, feats := streamCase(i)
			fb := make([]byte, len(feats))
			for j, v := range feats {
				fb[j] = byte(v)
			}
			bp, fp := filepath.Join(tmp, "blob"), filepath.Join(tmp, "feats")
			os.WriteFile(bp, blob, 0o644)
			os.WriteFile(fp, fb, 0o644)
			out, err := exec.Command(filepath.Join(dir, "stream_harness"), bp, fp,
				fmt.Sprint(c.cutoff), fmt.Sprint(c.window)).Output()
			if err != nil {
				t.Fatalf("case %d: %v", i, err)
			}
			os.WriteFile(streamExpected(i), out, 0o644)
		}
	}
	detections, muted := 0, 0
	for i := range streamCases {
		want, err := os.ReadFile(streamExpected(i))
		if err != nil {
			t.Fatal(err)
		}
		if got := streamRun(t, i); !bytes.Equal(got, want) {
			t.Errorf("case %d: first difference at frame %d", i, firstDiff(got, want))
		}
		for _, line := range strings.Split(strings.TrimSpace(string(want)), "\n") {
			f := strings.Fields(line)
			if f[2] == "1" {
				detections++
			}
			if f[1] != "0" {
				muted++
			}
		}
	}
	// Matching proves little unless detections and mute windows happen.
	if detections < 5 || muted < 500 {
		t.Errorf("only %d detections and %d muted frames across the cases", detections, muted)
	}
}

func TestStepAllocatesNothing(t *testing.T) {
	blob, feats := streamCase(0)
	m, _ := Parse(blob)
	s, _ := NewStream(m, 128, 3)
	frame := feats[:m.FeatureSize()]
	if n := testing.AllocsPerRun(100, func() { s.Step(frame) }); n != 0 {
		t.Errorf("Step allocates %v times per call", n)
	}
}

func TestNewStreamRefusesFramesThatAreNot40Wide(t *testing.T) {
	m := synthModel(rand.New(rand.NewPCG(9, 9)))
	m.featureSz = 39
	m.ops[0].nc = 39 // the input push, so the blob itself is valid
	model, err := Parse(m.blob())
	if err != nil {
		t.Skipf("the edited model does not parse: %v", err)
	}
	if _, err := NewStream(model, 128, 3); err == nil {
		t.Fatal("accepted a 39-wide model")
	}
}

func TestCutoffByte(t *testing.T) {
	nan := float32(math.NaN())
	for _, c := range []struct {
		in   float32
		want uint8
	}{{0.85, 217}, {0, 0}, {1, 255}, {-0.5, 0}, {2, 255}, {0.5, 128}, {nan, 0}, {float32(math.Inf(1)), 255}} {
		if got := CutoffByte(c.in); got != c.want {
			t.Errorf("CutoffByte(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
