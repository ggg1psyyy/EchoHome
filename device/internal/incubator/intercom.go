package incubator

import (
	"log"
	"time"
)

// The intercom: the app joins two satellites (MONITOR {"active": true,
// "intercom": true}, PROTOCOL.md "Monitoring and the intercom"), each
// one's microphone on the other's speaker, until the button on either ends
// it (DEVICE_EVENT intercom_end). The Voice PE answers at once; the Echo
// rings and waits instead (the owner's choice): its microphone opens only
// after a tap here, so nothing of this room leaves without someone in it
// agreeing. Until then what the other side says is not played, the ring
// pulses green and a tone rings every intercomChimeEvery; unanswered after
// intercomAnswerWithin, or muted while it rings, it declines (intercom_end,
// and the app ends it on the other side too). Answered, the microphone
// streams as AUDIO_MONITOR (the first capture only, no WAKE_REPORT) and the
// Echo reports itself monitored, as live listening, so the privacy line
// says audio is leaving. Mute closes the microphone and keeps the call: the
// other side is still heard. Wake words are not answered meanwhile.
//
// The caller does not ring: the app sends both satellites the same MONITOR,
// and starts the intercom only once the call that asked for it has ended
// (INTERCOM_START_WAIT), with the callee refused while in a call of its own.
// So an Echo whose own call ended within intercomCallerWithin is the one
// that asked, someone having just spoken the command to it: it connects at
// once. The callee could only be taken for the caller if someone ended a
// call of their own on it in those same seconds, someone in its room.

type intercomState int

const (
	intercomNone intercomState = iota
	intercomRinging
	intercomConnected
)

var (
	intercomAnswerWithin = 30 * time.Second
	intercomChimeEvery   = 3 * time.Second
	intercomCallerWithin = 5 * time.Second
)

// intercomStartLocked is MONITOR with the intercom: ring. s.mu held.
func (s *session) intercomStartLocked() {
	if s.closed || s.intercom != intercomNone {
		return
	}
	s.intercom = intercomRinging
	s.monitorAsked = false // the intercom stands in for live listening
	if !s.callEnded.IsZero() && time.Since(s.callEnded) < intercomCallerWithin {
		s.intercom = intercomConnected
		s.intercomGen++
		log.Printf("[incubator] intercom: asked for here, connected")
		s.showLocked()
		return
	}
	gen := s.intercomGen + 1
	s.intercomGen = gen
	log.Printf("[incubator] intercom: ringing, waiting for a tap to answer")
	go s.intercomRing(gen)
	s.showLocked()
}

// intercomRing chimes until the call is answered or gone, and declines it
// when nobody answers in time.
func (s *session) intercomRing(gen int) {
	deadline := time.Now().Add(intercomAnswerWithin)
	for {
		s.mu.Lock()
		ringing := s.intercom == intercomRinging && s.intercomGen == gen && !s.closed
		s.mu.Unlock()
		if !ringing {
			return
		}
		if !time.Now().Before(deadline) {
			s.intercomDecline("nobody answered")
			return
		}
		s.dev.IntercomChime()
		time.Sleep(min(intercomChimeEvery, time.Until(deadline)))
	}
}

// intercomAnswer is a tap while it rings: the microphone opens.
func (s *session) intercomAnswerLocked() {
	s.intercom = intercomConnected
	s.intercomGen++
	log.Printf("[incubator] intercom: answered here, the microphone streams to the other satellite")
	s.showLocked()
}

// intercomEnd is the Echo ending or declining it: the app ends it on the
// other satellite too.
func (s *session) intercomEnd(why string) {
	s.mu.Lock()
	was := s.intercom
	s.intercom = intercomNone
	s.intercomGen++
	s.showLocked()
	s.mu.Unlock()
	if was == intercomNone {
		return
	}
	log.Printf("[incubator] intercom ended here (%s)", why)
	s.w.json(DeviceEvent, DeviceEventInfo{Type: "intercom_end"})
	s.updateMonitor()
	s.report()
}

func (s *session) intercomDecline(why string) {
	s.mu.Lock()
	ringing := s.intercom == intercomRinging
	s.mu.Unlock()
	if ringing {
		s.intercomEnd(why)
	}
}

// intercomStopLocked is the app's MONITOR off: no event back. s.mu held.
func (s *session) intercomStopLocked() {
	if s.intercom == intercomNone {
		return
	}
	s.intercom = intercomNone
	s.intercomGen++
	log.Printf("[incubator] intercom ended by the app")
	s.showLocked()
}

// intercomRinging says whether a called intercom waits for its answer.
func (s *session) intercomRinging() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intercom == intercomRinging
}

// intercomConnected says whether the microphone may go to the other side.
func (s *session) intercomOn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intercom == intercomConnected
}

// intercomName is what the state report says of it.
func (s *session) intercomName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return [...]string{"", "ringing", "connected"}[s.intercom]
}
