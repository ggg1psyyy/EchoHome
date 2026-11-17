package incubator

import (
	"encoding/json"
	"log"
	"sync/atomic"
)

// Monitoring: the app's Test page listening to the Echo live (MONITOR
// {"active": true}), as the reference firmware streams it (set_monitoring_,
// send_wake_report_ at 64ce6c2a): its microphone outside calls as
// AUDIO_MONITOR, and about ten times a second a WAKE_REPORT of what each
// wake word model heard; a wake word heard meanwhile is reported, never
// answered.
//
// It streams the microphone continuously, so it is opt-in: HELLO says
// `monitor` only when the controller has been told to allow it
// (Config.Monitor, the dashboard's per-Echo switch), and a MONITOR arriving
// otherwise is ignored. The intercom (MONITOR with intercom, the microphone to
// another satellite) is not offered and is ignored, as is the second capture
// (monitor_aec): the app needs that for enrolling speakers and recording wake
// words, and refuses both without it.
//
// Whether audio is leaving is one predicate, monitorWantLocked: asked,
// allowed, not muted, no call (a call streams its own audio). The Echo reports
// it (incubator_state monitored) before the first frame, the ring shows it,
// and every exit clears it: MONITOR off, the app gone, mute, a call, the
// switch (which restarts the satellite).

// monitorQueue is how many frames may wait for the socket: past it they are
// dropped and counted, never blocking the worker (and the detector behind it)
// on an app that stalls.
const monitorQueue = 32

type outFrame struct {
	m Message
	p []byte
}

// monitor is a session's monitoring state that is not under s.mu.
type monitor struct {
	q        chan outFrame
	quit     chan struct{}
	done     chan struct{}
	dropped  atomic.Int64
	on       atomic.Bool // frames may go: set only once the Echo has reported it
	fresh    atomic.Bool // it just started: the stale report and detections go
	detected []string    // the worker's: detections since the last report
}

func newMonitor() *monitor {
	return &monitor{q: make(chan outFrame, monitorQueue), quit: make(chan struct{}), done: make(chan struct{})}
}

// monitorLoop sends the monitor's frames, off the worker.
func (s *session) monitorLoop() {
	defer close(s.mon.done)
	for {
		select {
		case <-s.mon.quit:
			return
		case f := <-s.mon.q:
			for p := f.p; ; {
				n := min(len(p), MaxPayload)
				if s.w.frame(f.m, p[:n]) != nil {
					break
				}
				if p = p[n:]; len(p) == 0 {
					break
				}
			}
		}
	}
}

func (s *session) monitorPush(m Message, p []byte) {
	select {
	case s.mon.q <- outFrame{m, p}:
	default:
		s.mon.dropped.Add(int64(len(p)))
	}
}

// monitorWantLocked is whether the microphone streams to the app now.
// s.mu held.
func (s *session) monitorWantLocked() bool {
	return s.allowMonitor && s.monitorAsked && !s.muted && !s.closed && s.call == nil
}

// monitoring says whether the Echo is streaming to the app (what it reports).
func (s *session) monitoring() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.monitored
}

// updateMonitor brings monitoring in line with the predicate after any
// change, without the lock held: off at once; on only once the Echo has
// reported it, so the controller hears that audio is leaving before any does.
func (s *session) updateMonitor() {
	s.mu.Lock()
	want := s.monitorWantLocked()
	changed := want != s.monitored
	s.monitored = want
	if !want {
		s.mon.on.Store(false)
	}
	s.syncMicLocked()
	s.showLocked()
	s.mu.Unlock()
	if changed {
		if want {
			s.mon.fresh.Store(true)
			log.Printf("[incubator] monitoring started: the microphone is streamed to the app")
		} else {
			if n := s.mon.dropped.Swap(0); n > 0 {
				log.Printf("[incubator] monitoring stopped, %d bytes dropped", n)
			} else {
				log.Printf("[incubator] monitoring stopped")
			}
		}
		s.report()
	}
	// Under the lock again, so a change that came meanwhile (a call, a
	// mute) is never overwritten with a stale "on".
	s.mu.Lock()
	s.mon.on.Store(s.monitored)
	s.mu.Unlock()
}

// monitorFrame is MONITOR from the app.
func (s *session) monitorFrame(p []byte) {
	var m MonitorInfo
	if err := json.Unmarshal(p, &m); err != nil {
		return
	}
	asked := m.Active
	switch {
	case m.Active && !s.allowMonitor:
		log.Printf("[incubator] MONITOR ignored: listening live is not allowed on this Echo")
		return
	case m.Active && m.Intercom:
		// Not offered, and the app sends a monitored Echo's audio to the
		// other satellite once it asks: a stream already going stops.
		log.Printf("[incubator] MONITOR with the intercom: not offered, monitoring stops")
		asked = false
	}
	s.mu.Lock()
	s.monitorAsked = asked
	s.mu.Unlock()
	s.updateMonitor()
}

// monitorChunk is the worker's part, for one chunk: before the detector, the
// audio; after it, the report. The detections it made are in found.
func (s *session) monitorAudio(pcm []byte) {
	s.monitorPush(AudioMonitor, pcm)
}

// monitorReport sends WAKE_REPORT: each model's peak and window since the
// last, and what was detected meanwhile (reported, not answered).
func (s *session) monitorReport() {
	w := s.wake
	w.detMu.Lock()
	reports := w.det.TakeReport()
	r := wakeReport{Models: make([]wakeReportModel, 0, len(reports)), Detected: s.mon.detected}
	for _, m := range reports {
		r.Models = append(r.Models, wakeReportModel{ID: m.ID, VAD: m.IsVAD, Cutoff: m.Cutoff, Peak: m.Peak, Window: m.Window})
	}
	w.detMu.Unlock()
	if r.Detected == nil {
		r.Detected = []string{}
	}
	s.mon.detected = nil
	if b, err := json.Marshal(r); err == nil {
		s.monitorPush(WakeReport, b)
	}
}

// monitorStart drops what was heard before monitoring started: the report's
// maxima and detections (set_monitoring_ takes a stale report).
func (s *session) monitorStart() {
	w := s.wake
	w.detMu.Lock()
	w.det.TakeReport()
	w.detMu.Unlock()
	s.mon.detected = nil
}

type wakeReport struct {
	Models   []wakeReportModel `json:"models"`
	Detected []string          `json:"detected"`
}

type wakeReportModel struct {
	ID     string `json:"id"`
	VAD    bool   `json:"vad,omitempty"`
	Cutoff uint8  `json:"cutoff"`
	Peak   uint8  `json:"peak"`
	Window uint8  `json:"window"`
}

// stopMonitor ends the sender: the session is over.
func (s *session) stopMonitor() {
	close(s.mon.quit)
	<-s.mon.done
}
