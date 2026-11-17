// Ported from firmware-voice-incubator's components/voice_incubator/
// mixednet.cpp at commit 64ce6c2aa9f17b779293626a76d36fd9169e06d8,
// Apache-2.0 (LICENSE beside this file); see the package comment in blob.go.

package mixednet

import "math"

// Executor runs one Model's streaming state. Not safe for concurrent use;
// Invoke allocates nothing.
type Executor struct {
	m         *Model
	arena     []int8
	lastLogit int8
}

// NewExecutor allocates the model's arena and resets it to its initial values.
func NewExecutor(m *Model) *Executor {
	e := &Executor{m: m, arena: make([]int8, m.arenaSize)}
	e.Reset()
	return e
}

// Reset clears all streaming state back to the model's initial values.
func (e *Executor) Reset() {
	for i := range e.m.buffers {
		b := &e.m.buffers[i]
		buf := e.arena[b.off : b.off+b.size()]
		for j := range buf {
			buf[j] = b.init
		}
	}
	e.lastLogit = 0
}

// LastLogit is the int8 logit of the most recent Invoke, before the sigmoid.
func (e *Executor) LastLogit() int8 { return e.lastLogit }

// Invoke runs one inference step over InputFrames feature frames (laid out
// frame by frame, FeatureSize values each) and returns the wake word
// probability quantized to 0-255. It returns 0 and does nothing for features
// of the wrong length.
func (e *Executor) Invoke(features []int8) uint8 {
	m := e.m
	if len(features) != m.inputFrames*m.featureSize {
		return 0
	}
	for i := range m.ops {
		o := &m.ops[i]
		switch o.typ {
		case opRingPush:
			e.ringPush(o, features)
		case opDense:
			e.dense(o)
		case opDepthwise:
			e.depthwise(o)
		case opCopy:
			e.copyOp(o)
		case opMaxPool:
			e.maxPool(o)
		}
	}
	lb := &m.buffers[m.logitBuffer]
	e.lastLogit = e.arena[lb.off]
	p := m.sigmoid[uint8(e.lastLogit)]

	// Advance every ring window now that all reads of this invoke are done.
	for _, idx := range m.shifts {
		b := &m.buffers[idx]
		base := e.arena[b.off : b.off+b.size()]
		copy(base, base[b.push*b.channels:(b.push+b.keep)*b.channels])
	}
	return p
}

// buf is buffer i's slice of the arena, capped at its end: an index past it
// panics rather than reading the next buffer, so a gap in Parse's checks
// shows under the fuzzer instead of passing unseen.
func (e *Executor) buf(i int) []int8 {
	b := &e.m.buffers[i]
	return e.arena[b.off : b.off+b.size() : b.off+b.size()]
}

func (e *Executor) ringPush(o *op, features []int8) {
	db := &e.m.buffers[o.dst]
	dst := e.buf(o.dst)[db.keep*db.channels:]
	if o.src == srcExternal {
		copy(dst, features)
		return
	}
	src := e.buf(o.src)[o.srcT0*o.srcStride+o.srcC0:]
	for t := 0; t < o.srcNT; t++ {
		copy(dst[t*db.channels:t*db.channels+o.srcNC], src[t*o.srcStride:t*o.srcStride+o.srcNC])
	}
}

func (e *Executor) copyOp(o *op) {
	src := e.buf(o.src)[o.srcT0*o.srcStride+o.srcC0:]
	copy(e.buf(o.dst)[o.dstOff:o.dstOff+o.srcNC], src[:o.srcNC])
}

func (e *Executor) dense(o *op) {
	row := o.srcNT * o.srcNC
	src := e.buf(o.src)[o.srcT0*o.srcStride:][:row]
	dst := e.buf(o.dst)[o.dstOff:][:o.outChannels]
	for oc := range dst {
		w := o.weights[oc*row:][:row]
		acc := o.bias[oc]
		for i, x := range src {
			acc += int32(w[i]) * (int32(x) + o.inOffset)
		}
		acc = multiplyByQuantizedMultiplier(acc, o.mult[oc], o.shift[oc]) + o.outOffset
		dst[oc] = int8(clamp(acc, o.actMin, o.actMax))
	}
}

func (e *Executor) depthwise(o *op) {
	src := e.buf(o.src)[o.srcT0*o.srcStride+o.srcC0:]
	dst := e.buf(o.dst)[o.dstOff:][:o.outChannels]
	for c := range dst {
		acc := o.bias[c]
		for k := 0; k < o.srcNT; k++ {
			acc += int32(o.weights[k*o.srcNC+c]) * (int32(src[k*o.srcStride+c]) + o.inOffset)
		}
		acc = multiplyByQuantizedMultiplier(acc, o.mult[c], o.shift[c]) + o.outOffset
		dst[c] = int8(clamp(acc, o.actMin, o.actMax))
	}
}

// maxPool keeps the input's quantization, so no requantization: the int8
// maximum of K frames, channel by channel.
func (e *Executor) maxPool(o *op) {
	src := e.buf(o.src)[o.srcT0*o.srcStride+o.srcC0:]
	dst := e.buf(o.dst)[o.dstOff:][:o.srcNC]
	for c := range dst {
		best := int32(src[c])
		for k := 1; k < o.srcNT; k++ {
			best = max(best, int32(src[k*o.srcStride+c]))
		}
		dst[c] = int8(clamp(best, o.actMin, o.actMax))
	}
}

// clamp is the reference's std::min(std::max(x, lo), hi): with lo > hi it
// returns hi, as that does.
func clamp(x, lo, hi int32) int32 {
	return min(max(x, lo), hi)
}

// The requantization is gemmlowp's double rounding, as the reference builds
// it (MIXEDNET_SINGLE_ROUNDING undefined, matching esp-tflite-micro and
// esp-nn). Go's int32 arithmetic wraps, as the C++ does on every target it
// runs on, and int64 division truncates toward zero, as C++'s does.

func saturatingRoundingDoublingHighMul(a, b int32) int32 {
	if a == b && a == math.MinInt32 {
		return math.MaxInt32
	}
	ab := int64(a) * int64(b)
	nudge := int64(1 << 30)
	if ab < 0 {
		nudge = 1 - (1 << 30)
	}
	// (ab + nudge) / 2^31, truncating toward zero as C++'s division does,
	// written as shifts: 32-bit ARM has no divide instruction, and Go's
	// int64 division there is a runtime call, once per output channel.
	s := ab + nudge
	if s < 0 {
		s += 1<<31 - 1
	}
	return int32(s >> 31)
}

func roundingDivideByPOT(x int32, exponent int32) int32 {
	if exponent == 0 {
		return x
	}
	mask := int32(1)<<exponent - 1
	remainder := x & mask
	threshold := mask >> 1
	if x < 0 {
		threshold++
	}
	r := x >> exponent
	if remainder > threshold {
		r++
	}
	return r
}

func multiplyByQuantizedMultiplier(x, multiplier, shift int32) int32 {
	left, right := int32(0), int32(0)
	if shift > 0 {
		left = shift
	} else {
		right = -shift
	}
	return roundingDivideByPOT(saturatingRoundingDoublingHighMul(x*(int32(1)<<left), multiplier), right)
}
