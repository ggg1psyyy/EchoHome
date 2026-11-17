// Ported from firmware-voice-incubator's ModelStream (wake_detector.h and
// wake_detector.cpp) at commit 64ce6c2aa9f17b779293626a76d36fd9169e06d8,
// Apache-2.0 (LICENSE beside this file).

package mixednet

import "errors"

// MinSlicesBeforeDetection is how many quiet feature frames a stream waits
// out, at start and after each detection, before it may fire again
// (MIN_SLICES_BEFORE_DETECTION: one second at the 10 ms step).
const MinSlicesBeforeDetection = 100

// StreamFeatureSize is the frame width a Stream accepts (FEATURE_SIZE).
const StreamFeatureSize = 40

// Stream turns one model's per-invoke probabilities into detections, as the
// firmware's ModelStream does: frames accumulate until the model has an
// input stride, each invoke's probability goes into a ring of the model's
// sliding window, and the stream fires when the window's sum exceeds
// cutoff x window. A mute window of MinSlicesBeforeDetection frames, counted
// down only while the model is below its cutoff, keeps one utterance from
// firing twice. Not safe for concurrent use; Step allocates nothing.
type Stream struct {
	exec    *Executor
	input   []int8
	step    int
	recent  []uint8
	last    int
	ignore  int16
	fresh   bool
	cutoff  uint8
	frameSz int
}

// NewStream sets up a stream for m with the model's probability cutoff and
// sliding window size, both as its manifest states them (0-255 and frames of
// invokes).
func NewStream(m *Model, cutoff, window uint8) (*Stream, error) {
	if window == 0 {
		return nil, errors.New("mixednet: a sliding window of zero")
	}
	// The firmware refuses any other ("unexpected feature size"): frames
	// come from the front end 40 wide, and every microWakeWord model is.
	if m.FeatureSize() != StreamFeatureSize {
		return nil, errors.New("mixednet: a model whose feature frames are not 40 wide")
	}
	s := &Stream{
		exec:    NewExecutor(m),
		input:   make([]int8, m.InputFrames()*m.FeatureSize()),
		recent:  make([]uint8, window),
		cutoff:  cutoff,
		frameSz: m.FeatureSize(),
	}
	s.Reset()
	return s, nil
}

// Feed takes one feature frame (FeatureSize values), invoking the model when
// a full input stride has gathered (ModelStream::feed).
func (s *Stream) Feed(frame []int8) {
	copy(s.input[s.step*s.frameSz:(s.step+1)*s.frameSz], frame)
	s.step++
	if s.step >= s.exec.m.InputFrames() {
		s.step = 0
		p := s.exec.Invoke(s.input)
		s.last++
		if s.last >= len(s.recent) {
			s.last = 0
		}
		s.recent[s.last] = p
		s.fresh = true
	}
	// Count the mute window down only while the model is quiet, so a
	// detection cannot repeat off the tail of the same utterance.
	if s.recent[s.last] < s.cutoff {
		s.ignore = min(s.ignore+1, 0)
	}
}

// Fresh reports whether an invoke has happened since the last Detected.
func (s *Stream) Fresh() bool { return s.fresh }

// Detected reports whether the window is over the cutoff and the mute
// window has passed, and clears Fresh (ModelStream::detected).
func (s *Stream) Detected() bool {
	s.fresh = false
	if s.ignore < 0 {
		return false
	}
	var sum uint32
	for _, p := range s.recent {
		sum += uint32(p)
	}
	return sum > uint32(s.cutoff)*uint32(len(s.recent))
}

// Reset clears the window and starts the mute window again
// (ModelStream::reset_probabilities); the model's own state is kept.
func (s *Stream) Reset() {
	clear(s.recent)
	s.ignore = -MinSlicesBeforeDetection
}

// Step is one frame as the detector handles one enabled wake word model:
// feed it, and on a fresh invoke ask whether it fired. On a detection it
// returns the window's mean and highest probability, taken before the
// window is reset, as the firmware's detection report takes them
// (WakeDetector::process_features_, queue_detection_).
func (s *Stream) Step(frame []int8) (detected bool, average, highest uint8) {
	s.Feed(frame)
	if !s.fresh {
		return false, 0, 0
	}
	if !s.Detected() {
		return false, 0, 0
	}
	average, highest = s.Summarize()
	s.Reset()
	return true, average, highest
}

// Latest is the most recent invoke's probability.
func (s *Stream) Latest() uint8 { return s.recent[s.last] }

// Summarize is the window's rounded mean and its highest probability, as a
// detection reports them (ModelStream::summarize).
func (s *Stream) Summarize() (average, highest uint8) {
	var sum uint32
	for _, p := range s.recent {
		sum += uint32(p)
		highest = max(highest, p)
	}
	n := uint32(len(s.recent))
	return uint8((sum + n/2) / n), highest
}

// CutoffByte converts a manifest's probability cutoff (0.0-1.0) into the
// 0-255 the stream compares against, as the firmware's MODEL_START handler
// does: rounded, clamped (voice_incubator.cpp).
// The float is clamped before it is converted: Go's out-of-range conversion
// differs between amd64 and arm, and a NaN refuses to compare, so it is 0.
func CutoffByte(cutoff float32) uint8 {
	if !(cutoff > 0) {
		return 0
	}
	if cutoff >= 1 {
		return 255
	}
	return uint8(int(cutoff*255.0 + 0.5))
}
