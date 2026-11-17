// Ported from firmware-voice-incubator's WakeDetector (wake_detector.h and
// wake_detector.cpp: begin_model .. finish_model, set_stop_armed,
// process_features_) at commit 64ce6c2aa9f17b779293626a76d36fd9169e06d8,
// Apache-2.0 (LICENSE beside this file).

package mixednet

import "errors"

const (
	// MaxModelSlots is how many models a detector holds (MAX_MODEL_SLOTS).
	MaxModelSlots = 6
	// MaxModelBytes is the largest blob a detector takes (MAX_MODEL_BYTES).
	MaxModelBytes = 512 * 1024
	// StopModelID is the model that runs only while armed (STOP_MODEL_ID).
	StopModelID = "stop"
)

// ModelConfig is what the app's MODEL_START says of a model.
type ModelConfig struct {
	ID       string
	WakeWord string
	IsVAD    bool
	Cutoff   uint8 // 0-255; CutoffByte converts a manifest's
	Window   uint8 // sliding window, in invokes
	StepMs   uint8 // feature step the model was trained on
}

// Detection is one wake word heard: which model, and the window's mean and
// highest probability at that moment (queue_detection_).
type Detection struct {
	Index    int
	ID       string
	WakeWord string
	Average  uint8
	Highest  uint8
}

type detModel struct {
	cfg     ModelConfig
	stream  *Stream
	enabled bool
}

// Detector runs up to MaxModelSlots models over one stream of feature
// frames, as the firmware's WakeDetector does: wake word models fire on a
// fresh invoke whose window passes its cutoff; a VAD model, when one is
// loaded, gates them (a wake word heard while it says no voice is
// suppressed, and its window is kept); the "stop" model runs only while
// armed. Not safe for concurrent use; Process allocates nothing.
type Detector struct {
	stepMs    uint8
	models    []*detModel
	stopArmed bool
	frames    uint32
	found     []Detection
}

// NewDetector is a detector for a front end with the given feature step.
func NewDetector(stepMs uint8) *Detector {
	return &Detector{stepMs: stepMs, found: make([]Detection, 0, MaxModelSlots)}
}

// The firmware's refusals (begin_model, finish_model), its words where it
// has them.
var (
	errModelSize   = errors.New("model size out of range")
	errStep        = errors.New("model wants another feature step")
	errWindow      = errors.New("a sliding window of zero")
	errFeatureSize = errors.New("unexpected feature size")
	errTooMany     = errors.New("too many models")
)

// Load parses and adds a model, replacing one with the same ID, as
// begin_model, append_model and finish_model do between them. On an error
// the detector is unchanged.
func (d *Detector) Load(cfg ModelConfig, blob []byte) error {
	if len(blob) == 0 || len(blob) > MaxModelBytes {
		return errModelSize
	}
	if cfg.StepMs != d.stepMs {
		return errStep
	}
	if cfg.Window == 0 {
		return errWindow
	}
	m, err := Parse(blob)
	if err != nil {
		return err
	}
	if m.FeatureSize() != StreamFeatureSize {
		return errFeatureSize
	}
	s, err := NewStream(m, cfg.Cutoff, cfg.Window)
	if err != nil {
		return err
	}
	dm := &detModel{cfg: cfg, stream: s, enabled: cfg.ID != StopModelID || d.stopArmed}
	for i, old := range d.models {
		if old.cfg.ID == cfg.ID {
			d.models[i] = dm
			return nil
		}
	}
	if len(d.models) >= MaxModelSlots {
		return errTooMany
	}
	d.models = append(d.models, dm)
	return nil
}

// Clear drops every model (clear_models; the app sends MODEL_CLEAR on each
// connect).
func (d *Detector) Clear() { d.models = d.models[:0] }

// Models is how many are loaded.
func (d *Detector) Models() int { return len(d.models) }

// SetStopArmed enables the "stop" model, from a clean window and mute, or
// disables it (set_stop_armed: armed while a reply plays or a timer rings).
func (d *Detector) SetStopArmed(armed bool) {
	d.stopArmed = armed
	for _, m := range d.models {
		if m.cfg.ID != StopModelID || m.enabled == armed {
			continue
		}
		if armed {
			m.stream.Reset()
		}
		m.enabled = armed
	}
}

// Process takes one feature frame and returns what it detected
// (process_features_). The slice is reused by the next call.
func (d *Detector) Process(frame []int8) []Detection {
	d.frames++
	d.found = d.found[:0]
	voice, haveVAD := true, false
	for _, m := range d.models {
		if !m.enabled {
			continue
		}
		m.stream.Feed(frame)
		if m.cfg.IsVAD {
			// Level-triggered: read every frame, not only on a fresh invoke.
			haveVAD = true
			var sum uint32
			for _, p := range m.stream.recent {
				sum += uint32(p)
			}
			voice = sum > uint32(m.cfg.Cutoff)*uint32(m.cfg.Window)
			m.stream.fresh = false
		}
	}
	for i, m := range d.models {
		if !m.enabled || m.cfg.IsVAD || !m.stream.Fresh() {
			continue
		}
		if !m.stream.Detected() {
			continue
		}
		if haveVAD && !voice {
			continue // suppressed by the VAD; the window is kept
		}
		avg, top := m.stream.Summarize()
		d.found = append(d.found, Detection{Index: i, ID: m.cfg.ID, WakeWord: m.cfg.WakeWord, Average: avg, Highest: top})
		m.stream.Reset()
	}
	return d.found
}
