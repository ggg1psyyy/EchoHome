package mixednet

import (
	"encoding/binary"
	"math/bits"
	"math/rand/v2"
)

// A test-only writer for MNB1 blobs, laid out as voice_incubator/mixednet.py
// writes them: header, buffer table, op table, shift list, padding to 4
// bytes, the 256-byte sigmoid table, then the data section with every array
// 4-byte aligned.

type tBuf struct{ frames, channels, keep, push, init int }

type tOp struct {
	typ, src, dst                 int
	t0, c0, nt, nc, stride        int
	dstOff, outCh                 int
	inOff, outOff, actMin, actMax int32
	weights                       []int8
	bias, mult, shift             []int32
}

type tModel struct {
	bufs                   []tBuf
	ops                    []tOp
	shifts                 []int
	inputFrames, featureSz int
	logit                  int
	sigmoid                [256]uint8
	// arena, when set, is the arena size the header declares, in place of
	// the buffers' true sum.
	arena int
}

func (m *tModel) blob() []byte {
	var data []byte
	align := func() {
		for len(data)%4 != 0 {
			data = append(data, 0)
		}
	}
	place8 := func(v []int8) uint32 {
		if v == nil {
			return nullOffset
		}
		align()
		off := uint32(len(data))
		for _, x := range v {
			data = append(data, byte(x))
		}
		return off
	}
	place32 := func(v []int32) uint32 {
		if v == nil {
			return nullOffset
		}
		align()
		off := uint32(len(data))
		for _, x := range v {
			data = binary.LittleEndian.AppendUint32(data, uint32(x))
		}
		return off
	}
	var ops []byte
	for _, o := range m.ops {
		p := make([]byte, opSize)
		p[0], p[1], p[2] = byte(o.typ), byte(o.src), byte(o.dst)
		le := binary.LittleEndian
		le.PutUint16(p[4:], uint16(o.t0))
		le.PutUint16(p[6:], uint16(o.c0))
		le.PutUint16(p[8:], uint16(o.nt))
		le.PutUint16(p[10:], uint16(o.nc))
		le.PutUint16(p[12:], uint16(o.stride))
		le.PutUint16(p[14:], uint16(o.dstOff))
		le.PutUint16(p[16:], uint16(o.outCh))
		le.PutUint32(p[20:], uint32(o.inOff))
		le.PutUint32(p[24:], uint32(o.outOff))
		le.PutUint32(p[28:], uint32(o.actMin))
		le.PutUint32(p[32:], uint32(o.actMax))
		le.PutUint32(p[36:], place8(o.weights))
		le.PutUint32(p[40:], place32(o.bias))
		le.PutUint32(p[44:], place32(o.mult))
		le.PutUint32(p[48:], place32(o.shift))
		ops = append(ops, p...)
	}
	align()

	var body []byte
	arena := 0
	for _, b := range m.bufs {
		p := make([]byte, bufferSize)
		binary.LittleEndian.PutUint16(p, uint16(b.frames))
		binary.LittleEndian.PutUint16(p[2:], uint16(b.channels))
		binary.LittleEndian.PutUint16(p[4:], uint16(b.keep))
		binary.LittleEndian.PutUint16(p[6:], uint16(b.push))
		p[8] = byte(b.init)
		body = append(body, p...)
		arena += b.frames * b.channels
	}
	body = append(body, ops...)
	for _, s := range m.shifts {
		body = append(body, byte(s))
	}
	for (headerSize+len(body))%4 != 0 {
		body = append(body, 0)
	}
	body = append(body, m.sigmoid[:]...)
	body = append(body, data...)

	h := make([]byte, headerSize)
	copy(h, "MNB1")
	binary.LittleEndian.PutUint16(h[4:], 1)
	binary.LittleEndian.PutUint16(h[8:], uint16(len(m.bufs)))
	binary.LittleEndian.PutUint16(h[10:], uint16(len(m.ops)))
	binary.LittleEndian.PutUint16(h[12:], uint16(len(m.shifts)))
	h[14], h[15], h[16] = byte(m.inputFrames), byte(m.logit), byte(m.featureSz)
	if m.arena != 0 {
		arena = m.arena
	}
	binary.LittleEndian.PutUint32(h[20:], uint32(arena))
	binary.LittleEndian.PutUint32(h[24:], uint32(len(data)))
	return append(h, body...)
}

// synthModel is a small streaming net over 40-wide features that uses every
// op the way the app's lowering does: the input pushed into a ring, a
// depthwise filter over it, a channel concatenation (copy) beside a dense
// layer, a second ring fed from inside the arena, max pooling over it, and a
// dense layer down to one logit. Every parameter is drawn from r.
func synthModel(r *rand.Rand) *tModel {
	const feat, in = 40, 3
	i8 := func(n int) []int8 {
		v := make([]int8, n)
		for i := range v {
			v[i] = int8(r.IntN(256) - 128)
		}
		return v
	}
	quant := func(n, fanIn int, wideShift bool) (bias, mult, shift []int32) {
		// A real layer's shift brings fanIn products of two int8s back to
		// int8: about -(log2(fanIn) + 7). Centred there, outputs spread over
		// the int8 range instead of saturating.
		centre := -(bits.Len(uint(fanIn)) + 7)
		bias, mult, shift = make([]int32, n), make([]int32, n), make([]int32, n)
		for i := range bias {
			bias[i] = int32(r.IntN(2001) - 1000)
			// TFLite multipliers are normalised to [2^30, 2^31).
			mult[i] = int32(1<<30 + r.Int64N(1<<30))
			if wideShift {
				shift[i] = int32(r.IntN(32) - 30) // the whole permitted range below 2, -30..1
			} else {
				shift[i] = int32(centre + r.IntN(5) - 2) // where real layers sit
			}
		}
		// Pin the edges once each: shift 0 (no divide), a positive shift,
		// and a multiplier that saturates the high mul on INT32_MIN.
		// Only in a layer with channels to spare: the one-channel logit
		// layer pinned to shift 0 would saturate every invoke.
		if n >= 8 {
			shift[0] = 0
			shift[1] = 1
			mult[2] = -1 << 31
			// Exactly one half: the high mul then lands on a rounding tie
			// whenever the accumulator is odd, the only place the two
			// halves of its nudge differ.
			mult[3] = 1 << 30
		}
		return
	}
	act := func() (int32, int32) {
		// Now and then inverted: the reference's min(max(x, lo), hi) then
		// returns hi, and the order of the two is all that decides it.
		if r.IntN(8) == 0 {
			return int32(r.IntN(64) + 64), int32(r.IntN(64) - 128)
		}
		if r.IntN(4) == 0 {
			return int32(r.IntN(64) - 128), int32(r.IntN(64) + 64)
		}
		return -128, 127
	}
	wide := r.IntN(3) == 0
	m := &tModel{inputFrames: in, featureSz: feat, logit: 5}
	m.bufs = []tBuf{
		{frames: 2 + in, channels: feat, keep: 2, push: in, init: r.IntN(256) - 128}, // 0: input ring
		{frames: 1, channels: feat},     // 1: depthwise out
		{frames: 1, channels: feat + 8}, // 2: concat
		{frames: 4, channels: feat + 8, keep: 3, push: 1, init: r.IntN(256) - 128}, // 3: pooling ring
		{frames: 1, channels: feat + 8},                                            // 4: pooled
		{frames: 1, channels: 1},                                                   // 5: logit
	}
	m.shifts = []int{0, 3}
	// The depthwise layer starts at channel c0 of its source and writes at
	// the same offset; the max pool starts at frame pt0 of its ring. Both
	// vary, so channel and frame offsets are exercised, not only zero.
	c0, pt0 := r.IntN(5), r.IntN(2)
	b1, m1, s1 := quant(feat-c0, 2+in, wide)
	lo1, hi1 := act()
	b2, m2, s2 := quant(8, in*feat, wide)
	lo2, hi2 := act()
	b3, m3, s3 := quant(1, feat+8, wide)
	// Dense channel 4 reaches the high mul's one saturating input: zero
	// weights leave the accumulator at its bias, -2, and a left shift of 30
	// makes that exactly INT32_MIN, against a multiplier of INT32_MIN.
	w2 := i8(8 * in * feat)
	clear(w2[4*in*feat : 5*in*feat])
	b2[4], s2[4], m2[4] = -2, 30, -1<<31
	// The saturated INT32_MAX then has out_offset added with int32
	// wrap-around: for any non-zero offset MAX+off and MIN+off clamp alike,
	// so the edge is only observable at offset 0, which half the models use.
	out2 := int32(r.IntN(256) - 128)
	if r.IntN(2) == 0 {
		out2 = 0
	}
	lo4, hi4 := act()
	m.ops = []tOp{
		{typ: opRingPush, src: srcExternal, dst: 0, nt: in, nc: feat, stride: feat},
		{typ: opDepthwise, src: 0, dst: 1, c0: c0, nt: 2 + in, nc: feat, stride: feat, outCh: feat - c0, dstOff: c0,
			inOff: int32(r.IntN(256) - 128), outOff: int32(r.IntN(256) - 128), actMin: lo1, actMax: hi1,
			weights: i8((2 + in) * feat), bias: b1, mult: m1, shift: s1},
		{typ: opCopy, src: 1, dst: 2, nt: 1, nc: feat, stride: feat},
		{typ: opDense, src: 0, dst: 2, t0: 2, nt: in, nc: feat, stride: feat, dstOff: feat, outCh: 8,
			inOff: int32(r.IntN(256) - 128), outOff: out2, actMin: lo2, actMax: hi2,
			weights: w2, bias: b2, mult: m2, shift: s2},
		{typ: opRingPush, src: 2, dst: 3, nt: 1, nc: feat + 8, stride: feat + 8},
		{typ: opMaxPool, src: 3, dst: 4, t0: pt0, nt: 4 - pt0, nc: feat + 8, stride: feat + 8, actMin: lo4, actMax: hi4},
		{typ: opDense, src: 4, dst: 5, nt: 1, nc: feat + 8, stride: feat + 8, outCh: 1,
			inOff: int32(r.IntN(256) - 128), outOff: int32(r.IntN(256) - 128), actMin: -128, actMax: 127,
			weights: i8(feat + 8), bias: b3, mult: m3, shift: s3},
	}
	for i := range m.sigmoid {
		m.sigmoid[i] = uint8(r.IntN(256))
	}
	return m
}

// synthFeatures is n invokes' worth of input: mostly a slow random walk, as
// a spectrogram looks, with full-scale bursts so saturation is reached.
func synthFeatures(r *rand.Rand, m *tModel, n int) []int8 {
	out := make([]int8, n*m.inputFrames*m.featureSz)
	v := 0
	for i := range out {
		if r.IntN(50) == 0 {
			v = r.IntN(256) - 128
		} else {
			v = min(127, max(-128, v+r.IntN(17)-8))
		}
		out[i] = int8(v)
	}
	return out
}
