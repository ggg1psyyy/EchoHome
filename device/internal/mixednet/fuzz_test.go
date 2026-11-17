package mixednet

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// A blob arrives from the network. Whatever it holds, Parse either refuses it
// or returns a model whose Invoke stays inside its arena: never a panic,
// which on the Echo would be a crash on every reconnect.
func FuzzParseInvoke(f *testing.F) {
	for i := 0; i < 4; i++ {
		blob, _ := synthCase(i)
		f.Add(blob)
	}
	for _, c := range refusals {
		m := seedModel()
		c.edit(m)
		f.Add(m.blob())
	}
	for _, name := range realModels {
		if blob, err := os.ReadFile(filepath.Join("testdata", name+".blob")); err == nil {
			f.Add(blob)
		}
	}
	f.Fuzz(func(t *testing.T, blob []byte) {
		m, err := Parse(blob)
		if err != nil {
			return
		}
		if m.ArenaSize() > MaxArena {
			t.Fatalf("accepted an arena of %d", m.ArenaSize())
		}
		e := NewExecutor(m)
		feats := make([]int8, m.InputFrames()*m.FeatureSize())
		r := rand.New(rand.NewPCG(1, uint64(len(blob))))
		for n := 0; n < 4; n++ {
			for i := range feats {
				feats[i] = int8(r.IntN(256) - 128)
			}
			e.Invoke(feats)
		}
	})
}

func seedModel() *tModel { return synthModel(rand.New(rand.NewPCG(7, 7))) }

// Each refusal of the range proof, one field of a valid blob
// pushed past what its op may touch, and the check that must catch it.
var refusals = []struct {
	name string
	want error
	edit func(m *tModel)
}{
	{"dense reads past its source", errRange, func(m *tModel) { m.ops[3].t0 = 3 }},
	{"dense writes past its destination", errRange, func(m *tModel) { m.ops[3].dstOff = 41 }},
	{"depthwise has more outputs than its taps' channels", errRange, func(m *tModel) {
		o := &m.ops[1]
		o.outCh, o.c0, o.dstOff = 41, 0, 0
		o.bias = append(o.bias, make([]int32, 41-len(o.bias))...)
		o.mult = append(o.mult, make([]int32, 41-len(o.mult))...)
		o.shift = append(o.shift, make([]int32, 41-len(o.shift))...)
	}},
	{"max pool reads past its ring", errRange, func(m *tModel) { m.ops[5].t0, m.ops[5].nt = 0, 5 }},
	{"copy writes past its destination", errRange, func(m *tModel) { m.ops[2].dstOff = 9 }},
	{"ring push of the input is the wrong shape", errInputShape, func(m *tModel) { m.ops[0].nt = 2 }},
	{"an internal ring push overruns its ring", errRange, func(m *tModel) { m.ops[4].nt = 2 }},
	{"copy reads the external input", errExternal, func(m *tModel) { m.ops[2].src = srcExternal }},
	{"a shifted ring is larger than its buffer", errRing, func(m *tModel) { m.bufs[3].push = 2 }},
	{"dense weights run past the data", errArray, func(m *tModel) { m.ops[6].weights = m.ops[6].weights[:10] }},
	{"a shift outside the defined range", errShift, func(m *tModel) { m.ops[6].shift[0] = 31 }},

	// An op that touches nothing is still sliced at its offsets.
	{"an empty copy far outside its source", errRange, func(m *tModel) { m.ops[2].nc, m.ops[2].t0, m.ops[2].stride = 0, 1000, 1000 }},
	{"an empty max pool far outside its source", errRange, func(m *tModel) { m.ops[5].nc, m.ops[5].t0, m.ops[5].stride = 0, 1000, 1000 }},
	{"an empty internal ring push far outside its source", errRange, func(m *tModel) { m.ops[4].nt, m.ops[4].t0, m.ops[4].stride = 0, 1000, 1000 }},
	{"an empty depthwise far outside its source", errRange, func(m *tModel) { m.ops[1].outCh, m.ops[1].t0, m.ops[1].stride = 0, 1000, 1000 }},
	{"an empty dense writing far outside its destination", errRange, func(m *tModel) { m.ops[6].outCh, m.ops[6].dstOff = 0, 1000 }},
	{"a zero-width internal ring push striding out of its source", errRange, func(m *tModel) { m.ops[4].nc, m.ops[4].nt, m.ops[4].stride = 0, 2, 1000 }},
	{"a zero-frame max pool starting past its ring", errRange, func(m *tModel) { m.ops[5].nt, m.ops[5].t0 = 0, 4 }},
	{"a zero-frame ring push into an unshifted buffer keeping more than it holds", errRange, func(m *tModel) {
		// Buffer 1 is not a shifted ring, and 40 channels wide like the push.
		m.ops[4].nt, m.ops[4].dst, m.ops[4].nc = 0, 1, 40
		m.bufs[1].keep = 2
	}},

	// Products of two 16-bit fields pass 32 bits, where int is 32 bits.
	{"one buffer larger than an arena", errArena, func(m *tModel) {
		m.arena = 3000 // a plausible header; only the per-buffer check can refuse
		m.bufs[1].frames, m.bufs[1].channels = 65535, 65535
	}},
	{"a dense window whose offset wraps 32 bits", errRange, func(m *tModel) { m.ops[3].t0, m.ops[3].stride = 65535, 65535 }},
	{"a depthwise span that wraps 32 bits", errRange, func(m *tModel) {
		o := &m.ops[1]
		o.c0, o.dstOff, o.nc, o.outCh, o.nt, o.stride = 0, 0, 1, 1, 65535, 65535
		o.weights = make([]int8, 65535)
		o.bias, o.mult, o.shift = o.bias[:1], o.mult[:1], o.shift[:1]
	}},
	{"a shifted ring whose window wraps 32 bits", errRing, func(m *tModel) {
		// (keep+push)*channels = 131070*40000, past 2^32, in a buffer of
		// 40000 bytes that the arena can still hold.
		m.bufs = append(m.bufs, tBuf{frames: 1, channels: 40000, keep: 65535, push: 65535})
		m.shifts = append(m.shifts, len(m.bufs)-1)
		m.arena = 0
	}},

	// Budgets.
	{"ops whose arrays add up past the copy budget", errBudget, func(m *tModel) {
		// A dense from the input ring into the 40-channel buffer: 4,800
		// bytes of weights, 255 times over, is past maxParams.
		d := m.ops[3]
		d.dst, d.dstOff, d.outCh = 1, 0, 40
		d.weights = make([]int8, 40*d.nt*d.nc)
		d.bias, d.mult, d.shift = make([]int32, 40), make([]int32, 40), make([]int32, 40)
		for len(m.ops) < 255 {
			m.ops = append(m.ops, d)
		}
	}},
}

func TestParseRefusesWhatItCannotProveSafe(t *testing.T) {
	trialInvoke = false // the explicit checks alone
	defer func() { trialInvoke = true }()
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(seedModel().blob()); err != nil {
				t.Fatalf("the unedited model is refused: %v", err)
			}
			m := seedModel()
			c.edit(m)
			_, err := Parse(m.blob())
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// Every op and buffer field of the seed model, set to an edge value singly
// and two at a time within one op or buffer: any model the explicit checks
// accept must run without a panic. Run on 386 too, where int is 32 bits as
// on the Echo. A failure here is a missing check (the trial invoke would
// still refuse it, which is why the trial is switched off).
func TestEveryFieldAtItsEdgesEitherRefusedOrSafe(t *testing.T) {
	trialInvoke = false
	defer func() { trialInvoke = true }()
	edges := []int{0, 1, 2, 3, 40, 1000, 32768, 65535}
	opFields := func(o *tOp) []*int {
		return []*int{&o.t0, &o.c0, &o.nt, &o.nc, &o.stride, &o.dstOff, &o.outCh}
	}
	bufFields := func(b *tBuf) []*int { return []*int{&b.frames, &b.channels, &b.keep, &b.push} }

	try := func(label string, m *tModel) {
		m.arena = 0
		model, err := Parse(m.blob())
		if err != nil {
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: accepted, then Invoke panicked: %v", label, r)
				}
			}()
			NewExecutor(model).Invoke(make([]int8, model.InputFrames()*model.FeatureSize()))
		}()
	}
	nOps, nBufs := len(seedModel().ops), len(seedModel().bufs)
	for oi := 0; oi < nOps; oi++ {
		n := len(opFields(&seedModel().ops[oi]))
		for a := 0; a < n; a++ {
			for b := a; b < n; b++ {
				for _, va := range edges {
					for _, vb := range edges {
						m := seedModel()
						f := opFields(&m.ops[oi])
						*f[a], *f[b] = va, vb
						if a == b {
							*f[a] = va
						}
						try(fmt.Sprintf("op %d fields %d=%d %d=%d", oi, a, va, b, vb), m)
					}
				}
			}
		}
	}
	for bi := 0; bi < nBufs; bi++ {
		n := len(bufFields(&seedModel().bufs[bi]))
		for a := 0; a < n; a++ {
			for b := a; b < n; b++ {
				for _, va := range edges {
					for _, vb := range edges {
						m := seedModel()
						f := bufFields(&m.bufs[bi])
						*f[a], *f[b] = va, vb
						try(fmt.Sprintf("buffer %d fields %d=%d %d=%d", bi, a, va, b, vb), m)
					}
				}
			}
		}
	}
}

// The trial invoke is the layer the Echo's safety finally rests on, so it
// must be seen to refuse: an op pushed out of range after the explicit
// checks have passed, which only the trial can catch.
func TestTheTrialInvokeRefusesAModelThatWouldPanic(t *testing.T) {
	m, err := Parse(seedModel().blob())
	if err != nil {
		t.Fatal(err)
	}
	if !trial(m) {
		t.Fatal("the trial refuses an intact model")
	}
	m.ops[3].srcT0 = 1000 // the dense now reads far past its source
	if trial(m) {
		t.Fatal("the trial accepts a model whose Invoke panics")
	}
}
