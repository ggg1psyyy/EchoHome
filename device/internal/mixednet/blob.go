// Package mixednet runs the streaming int8 wake word models OHF-Voice's
// Voice Incubator pushes to its satellites ("mixednet": microWakeWord's
// MixedNet, lowered by the app into a flat op table plus weights, the MNB1
// blob). No TFLite interpreter: five op types over one int8 arena.
//
// Ported to Go from firmware-voice-incubator's components/voice_incubator/
// mixednet.cpp and mixednet_blob.cpp (themselves vendored from
// synesthesiam/mixednet-executor) at commit
// 64ce6c2aa9f17b779293626a76d36fd9169e06d8, Apache-2.0 (LICENSE beside this
// file). Changes from that source: Go, not C++; the blob is copied out rather
// than pointed into; and Parse proves every op's reads and writes lie inside
// the buffers and arrays they touch.
//
// That last change is load-bearing. A blob arrives from the network, and in
// Go an op whose ranges overran its buffer would panic the firmware; a panic
// on every reconnect is three fast exits, after which start_server.sh flips
// to the other firmware slot. So Parse refuses what it cannot prove safe, in
// two layers: explicit range checks in int64 (Go's int
// is 32 bits on the Echo), each with a named refusal, and then one trial
// Invoke with any panic recovered into a refusal. Every index Invoke uses
// depends on the model's fields alone, so that one trial proves every later
// invoke safe on the machine running it. The tests run the explicit checks
// alone, on 386 as well, over every field at its edge values, and fuzz the
// whole; budgets bound what a model may copy and compute.
package mixednet

import (
	"encoding/binary"
	"errors"
)

// The MNB1 layout. Must match voice_incubator/mixednet.py in the app and
// mixednet_blob.cpp in the firmware.
const (
	headerSize  = 32
	bufferSize  = 10
	opSize      = 52
	sigmoidSize = 256
	nullOffset  = 0xFFFFFFFF

	// MaxArena is the reference executor's limit: it indexes its arena with
	// uint16 offsets. Kept so a blob refused there is refused here.
	MaxArena = 0xFFFF

	// srcExternal marks an op whose source is the feature frames passed to
	// Invoke (MIXEDNET_SRC_EXTERNAL).
	srcExternal = 0xFF
)

// Op types (MixedNetOpType).
const (
	opRingPush  = 0 // append frames to a ring window
	opDense     = 1 // per-channel dot product over a contiguous window
	opDepthwise = 2 // per-channel dot product over K taps
	opCopy      = 3 // channel-axis concatenation
	opMaxPool   = 4 // per-channel maximum over K frames
)

// buffer holds frames rows of channels int8 values, channels innermost. A
// ring buffer keeps its oldest keep frames and receives push new ones per
// invoke.
type buffer struct {
	frames, channels, keep, push int
	init                         int8
	off                          int // into the arena
}

func (b *buffer) size() int { return b.frames * b.channels }

type op struct {
	typ                 int
	src, dst            int // buffer index, or srcExternal
	srcT0, srcC0        int
	srcNT, srcNC        int
	srcStride           int
	dstOff, outChannels int
	inOffset, outOffset int32
	actMin, actMax      int32
	weights             []int8
	bias, mult, shift   []int32
}

// Model is a parsed, validated blob. It holds copies of everything it needs,
// so the blob may be discarded after Parse.
type Model struct {
	buffers     []buffer
	ops         []op
	shifts      []int
	inputFrames int
	featureSize int
	logitBuffer int
	arenaSize   int
	sigmoid     [sigmoidSize]uint8
}

// InputFrames is how many feature frames one Invoke consumes.
func (m *Model) InputFrames() int { return m.inputFrames }

// FeatureSize is the width of one feature frame (40 for every microWakeWord
// model; the caller decides whether to accept another).
func (m *Model) FeatureSize() int { return m.featureSize }

// ArenaSize is the working memory one Executor allocates for this model.
func (m *Model) ArenaSize() int { return m.arenaSize }

// errors carries the reference parser's messages where it has one, so a
// refusal reads the same from either firmware.
var (
	errTruncatedHeader = errors.New("truncated header")
	errBadMagic        = errors.New("bad magic")
	errVersion         = errors.New("unsupported format version")
	errEmpty           = errors.New("empty model")
	errTooLarge        = errors.New("model too large")
	errLogit           = errors.New("logit buffer out of range")
	errArena           = errors.New("arena too large")
	errTables          = errors.New("truncated tables")
	errEmptyBuffer     = errors.New("empty buffer")
	errArenaMismatch   = errors.New("arena size mismatch")
	errData            = errors.New("truncated data section")
	errUnknownOp       = errors.New("unknown op")
	errDst             = errors.New("op destination out of range")
	errSrc             = errors.New("op source out of range")
	errWeights         = errors.New("op is missing weights")
	errShiftBuf        = errors.New("shift buffer out of range")

	// Refusals from the range proof (see the package comment).
	errRange      = errors.New("op reads or writes outside its buffers")
	errArray      = errors.New("op weights or parameters out of range")
	errExternal   = errors.New("external input used by an op that cannot read it")
	errInputShape = errors.New("ring push of the input does not match its frames")
	errRing       = errors.New("ring window larger than its buffer")
	errShift      = errors.New("requantization shift out of range")
	errBudget     = errors.New("model exceeds the memory or work budget")
	errTrial      = errors.New("model fails a trial invoke")
)

// Budgets for what one model may cost: far above any real wake word model
// (okay_nabu: 46,880 B, 36,480 multiply-accumulates per invoke, by the app's
// DESIGN.md) and provisional until measured on an Echo, since 2^21
// operations every 30 ms per model may be a large share of a core. Ops may
// share weight offsets, so a small blob could otherwise ask for many copies
// of a large array.
const (
	maxParams = 1 << 20 // bytes of weights and parameters copied out
	maxWork   = 1 << 21 // multiply-accumulates and element moves per invoke
)

// trialInvoke runs one Invoke at the end of Parse (tests switch it off to
// see the explicit checks alone).
var trialInvoke = true

func u16(p []byte) int    { return int(binary.LittleEndian.Uint16(p)) }
func u32(p []byte) uint32 { return binary.LittleEndian.Uint32(p) }
func i32(p []byte) int32  { return int32(binary.LittleEndian.Uint32(p)) }

// Parse validates an MNB1 blob and returns the model it describes.
func Parse(data []byte) (*Model, error) {
	if len(data) < headerSize {
		return nil, errTruncatedHeader
	}
	if string(data[:4]) != "MNB1" {
		return nil, errBadMagic
	}
	// The reference refuses a blob not 4-byte aligned in memory; a copy out,
	// as here, needs no alignment.
	if u16(data[4:]) != 1 {
		return nil, errVersion
	}
	numBuffers := u16(data[8:])
	numOps := u16(data[10:])
	numShifts := u16(data[12:])
	m := &Model{
		inputFrames: int(data[14]),
		logitBuffer: int(data[15]),
		featureSize: int(data[16]),
	}
	arenaSize := u32(data[20:])
	dataSize := int(u32(data[24:])) // used only after the length check below

	if numBuffers == 0 || numOps == 0 || m.inputFrames == 0 {
		return nil, errEmpty
	}
	if numBuffers >= srcExternal || numOps > 255 || numShifts > 255 {
		return nil, errTooLarge
	}
	if m.logitBuffer >= numBuffers {
		return nil, errLogit
	}
	if arenaSize > MaxArena {
		return nil, errArena
	}
	m.arenaSize = int(arenaSize)

	pos := headerSize
	buffersBytes := numBuffers * bufferSize
	opsBytes := numOps * opSize
	if len(data) < pos+buffersBytes+opsBytes+numShifts {
		return nil, errTables
	}

	m.buffers = make([]buffer, numBuffers)
	arena := 0
	for i := range m.buffers {
		p := data[pos+i*bufferSize:]
		b := &m.buffers[i]
		b.frames, b.channels = u16(p), u16(p[2:])
		b.keep, b.push = u16(p[4:]), u16(p[6:])
		b.init = int8(p[8])
		if b.frames == 0 || b.channels == 0 {
			return nil, errEmptyBuffer
		}
		// Sized in int64 first: int is 32 bits on the Echo, where two
		// 16-bit fields multiply past it, and wrapped sizes could sum to a
		// plausible arena.
		if int64(b.frames)*int64(b.channels) > MaxArena {
			return nil, errArena
		}
		b.off = arena
		arena += b.size()
	}
	if arena != m.arenaSize {
		return nil, errArenaMismatch
	}
	pos += buffersBytes

	// The data section follows the tables, padded to 4 bytes, and the
	// sigmoid table.
	tail := pos + opsBytes + numShifts
	dataStart := (tail+3)&^3 + sigmoidSize
	if int64(len(data)) < int64(dataStart)+int64(u32(data[24:])) {
		return nil, errData
	}
	section := data[dataStart : dataStart+dataSize]
	copy(m.sigmoid[:], data[dataStart-sigmoidSize:dataStart])

	m.ops = make([]op, numOps)
	var params int64 // bytes copied out so far, against maxParams
	for i := range m.ops {
		p := data[pos+i*opSize:]
		o := &m.ops[i]
		o.typ = int(p[0])
		o.src, o.dst = int(p[1]), int(p[2])
		o.srcT0, o.srcC0 = u16(p[4:]), u16(p[6:])
		o.srcNT, o.srcNC = u16(p[8:]), u16(p[10:])
		o.srcStride = u16(p[12:])
		o.dstOff, o.outChannels = u16(p[14:]), u16(p[16:])
		o.inOffset, o.outOffset = i32(p[20:]), i32(p[24:])
		o.actMin, o.actMax = i32(p[28:]), i32(p[32:])

		if o.typ > opMaxPool {
			return nil, errUnknownOp
		}
		if o.dst >= numBuffers {
			return nil, errDst
		}
		if o.src != srcExternal && o.src >= numBuffers {
			return nil, errSrc
		}

		wOff, bOff, mOff, sOff := u32(p[36:]), u32(p[40:]), u32(p[44:]), u32(p[48:])
		arithmetic := o.typ == opDense || o.typ == opDepthwise
		if arithmetic {
			// The reference's "missing weights": null or misaligned offsets.
			if !present(wOff, 1, dataSize) || !present(bOff, 4, dataSize) ||
				!present(mOff, 4, dataSize) || !present(sOff, 4, dataSize) {
				return nil, errWeights
			}
			if err := m.bindArrays(o, section, wOff, bOff, mOff, sOff, &params); err != nil {
				return nil, err
			}
		}
		if err := m.checkRanges(o); err != nil {
			return nil, err
		}
	}
	pos += opsBytes

	m.shifts = make([]int, numShifts)
	for i := range m.shifts {
		idx := int(data[pos+i])
		if idx >= numBuffers {
			return nil, errShiftBuf
		}
		b := &m.buffers[idx]
		// invoke moves keep frames down by push: both must lie in the buffer.
		if (int64(b.keep)+int64(b.push))*int64(b.channels) > int64(b.size()) {
			return nil, errRing
		}
		m.shifts[i] = idx
	}
	if err := m.checkBudget(); err != nil {
		return nil, err
	}
	// Every index Invoke uses depends on the model's fields alone, never on
	// the audio or the arena's contents, so one invoke that does not panic
	// proves every later one safe, on the machine running it: this is what
	// holds on the Echo's 32-bit int, whatever the checks above missed.
	if trialInvoke && !trial(m) {
		return nil, errTrial
	}
	return m, nil
}

func trial(m *Model) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	NewExecutor(m).Invoke(make([]int8, m.inputFrames*m.featureSize))
	return true
}

// checkBudget bounds the work one Invoke does.
func (m *Model) checkBudget() error {
	var work int64
	for i := range m.ops {
		o := &m.ops[i]
		switch o.typ {
		case opDense:
			work += int64(o.outChannels) * int64(o.srcNT) * int64(o.srcNC)
		case opDepthwise:
			work += int64(o.outChannels) * int64(o.srcNT)
		case opMaxPool:
			work += int64(o.srcNC) * int64(o.srcNT)
		default:
			work += int64(o.srcNT) * int64(o.srcNC)
		}
	}
	for _, b := range m.buffers {
		work += int64(b.keep) * int64(b.channels)
	}
	if work > maxWork {
		return errBudget
	}
	return nil
}

// present is the reference's resolve(): an offset that is set, inside the
// data section, and aligned.
func present(off uint32, align uint32, dataSize int) bool {
	return off != nullOffset && int64(off) < int64(dataSize) && off%align == 0
}

// All range arithmetic below is int64: every field is at most 16 bits and
// products of three fit, where Go's int is 32 bits on the Echo.

// bindArrays copies an arithmetic op's weights and per-channel parameters
// out of the data section, refusing any that run past its end.
func (m *Model) bindArrays(o *op, section []byte, wOff, bOff, mOff, sOff uint32, params *int64) error {
	var nWeights int64
	switch o.typ {
	case opDense:
		nWeights = int64(o.outChannels) * int64(o.srcNT) * int64(o.srcNC)
	case opDepthwise:
		nWeights = int64(o.srcNT) * int64(o.srcNC)
	}
	n := int64(o.outChannels)
	end := int64(len(section))
	if int64(wOff)+nWeights > end || int64(bOff)+4*n > end ||
		int64(mOff)+4*n > end || int64(sOff)+4*n > end {
		return errArray
	}
	// Counted before copying: ops may share offsets, so the copies could
	// otherwise far outgrow the blob.
	if *params += nWeights + 12*n; *params > maxParams {
		return errBudget
	}
	o.weights = make([]int8, nWeights)
	for i := range o.weights {
		o.weights[i] = int8(section[int(wOff)+i])
	}
	o.bias, o.mult, o.shift = make([]int32, n), make([]int32, n), make([]int32, n)
	for c := 0; c < int(n); c++ {
		o.bias[c] = i32(section[int(bOff)+4*c:])
		o.mult[c] = i32(section[int(mOff)+4*c:])
		o.shift[c] = i32(section[int(sOff)+4*c:])
		// multiplyByQuantizedMultiplier shifts by these. Outside -30..30 a 1
		// would be shifted into int32's sign bit or past it, where the
		// requantization has no defined result to match, so such a blob is
		// refused. Real models sit far inside (the app's: -12..-5).
		if o.shift[c] < -30 || o.shift[c] > 30 {
			return errShift
		}
	}
	return nil
}

// checkRanges proves that every index the op's Invoke step will touch lies
// inside its source and destination buffers. Offsets are checked even for
// an op that touches nothing, since Invoke slices at them regardless.
func (m *Model) checkRanges(o *op) error {
	dst := &m.buffers[o.dst]
	dstSize := int64(dst.size())
	t0, c0 := int64(o.srcT0), int64(o.srcC0)
	nt, nc := int64(o.srcNT), int64(o.srcNC)
	stride, dstOff, outCh := int64(o.srcStride), int64(o.dstOff), int64(o.outChannels)
	keep, dch := int64(dst.keep), int64(dst.channels)

	if o.src == srcExternal {
		if o.typ != opRingPush {
			return errExternal
		}
		// One Invoke's features land contiguously after the kept frames.
		if nt*nc != int64(m.inputFrames)*int64(m.featureSize) {
			return errInputShape
		}
		if keep*dch+nt*nc > dstSize {
			return errRange
		}
		return nil
	}
	srcSize := int64(m.buffers[o.src].size())
	base := t0*stride + c0
	// reads is whether nt frames from base, width values each and stride
	// apart, stay in the source. Frame 0 counts even when nt is 0 (max
	// pooling reads it regardless), and a zero width still slices at every
	// frame's start, so neither shortens the check.
	reads := func(width int64) bool {
		return base+max(nt-1, 0)*stride+width <= srcSize
	}
	writes := func(width int64) bool { return dstOff+width <= dstSize }

	switch o.typ {
	case opRingPush:
		// Frame t is written at row keep+t of dst, nc channels of it.
		if nc > dch || !reads(nc) || keep*dch > dstSize {
			return errRange
		}
		if nt > 0 && (keep+nt-1)*dch+nc > dstSize {
			return errRange
		}
	case opCopy:
		if base+nc > srcSize || !writes(nc) {
			return errRange
		}
	case opDense:
		// A dense op reads one contiguous window from frame srcT0 (no
		// channel offset), and writes outChannels values.
		if t0*stride+nt*nc > srcSize || !writes(outCh) {
			return errRange
		}
	case opDepthwise:
		// Channel c of tap k reads src[k*stride+c] and weights[k*nc+c].
		if outCh > nc || !reads(outCh) || !writes(outCh) {
			return errRange
		}
	case opMaxPool:
		if !reads(nc) || !writes(nc) {
			return errRange
		}
	}
	return nil
}
