package incubator

import "math"

// Upsampler turns the app's 16 kHz S16LE audio into the Echo speaker's
// 48 kHz, streamed: a factor-3 polyphase FIR, each input sample becoming
// three output samples, each the dot product of the recent input with one
// phase of a windowed-sinc low-pass cut at 7.2 kHz, so the images 3x
// upsampling folds up are removed rather than played. A port of the
// controller's em_resample.Upsampler, with its taps (resample_taps.go)
// generated from it and its output held to it (testdata/resample_*).
// Chunks of any size, odd byte counts included, give exactly what one call
// over the whole stream gives. Not safe for concurrent use.
type Upsampler struct {
	hist   [tapsPerPhase - 1]float64 // the filter's history, oldest first
	odd    [1]byte
	hasOdd bool
	win    [tapsPerPhase]float64
}

// Process appends the 48 kHz output for pcm to dst and returns it. It
// allocates only if dst lacks capacity (three times the input).
func (u *Upsampler) Process(dst, pcm []byte) []byte {
	if u.hasOdd && len(pcm) > 0 {
		dst = u.sample(dst, int16(uint16(u.odd[0])|uint16(pcm[0])<<8))
		pcm = pcm[1:]
		u.hasOdd = false
	}
	for len(pcm) >= 2 {
		dst = u.sample(dst, int16(uint16(pcm[0])|uint16(pcm[1])<<8))
		pcm = pcm[2:]
	}
	if len(pcm) == 1 {
		u.odd[0], u.hasOdd = pcm[0], true
	}
	return dst
}

// Flush appends the filter's tail: what the last input samples still owe
// (TAPS_PER_PHASE/2 samples of silence through it), and clears the odd byte.
func (u *Upsampler) Flush(dst []byte) []byte {
	u.hasOdd = false
	for i := 0; i < tapsPerPhase/2; i++ {
		dst = u.sample(dst, 0)
	}
	return dst
}

func (u *Upsampler) sample(dst []byte, x int16) []byte {
	copy(u.win[:], u.hist[:])
	u.win[tapsPerPhase-1] = float64(x)
	copy(u.hist[:], u.win[1:])
	for p := 0; p < len(phases); p++ {
		var y float64
		for k, h := range phases[p] {
			y += u.win[k] * h
		}
		v := math.RoundToEven(y) // numpy's rint
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		s := uint16(int16(v))
		dst = append(dst, byte(s), byte(s>>8))
	}
	return dst
}
