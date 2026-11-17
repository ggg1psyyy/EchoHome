package microfeatures

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Expected frames come from tools/mixednet_ref's features_harness (this
// package's C, configured and scaled by the firmware's own lines):
//
//	MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/microfeatures -update
var update = flag.Bool("update", false, "regenerate testdata from MIXEDNET_REF")

// signal returns case i: seeded noise at a level that rises over the clip,
// with a tone sweeping the band, so every filterbank channel and the
// noise and gain adaptation are exercised.
func signal(i int) []int16 {
	r := rand.New(rand.NewPCG(0x6d66, uint64(i)))
	n := 3 * SampleRate
	out := make([]int16, n)
	ph := 0.0
	for j := range out {
		t := float64(j) / float64(n)
		ph += 2 * math.Pi * (100 + 7000*t) / SampleRate
		level := math.Pow(10, float64(i)/2) * (0.2 + t)
		v := level*(r.NormFloat64()*30+math.Sin(ph)*60) + 0.5
		out[j] = int16(max(-32768, min(32767, v)))
	}
	return out
}

const cases = 6

// features runs the Go front end, feeding chunks of the given size.
func features(t *testing.T, pcm []int16, chunk int) []byte {
	f, err := New(DefaultStepMs)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	for i := 0; i < len(pcm); i += chunk {
		f.Feed(pcm[i:min(len(pcm), i+chunk)], func(fr *[FeatureSize]int8) {
			for k, v := range fr {
				if k > 0 {
					out.WriteByte(' ')
				}
				fmt.Fprint(&out, v)
			}
			out.WriteByte('\n')
		})
	}
	return out.Bytes()
}

func expected(i int) string { return filepath.Join("testdata", fmt.Sprintf("case_%d.expected", i)) }

func TestMatchesTheReference(t *testing.T) {
	if *update {
		ref := os.Getenv("MIXEDNET_REF")
		if ref == "" {
			t.Fatal("-update needs MIXEDNET_REF")
		}
		os.MkdirAll("testdata", 0o755)
		for i := 0; i < cases; i++ {
			raw := filepath.Join(t.TempDir(), "pcm")
			var b bytes.Buffer
			binary.Write(&b, binary.LittleEndian, signal(i))
			os.WriteFile(raw, b.Bytes(), 0o644)
			out, err := exec.Command(filepath.Join(ref, "features_harness"), raw, "10", "512").Output()
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(expected(i), out, 0o644)
		}
	}
	for i := 0; i < cases; i++ {
		want, err := os.ReadFile(expected(i))
		if err != nil {
			t.Fatal(err)
		}
		if got := features(t, signal(i), 512); !bytes.Equal(got, want) {
			t.Errorf("case %d differs from the reference", i)
		}
	}
}

// How the audio is split must not change the frames.
func TestChunkingDoesNotMatter(t *testing.T) {
	pcm := signal(3)
	want := features(t, pcm, len(pcm))
	for _, chunk := range []int{1, 7, 160, 1000} {
		if got := features(t, pcm, chunk); !bytes.Equal(got, want) {
			t.Errorf("chunks of %d give different frames", chunk)
		}
	}
}

func TestScale(t *testing.T) {
	for _, c := range []struct {
		in   uint16
		want int8
	}{{0, -128}, {333, 0}, {664, 127}, {65535, 127}} {
		if got := Scale(c.in); got != c.want {
			t.Errorf("Scale(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFeedAllocatesNothingAndCloseIsSafe(t *testing.T) {
	f, err := New(DefaultStepMs)
	if err != nil {
		t.Fatal(err)
	}
	pcm := signal(2)[:1600]
	emit := func(*[FeatureSize]int8) {}
	if n := testing.AllocsPerRun(20, func() { f.Feed(pcm, emit) }); n != 0 {
		t.Errorf("Feed allocates %v times per call", n)
	}
	f.Close()
	f.Feed(pcm, emit) // after Close: a no-op, not a crash
	f.Reset()
	f.Close()
}
