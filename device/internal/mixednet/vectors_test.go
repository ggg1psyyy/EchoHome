package mixednet

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The expected values in testdata/ come from the Voice Incubator firmware's
// own C++ executor, never from this port: tools/mixednet_ref/build.sh builds
// it at a pinned commit, and -update runs it.
//
//	PYTHON=<python with numpy, tflite> tools/mixednet_ref/build.sh /tmp/mixednet_ref
//	MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/mixednet -run Vectors -update
var update = flag.Bool("update", false, "regenerate testdata from the reference build (MIXEDNET_REF)")

// realModels are blobs packed by the app's own packer (build.sh): its test
// fixture pooled.tflite ("okay nabu" as the app trains it, by the app's own
// test, and so max-pooled) and three of its built-in models. Apache-2.0, as
// testdata/README.md says.
var realModels = []string{"pooled", "okay_nabu", "hey_jarvis", "stop"}

const realInvokes = 2000

const (
	synthModels  = 24
	synthInvokes = 300
)

// synthCase rebuilds case i from its seed: the blob and features are not
// stored, only what the reference made of them.
func synthCase(i int) ([]byte, []int8) {
	r := rand.New(rand.NewPCG(0x6d6e6231, uint64(i)))
	m := synthModel(r)
	return m.blob(), synthFeatures(r, m, synthInvokes)
}

func expectedPath(i int) string {
	return filepath.Join("testdata", fmt.Sprintf("synth_%02d.expected", i))
}

func TestVectorsSynthetic(t *testing.T) {
	if *update {
		regenerate(t)
	}
	for i := 0; i < synthModels; i++ {
		blob, feats := synthCase(i)
		want, err := os.ReadFile(expectedPath(i))
		if err != nil {
			t.Fatalf("case %d: %v (regenerate with -update)", i, err)
		}
		m, err := Parse(blob)
		if err != nil {
			t.Fatalf("case %d: Parse refused what the reference accepts: %v", i, err)
		}
		got := run(m, feats)
		if !bytes.Equal(got, want) {
			t.Errorf("case %d: first difference at invoke %d", i, firstDiff(got, want))
		}
	}
}

func run(m *Model, feats []int8) []byte {
	e := NewExecutor(m)
	step := m.InputFrames() * m.FeatureSize()
	var out bytes.Buffer
	for off := 0; off+step <= len(feats); off += step {
		p := e.Invoke(feats[off : off+step])
		h := uint32(2166136261) // FNV-1a over the arena, as the harness hashes it
		for _, v := range e.arena {
			h ^= uint32(uint8(v))
			h *= 16777619
		}
		fmt.Fprintf(&out, "%d %d %08x\n", p, e.LastLogit(), h)
	}
	return out.Bytes()
}

func firstDiff(a, b []byte) int {
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := range min(len(al), len(bl)) {
		if al[i] != bl[i] {
			return i
		}
	}
	return min(len(al), len(bl))
}

func regenerate(t *testing.T) {
	dir := os.Getenv("MIXEDNET_REF")
	if dir == "" {
		t.Fatal("-update needs MIXEDNET_REF, the directory tools/mixednet_ref/build.sh fills")
	}
	ref := filepath.Join(dir, "harness")
	tmp := t.TempDir()
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < synthModels; i++ {
		blob, feats := synthCase(i)
		bp, fp := filepath.Join(tmp, "blob"), filepath.Join(tmp, "features")
		fb := make([]byte, len(feats))
		for j, v := range feats {
			fb[j] = byte(v)
		}
		if err := os.WriteFile(bp, blob, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, fb, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(ref, bp, fp).Output()
		if err != nil {
			t.Fatalf("case %d: reference: %v: %s", i, err, out)
		}
		if n := strings.Count(string(out), "\n"); n != synthInvokes {
			t.Fatalf("case %d: reference gave %d lines, want %d", i, n, synthInvokes)
		}
		if err := os.WriteFile(expectedPath(i), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("regenerated %d cases from %s", synthModels, ref)
}

// The synthetic cases must reach what they are there to reach, or matching
// the reference proves little: saturation at both ends, more than a few
// distinct logits, and probabilities that move.
func TestVectorsSyntheticExerciseTheArithmetic(t *testing.T) {
	logits := map[int]bool{}
	for i := 0; i < synthModels; i++ {
		want, err := os.ReadFile(expectedPath(i))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(want)), "\n") {
			f := strings.Fields(line)
			l, _ := strconv.Atoi(f[1])
			logits[l] = true
		}
	}
	if !logits[-128] || !logits[127] {
		t.Errorf("no case saturates the logit at both ends")
	}
	if len(logits) < 64 {
		t.Errorf("only %d distinct logits across the cases", len(logits))
	}
}

// realFeatures are seeded synthetic frames for a real model: the front end
// is not ported yet, so these prove the executor on the app's real weights
// and topologies, not on real audio.
func realFeatures(m *Model, i int) []int8 {
	r := rand.New(rand.NewPCG(0x7265616c, uint64(i)))
	return synthFeatures(r, &tModel{inputFrames: m.InputFrames(), featureSz: m.FeatureSize()}, realInvokes)
}

func TestVectorsRealModels(t *testing.T) {
	if *update {
		regenerateReal(t)
	}
	for i, name := range realModels {
		blob, err := os.ReadFile(filepath.Join("testdata", name+".blob"))
		if err != nil {
			t.Fatalf("%s: %v (regenerate with -update)", name, err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", name+".expected"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := Parse(blob)
		if err != nil {
			t.Fatalf("%s: Parse refused the app's own blob: %v", name, err)
		}
		if got := run(m, realFeatures(m, i)); !bytes.Equal(got, want) {
			t.Errorf("%s: first difference at invoke %d", name, firstDiff(got, want))
		}
	}
}

func regenerateReal(t *testing.T) {
	dir := os.Getenv("MIXEDNET_REF")
	if dir == "" {
		t.Fatal("-update needs MIXEDNET_REF, the directory tools/mixednet_ref/build.sh fills")
	}
	tmp := t.TempDir()
	for i, name := range realModels {
		blob, err := os.ReadFile(filepath.Join(dir, "blobs", name+".blob"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := Parse(blob)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		feats := realFeatures(m, i)
		fb := make([]byte, len(feats))
		for j, v := range feats {
			fb[j] = byte(v)
		}
		fp := filepath.Join(tmp, "features")
		if err := os.WriteFile(fp, fb, 0o644); err != nil {
			t.Fatal(err)
		}
		bp := filepath.Join("testdata", name+".blob")
		if err := os.WriteFile(bp, blob, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(filepath.Join(dir, "harness"), bp, fp).Output()
		if err != nil {
			t.Fatalf("%s: reference: %v: %s", name, err, out)
		}
		if n := strings.Count(string(out), "\n"); n != realInvokes {
			t.Fatalf("%s: reference gave %d lines, want %d", name, n, realInvokes)
		}
		if err := os.WriteFile(filepath.Join("testdata", name+".expected"), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
