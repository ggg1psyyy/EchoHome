// Package microfeatures turns 16 kHz audio into the int8 spectrogram frames
// microWakeWord models are trained on, as the Voice Incubator firmware
// computes them before its mixednet executor: TensorFlow Lite Micro's
// fixed-point microfrontend (esphome/esp-micro-speech-features 1.2.3,
// vendored byte-identical under libmsf/ by vendor.sh, sha256-checked,
// Apache-2.0; its kiss_fft files are BSD-licensed, (c) Mark Borgerding, with
// their notice in the sources), configured and scaled as
// firmware-voice-incubator's components/voice_incubator/wake_detector.cpp
// does at commit 64ce6c2aa9f17b779293626a76d36fd9169e06d8.
//
// What the tests show: frames identical to that configuration and scaling
// (cut verbatim from the firmware by tools/mixednet_ref) on synthetic audio,
// on the host. Not yet on an Echo, where the set-up's float math (window,
// filterbank and gain tables) runs on another libm.
//
// One feature frame of FeatureSize values comes out per step (10 ms by
// default) over a 30 ms window.
package microfeatures

/*
#cgo CFLAGS: -I${SRCDIR}/libmsf/include -I${SRCDIR}/libmsf/src -O2 -w
#cgo LDFLAGS: -lm
#include <stdlib.h>
#include "frontend.h"
#include "frontend_util.h"

// The detector's configuration (WakeDetector::setup in wake_detector.cpp),
// line for line, on a zeroed config as its value-initialised member is:
// output_scale_shift stays 0, which nothing in the processing reads.
static int em_populate(struct FrontendState *state, int step_ms) {
	struct FrontendConfig c = {0};
	c.window.size_ms = 30;
	c.window.step_size_ms = step_ms;
	c.filterbank.num_channels = 40;
	c.filterbank.lower_band_limit = 125.0f;
	c.filterbank.upper_band_limit = 7500.0f;
	c.noise_reduction.smoothing_bits = 10;
	c.noise_reduction.even_smoothing = 0.025f;
	c.noise_reduction.odd_smoothing = 0.06f;
	c.noise_reduction.min_signal_remaining = 0.05f;
	c.pcan_gain_control.enable_pcan = 1;
	c.pcan_gain_control.strength = 0.95f;
	c.pcan_gain_control.offset = 80.0f;
	c.pcan_gain_control.gain_bits = 21;
	c.log_scale.enable_log = 1;
	c.log_scale.scale_shift = 6;
	return FrontendPopulateState(&c, state, 16000);
}

// One FrontendProcessSamples call, returned flat for cgo.
static size_t em_process(struct FrontendState *state, const int16_t *samples, size_t n,
                         size_t *read, const uint16_t **values) {
	struct FrontendOutput out = FrontendProcessSamples(state, samples, n, read);
	*values = out.values;
	return out.size;
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

const (
	// SampleRate is the only rate the front end is set up for.
	SampleRate = 16000
	// FeatureSize is the width of one frame, as every microWakeWord model
	// expects.
	FeatureSize = 40
	// DefaultStepMs is the detector's default feature step.
	DefaultStepMs = 10
)

// Frontend holds one stream's state. Not safe for concurrent use; Feed
// allocates nothing.
type Frontend struct {
	state *C.struct_FrontendState
	frame [FeatureSize]int8
	read  C.size_t
	vals  *C.uint16_t
}

// New sets up a front end with the given feature step in milliseconds
// (models state theirs; the detector refuses a model whose step differs).
func New(stepMs int) (*Frontend, error) {
	if stepMs <= 0 || stepMs > 30 {
		return nil, errors.New("microfeatures: step must be 1..30 ms")
	}
	s := (*C.struct_FrontendState)(C.calloc(1, C.size_t(unsafe.Sizeof(C.struct_FrontendState{}))))
	if s == nil {
		return nil, errors.New("microfeatures: out of memory")
	}
	if C.em_populate(s, C.int(stepMs)) == 0 {
		C.free(unsafe.Pointer(s))
		return nil, errors.New("microfeatures: front end set-up failed")
	}
	return &Frontend{state: s}, nil
}

// Feed consumes samples and calls emit once per completed feature frame, as
// the detector's read loop does. The frame is reused by the next call: copy
// it to keep it.
func (f *Frontend) Feed(samples []int16, emit func(frame *[FeatureSize]int8)) {
	if f.state == nil {
		return // closed: C would be handed a null state
	}
	for len(samples) > 0 {
		n := C.em_process(f.state, (*C.int16_t)(unsafe.Pointer(&samples[0])), C.size_t(len(samples)), &f.read, &f.vals)
		if f.read == 0 {
			return // cannot happen with a non-empty window; never spin
		}
		samples = samples[f.read:]
		if n == 0 {
			continue
		}
		values := unsafe.Slice((*uint16)(unsafe.Pointer(f.vals)), int(n))
		for i := 0; i < FeatureSize && i < len(values); i++ {
			f.frame[i] = Scale(values[i])
		}
		emit(&f.frame)
	}
}

// Scale maps one front end value onto the int8 range, as
// WakeDetector::generate_features_ does: the front end emits roughly 0-670,
// training divided that by 25.6 and by 26.0, and quantization maps 0.0-1.0
// onto int8.
func Scale(v uint16) int8 {
	const scale, div = 256, 666 // 25.6 * 26.0, rounded
	x := (int32(v)*scale+div/2)/div - 128
	return int8(min(max(x, -128), 127))
}

// Reset clears the stream's state (noise estimate, gain, window).
func (f *Frontend) Reset() {
	if f.state != nil {
		C.FrontendReset(f.state)
	}
}

// Close frees the front end. It may not be used afterwards.
func (f *Frontend) Close() {
	if f.state != nil {
		C.FrontendFreeStateContents(f.state)
		C.free(unsafe.Pointer(f.state))
		f.state = nil
	}
}
