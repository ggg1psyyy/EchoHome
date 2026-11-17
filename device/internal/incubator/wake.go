package incubator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wilbowes/EchoMuse/internal/microfeatures"
	"github.com/wilbowes/EchoMuse/internal/mixednet"
)

// The satellite's wake words: the models the app pushes (MODEL_CLEAR, then
// for each MODEL_START, MODEL_DATA and MODEL_END), run over the microphone
// by internal/mixednet on internal/microfeatures' features, as the reference
// firmware's WakeDetector runs them (voice_incubator.cpp handle_model_start_,
// handle_model_end_, poll_wake_detector_, start_call; commit
// 64ce6c2aa9f17b779293626a76d36fd9169e06d8). A detection starts a call whose
// pre-roll is the audio before it.
//
// One path carries the microphone while it is open: the device's callback
// hands each chunk to the session's worker (never blocking: a full queue
// drops the chunk and counts it), and the worker, in order, keeps the
// pre-roll, passes the chunk to a listening call, and runs the detector.
// Inference is never on the microphone path, and a call started by a
// detection begins with the audio up to the chunk that detected it, the next
// chunk following as live audio, with nothing lost or repeated between.

const (
	// PrerollMs is how much audio from before a detection a wake call starts
	// with: the reference firmware's default. The app verifies the wake word
	// on it (voice_incubator server.py _verify_wake), so it must hold the
	// whole word.
	PrerollMs = 1500
	// ModelSlots is how many models the satellite takes: four were measured
	// on an Echo (12.3% of a core, docs/history/device.md), beside the
	// Echo's own wake word; the executor's six never were.
	ModelSlots = 4
	// FeatureStepMs is the front end's feature step, which a model must have
	// been trained on.
	FeatureStepMs = microfeatures.DefaultStepMs

	prerollBytes = PrerollMs * BytesPerSecond / 1000
	// wakeQueue is how many 80 ms chunks may wait for the worker: about 1.3 s.
	wakeQueue = 16
)

// The reference firmware's MODEL_START defaults, for a header that leaves a
// field out (handle_model_start_).
const (
	defaultCutoff = 0.85
	defaultWindow = 5
	defaultStep   = 10
)

// heard is one chunk from the microphone: the opening it came in, and its
// number.
type heard struct {
	opening uint32
	n       uint64
	pcm     []byte
}

// staged is the model being received.
type staged struct {
	id   string
	ok   bool
	cfg  mixednet.ModelConfig
	size int
	sha  string
	blob []byte
}

// wake is a session's wake word state. The detector and front end are the
// worker's, and a load or a clear from the reading goroutine; detMu is
// between them. ring is the worker's alone.
type wake struct {
	q       chan heard
	quit    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
	models  atomic.Int32
	// opened counts the microphone's openings; each chunk carries the one
	// it was heard in, and the worker starts the pre-roll over at a new one
	// and drops a chunk from an earlier one (queued before a close).
	opened atomic.Uint32
	at     uint32 // the worker's: the opening its pre-roll is from
	// heard numbers the chunks as the microphone delivers them; a call
	// takes only those after its start (a manual call: after the press).
	heard atomic.Uint64

	detMu sync.Mutex
	det   *mixednet.Detector
	fe    *microfeatures.Frontend

	ring    []byte
	samples []int16
	found   []mixednet.Detection

	stage staged // the reading goroutine's
}

func newWake() *wake {
	w := &wake{
		q:     make(chan heard, wakeQueue),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
		det:   mixednet.NewDetector(FeatureStepMs),
		ring:  make([]byte, 0, prerollBytes+4096),
		found: make([]mixednet.Detection, 0, mixednet.MaxModelSlots),
	}
	// HELLO says no stop_word, so the app pushes "stop" only for a ringing
	// timer and clears it after (server.py _ring, _after_ring), expecting it
	// to run while loaded: armed throughout. Heard outside a call it starts
	// one (wake_word "stop"), which the app reads as stopping the ring; in a
	// call it is ignored, as any wake word is.
	w.det.SetStopArmed(true)
	return w
}

// onMic is the device's microphone callback: it must not block.
func (s *session) onMic(pcm []byte) {
	select {
	case s.wake.q <- heard{s.wake.opened.Load(), s.wake.heard.Add(1), append([]byte(nil), pcm...)}:
	default:
		s.wake.dropped.Add(int64(len(pcm)))
	}
}

// wakeLoop is the worker: every chunk the microphone delivers, in order.
func (s *session) wakeLoop() {
	defer close(s.wake.done)
	for {
		select {
		case <-s.wake.quit:
			return
		case h := <-s.wake.q:
			s.chunk(h)
		}
	}
}

func (s *session) chunk(h heard) {
	w := s.wake
	if h.opening != w.opened.Load() {
		return // heard before the microphone last closed
	}
	pcm := h.pcm
	if h.opening != w.at {
		// What the pre-roll holds is from before the microphone last
		// closed (a mute, say): not the audio before a detection now, and
		// it does not leave.
		w.at = h.opening
		w.ring = w.ring[:0]
		w.detMu.Lock()
		if w.fe != nil {
			w.fe.Reset()
		}
		w.det.Reset()
		w.detMu.Unlock()
	}
	w.ring = append(w.ring, pcm...)
	if over := len(w.ring) - prerollBytes; over > 0 {
		w.ring = append(w.ring[:0], w.ring[over:]...)
	}

	s.mu.Lock()
	c := s.call
	listening := c != nil && c.listening && h.n > c.from
	s.mu.Unlock()
	if listening {
		c.push(pcm)
	}

	w.detMu.Lock()
	if w.det.Models() == 0 || w.fe == nil {
		w.detMu.Unlock()
		return
	}
	n := len(pcm) / 2
	if cap(w.samples) < n {
		w.samples = make([]int16, n)
	}
	samples := w.samples[:n]
	for i := range samples {
		samples[i] = int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
	}
	w.found = w.found[:0]
	w.fe.Feed(samples, func(fr *[microfeatures.FeatureSize]int8) {
		w.found = append(w.found, w.det.Process(fr[:])...)
	})
	w.detMu.Unlock()
	for _, d := range w.found {
		s.wakeHeard(d, h.n)
	}
}

// wakeHeard is a detection: a call, unless one is going on (a wake word
// heard during a call is ignored), the Echo is muted, or the app went.
func (s *session) wakeHeard(d mixednet.Detection, n uint64) {
	muted := s.dev.Muted()
	s.mu.Lock()
	if s.closed || s.call != nil || muted {
		s.mu.Unlock()
		why := "in a call"
		if muted {
			why = "muted"
		}
		log.Printf("[incubator] wake word %q heard, ignored (%s)", d.WakeWord, why)
		return
	}
	pre := append([]byte(nil), s.wake.ring...)
	p, m, cut := int(d.Average), int(d.Highest), int(d.Cutoff)
	info := CallStartInfo{WakeWord: d.WakeWord, PrerollBytes: len(pre),
		Probability: &p, MaxProbability: &m, Cutoff: &cut}
	started := s.startLocked(info, pre, n)
	s.mu.Unlock()
	if started {
		if n := s.wake.dropped.Swap(0); n > 0 {
			log.Printf("[incubator] %d microphone bytes dropped before this call", n)
		}
		log.Printf("[incubator] call started (wake word %q, average %d, max %d, cutoff %d, %d byte pre-roll)",
			d.WakeWord, p, m, cut, len(pre))
		s.dev.ListenStarted()
		s.report()
	}
}

// modelResult answers the model being received.
func (s *session) modelResult(err string) {
	r := ModelResultInfo{OK: err == "", ID: s.wake.stage.id, Models: int(s.wake.models.Load()), Error: err}
	s.w.json(ModelResult, r)
}

// modelStart is MODEL_START: refused as the reference firmware refuses it,
// or staged.
func (s *session) modelStart(p []byte) {
	h := ModelStartInfo{Format: "mixednet", Kind: "wake", ProbabilityCutoff: defaultCutoff,
		SlidingWindowSize: defaultWindow, FeatureStepSize: defaultStep}
	parsed := json.Unmarshal(p, &h) == nil
	st := &s.wake.stage
	*st = staged{id: h.ID}
	switch {
	case !parsed || h.ID == "" || h.Size <= 0:
		log.Printf("[incubator] malformed model header")
		s.modelResult("malformed header")
		return
	case h.Format != "mixednet":
		log.Printf("[incubator] model %q is %s, not mixednet", h.ID, h.Format)
		s.modelResult("unsupported format")
		return
	case h.Size > mixednet.MaxModelBytes, h.FeatureStepSize != FeatureStepMs,
		h.SlidingWindowSize <= 0, h.SlidingWindowSize > 255:
		log.Printf("[incubator] model %q refused: %d bytes, %d ms step, window %d",
			h.ID, h.Size, h.FeatureStepSize, h.SlidingWindowSize)
		s.modelResult("rejected")
		return
	}
	if h.WakeWord == "" {
		h.WakeWord = h.ID
	}
	*st = staged{id: h.ID, ok: true, size: h.Size, sha: strings.ToLower(h.SHA256),
		blob: make([]byte, 0, h.Size),
		cfg: mixednet.ModelConfig{ID: h.ID, WakeWord: h.WakeWord, IsVAD: h.Kind == "vad",
			Cutoff: mixednet.CutoffByte(float32(h.ProbabilityCutoff)),
			Window: uint8(h.SlidingWindowSize), StepMs: FeatureStepMs}}
	log.Printf("[incubator] receiving model %q (%d bytes)", h.ID, h.Size)
}

// modelData is MODEL_DATA: part of the staged blob.
func (s *session) modelData(p []byte) {
	st := &s.wake.stage
	if !st.ok {
		return
	}
	if len(st.blob)+len(p) > st.size {
		log.Printf("[incubator] model %q overran its declared size", st.id)
		st.ok, st.blob = false, nil
		return
	}
	st.blob = append(st.blob, p...)
}

var errSlots = errors.New("too many models")

// modelEnd is MODEL_END: the staged blob checked, parsed and loaded.
func (s *session) modelEnd() {
	st := &s.wake.stage
	if !st.ok {
		s.modelResult("no model staged")
		return
	}
	st.ok = false
	blob := st.blob
	st.blob = nil
	if len(blob) != st.size {
		log.Printf("[incubator] model %q: short transfer", st.id)
		s.modelResult("short transfer")
		return
	}
	if st.sha != "" {
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != st.sha {
			log.Printf("[incubator] model %q: sha256 mismatch", st.id)
			s.modelResult("sha256 mismatch")
			return
		}
	}
	if err := s.load(st.cfg, blob); err != nil {
		log.Printf("[incubator] model %q rejected: %v", st.id, err)
		s.modelResult(err.Error())
		return
	}
	log.Printf("[incubator] model %q loaded (%d model(s))", st.id, s.wake.models.Load())
	s.modelResult("")
	s.modelsChanged()
}

func (s *session) load(cfg mixednet.ModelConfig, blob []byte) error {
	w := s.wake
	w.detMu.Lock()
	defer w.detMu.Unlock()
	if w.fe == nil {
		fe, err := microfeatures.New(FeatureStepMs)
		if err != nil {
			return err
		}
		w.fe = fe
	}
	if w.det.Models() >= ModelSlots && !w.det.Has(cfg.ID) {
		return errSlots
	}
	if err := w.det.Load(cfg, blob); err != nil {
		return err
	}
	w.models.Store(int32(w.det.Models()))
	return nil
}

// modelClear is MODEL_CLEAR: every model dropped, and the microphone with
// them unless a call has it.
func (s *session) modelClear() {
	w := s.wake
	w.detMu.Lock()
	w.det.Clear()
	w.models.Store(0)
	w.detMu.Unlock()
	log.Printf("[incubator] wake word models cleared")
	s.modelsChanged()
}

// modelsChanged opens or closes the microphone for the models, and reports.
func (s *session) modelsChanged() {
	s.mu.Lock()
	s.syncMicLocked()
	s.mu.Unlock()
	s.report()
}

// stopWake ends the worker and frees the front end: the session is over
// and the microphone closed.
func (s *session) stopWake() {
	close(s.wake.quit)
	<-s.wake.done
	w := s.wake
	if n := w.dropped.Load(); n > 0 {
		// A worker that falls behind is otherwise said only at a wake call.
		log.Printf("[incubator] %d microphone bytes dropped since the last call", n)
	}
	w.detMu.Lock()
	defer w.detMu.Unlock()
	w.det.Clear()
	w.models.Store(0)
	if w.fe != nil {
		w.fe.Close()
		w.fe = nil
	}
}
