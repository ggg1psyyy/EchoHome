// Package softmute is the software half of the mute: Home Assistant's Mute
// switch, as a Voice PE's (home-assistant-voice.yaml master_mute_switch
// mutes its microphones in software, apart from the hardware slider). It
// gives what the hardware mute gives, silence from the microphones at the
// source, without touching the hardware: the capture loop zeroes the
// microphone channels of every frame before anything (wake word, AEC,
// stream, recording) sees them. Loopback channels (our own playback, the
// echo canceller's reference) are left, as the ADC mute leaves them.
package softmute

import "sync/atomic"

var on atomic.Bool

// Set turns the soft mute on or off.
func Set(v bool) { on.Store(v) }

// On reports whether it is on.
func On() bool { return on.Load() }

// Zero silences the first numMics channels of each frame of buf: frames of
// bytesPerFrame bytes, channels interleaved. The microphones are channels
// 0..numMics-1 of every frame (pkg/board MicArray, test-held there).
func Zero(buf []byte, frames, bytesPerFrame, channels, numMics int) {
	if channels <= 0 || numMics <= 0 || bytesPerFrame%channels != 0 {
		// A layout this cannot read: silence the whole frame rather than
		// pass a microphone through.
		clear(buf)
		return
	}
	n := numMics * (bytesPerFrame / channels)
	for f := 0; f < frames; f++ {
		o := f * bytesPerFrame
		if o+bytesPerFrame > len(buf) {
			clear(buf[o:])
			return
		}
		clear(buf[o : o+n])
	}
}
