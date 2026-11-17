package incubator

import (
	"bytes"
	"os"
	"testing"
)

func reference(t *testing.T) (in, out []byte) {
	t.Helper()
	in, err := os.ReadFile("testdata/resample_in.bin")
	if err != nil {
		t.Fatal(err)
	}
	out, err = os.ReadFile("testdata/resample_out.bin")
	if err != nil {
		t.Fatal(err)
	}
	return in, out
}

// Sample for sample what the controller's em_resample makes of the same
// audio (tools/incubator_interop/resample_gen.py), tail included.
func TestTheUpsamplerMatchesTheControllers(t *testing.T) {
	in, want := reference(t)
	var u Upsampler
	got := u.Flush(u.Process(nil, in))
	if len(got) != len(want) {
		t.Fatalf("%d bytes, want %d", len(got), len(want))
	}
	diff, worst := 0, 0
	for i := 0; i < len(got); i += 2 {
		a := int(int16(uint16(got[i]) | uint16(got[i+1])<<8))
		b := int(int16(uint16(want[i]) | uint16(want[i+1])<<8))
		if a != b {
			diff++
			worst = max(worst, abs(a-b))
		}
	}
	// numpy's matrix product may sum in another order; that can move a
	// value sitting on a rounding tie by one step, and nothing more.
	if worst > 1 || diff > len(got)/2/10000 {
		t.Fatalf("%d of %d samples differ, by up to %d", diff, len(got)/2, worst)
	}
	t.Logf("%d of %d samples differ by one step", diff, len(got)/2)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestChunkingDoesNotChangeTheOutput(t *testing.T) {
	in, _ := reference(t)
	var whole Upsampler
	want := whole.Flush(whole.Process(nil, in))
	for _, cut := range []int{1, 3, 7, 160, 999, 4096} {
		var u Upsampler
		var got []byte
		for i := 0; i < len(in); i += cut {
			got = u.Process(got, in[i:min(len(in), i+cut)])
		}
		got = u.Flush(got)
		if !bytes.Equal(got, want) {
			t.Errorf("chunks of %d bytes give different output", cut)
		}
	}
}

func TestThreeOutputSamplesPerInputAndNoAllocation(t *testing.T) {
	var u Upsampler
	in := make([]byte, 320)
	dst := make([]byte, 0, 960)
	if got := u.Process(dst, in); len(got) != 960 {
		t.Fatalf("%d bytes from 160 samples", len(got))
	}
	if n := testing.AllocsPerRun(50, func() { u.Process(dst[:0], in) }); n != 0 {
		t.Errorf("Process allocates %v times per call", n)
	}
}
