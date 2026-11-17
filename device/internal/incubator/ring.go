package incubator

import "encoding/json"

// Ring is what the Echo's ring shows of the satellite, derived from the
// session's state in one place (ringLocked) and handed to the device only on
// a change, under the session's lock, so it reaches the ring in order: a late
// "off" from one call cannot wipe the next call's ring.
//
// It shows what the Echo shows under Home Assistant (em_scenes: listening,
// the spinner while the command is worked on, the meter while the reply
// sounds, a flash for the outcome; em_timers' amber pulse for a ringing
// timer; the meter for an announcement, em_scenes.announcement_ring). The
// app's timer_progress and night events have no EchoMuse animation and are
// not shown.
type Ring int

const (
	RingOff Ring = iota
	RingListening
	RingThinking     // the command ended; the answer has not begun
	RingSpeaking     // a call's answer is sounding
	RingTimer        // a timer rings here
	RingAnnouncement // an announcement is playing
	// The outcome, shown once as a call ends and cleared by its own short
	// TTL: done (no reply was needed) or error (a timeout, a failed result).
	RingDone
	RingError
)

var ringNames = [...]string{"off", "listening", "thinking", "speaking", "timer", "announcement", "done", "error"}

func (r Ring) String() string {
	if r >= 0 && int(r) < len(ringNames) {
		return ringNames[r]
	}
	return "?"
}

// ringLocked is what the ring should show now. s.mu held.
func (s *session) ringLocked() Ring {
	c := s.call
	switch {
	case s.closed:
		return RingOff
	case c != nil && c.listening:
		return RingListening
	case s.replying:
		return RingSpeaking // its call may have ended: the answer still sounds
	case c != nil && !c.spoke:
		return RingThinking // the "heard" sound included
	case c != nil:
		return RingOff // the answer played; the hang-up is awaited
	case s.timerRings:
		return RingTimer
	case s.announcing:
		return RingAnnouncement
	}
	return RingOff
}

// showLocked hands the ring its state if it changed. s.mu held.
func (s *session) showLocked() {
	if r := s.ringLocked(); r != s.shown {
		s.shown = r
		s.dev.Show(r)
	}
}

// outcomeLocked shows how the call just ended, unless something else is
// to be shown now (a reply still sounding, a ringing timer): s.mu held,
// the call already gone. The flash clears itself; the ring is then dark,
// which is what it records.
func (s *session) outcomeLocked(c *call, reason string) {
	if s.askBack {
		return // the answer's call follows at once: listening, with no flash or gap
	}
	if r := s.ringLocked(); r != RingOff || s.closed {
		s.showLocked()
		return
	}
	var flash Ring
	switch {
	case reason == "timeout" || c.failed:
		flash = RingError
	case reason != "":
		// cancelled: dark
	case quietHangup[c.hangup]:
		// no call to speak of: the wake word refused, answered elsewhere,
		// a timer stopped
	case c.hangup == "error" || c.hangup == "timeout":
		flash = RingError
	case !c.spoke:
		flash = RingDone // done, nothing said: an acknowledgement
	}
	if flash == RingOff {
		s.showLocked()
		return
	}
	s.shown = RingOff
	s.dev.Show(flash)
}

// quietHangup are the app's HANGUP reasons (server.py _hang_up) after which
// there is no outcome to show.
var quietHangup = map[string]bool{
	"wake_rejected": true, "answered_elsewhere": true, "timer_stopped": true,
	"nothing_to_stop": true, "no_answer": true, "nothing_asked": true,
}

// resultFailed reads a `result` EVENT's ok.
func resultFailed(data json.RawMessage) bool {
	var d struct {
		OK *bool `json:"ok"`
	}
	return json.Unmarshal(data, &d) == nil && d.OK != nil && !*d.OK
}
