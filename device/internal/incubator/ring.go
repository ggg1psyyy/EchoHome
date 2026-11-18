package incubator

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// Ring is what the Echo's ring shows of the satellite, derived from the
// session's state in one place (ringLocked) and handed to the device only on
// a change, under the session's lock, so it reaches the ring in order: a late
// "off" from one call cannot wipe the next call's ring.
//
// It shows what the Echo shows under Home Assistant (em_scenes: listening,
// the spinner while the command is worked on, the meter while the reply
// sounds, a flash for the outcome; em_timers' amber pulse for a ringing
// timer; the meter for an announcement, em_scenes.announcement_ring), and
// what the VPE shows of the app beyond that (devices/voice-pe.yaml at
// 64ce6c2a): the running timers between calls (timer_progress), and night
// mode (night: dim and warm, a ringing timer aside, and a command's outcome
// as a green pulse or red blinks, since the app says nothing at night).
type Ring int

const (
	RingOff Ring = iota
	RingListening
	RingThinking     // the command ended; the answer has not begun
	RingSpeaking     // a call's answer is sounding
	RingTimer        // a timer rings here
	RingAnnouncement // an announcement is playing
	RingMonitored    // the app is listening live: the microphone streams
	RingTimers       // running timers, between calls (timer_progress)
	// The outcome, shown once as a call ends and cleared by its own short
	// TTL: done (no reply was needed) or error (a timeout, a failed result).
	RingDone
	RingError
)

var ringNames = [...]string{"off", "listening", "thinking", "speaking", "timer", "announcement", "monitored", "timers", "done", "error"}

func (r Ring) String() string {
	if r >= 0 && int(r) < len(ringNames) {
		return ringNames[r]
	}
	return "?"
}

// Ducks says whether music is ducked under this state: whenever the
// satellite is doing something a person listens to or speaks into, as a
// voice turn ducks it under Home Assistant from the wake to the end of the
// reply. Dark and the outcome flashes are not.
func (r Ring) Ducks() bool {
	return r != RingOff && r != RingDone && r != RingError && r != RingMonitored && r != RingTimers
}

// RingView is what the device paints: the state, night mode, and for
// RingTimers the timers.
type RingView struct {
	State  Ring
	Night  bool
	Timers TimerView
}

// TimerView is the app's last timer_progress (timers.progress): up to
// TimerLayers running timers, soonest first, each its time left and its
// color (0xRRGGBB; -1 the ring's own), on the scale of the longest; as of At,
// counted down from then by the device.
type TimerView struct {
	Lefts  []float64 // seconds
	Colors []int
	Scale  float64 // seconds, at least 1
	Paused bool
	At     time.Time
}

// TimerLayers is how many timers the ring shows at once (the app's LAYERS).
const TimerLayers = 6

// parseTimerProgress reads timer_progress, clamped: a count, times and a
// scale it can draw, colors it can paint. shown 0 is none.
func parseTimerProgress(data json.RawMessage, now time.Time) TimerView {
	var d map[string]any
	if json.Unmarshal(data, &d) != nil {
		return TimerView{}
	}
	num := func(k string, def float64) float64 {
		v, ok := d[k].(float64)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			return def
		}
		return v
	}
	if num("shown", 0) == 0 {
		return TimerView{}
	}
	n := int(num("count", num("shown", 0)))
	n = max(0, min(n, TimerLayers))
	v := TimerView{At: now, Paused: num("paused", 0) != 0}
	remaining := max(num("remaining", 0), 0)
	v.Scale = max(num("scale", num("total", 1)), 1)
	for k := 0; k < n; k++ {
		v.Lefts = append(v.Lefts, max(num("left"+strconv.Itoa(k), remaining), 0))
		c := int(num("color"+strconv.Itoa(k), -1))
		if c < -1 || c > 0xFFFFFF {
			c = -1
		}
		v.Colors = append(v.Colors, c)
	}
	return v
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
	case s.monitored:
		return RingMonitored // above a timer and an announcement: audio is leaving
	case s.timerRings:
		return RingTimer
	case s.announcing:
		return RingAnnouncement
	case len(s.timers.Lefts) > 0:
		return RingTimers
	}
	return RingOff
}

// repaint paints the ring again whatever was shown: something else held it
// (the link ring standing aside for a standalone satellite).
func (s *session) repaint() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shownKey = shownKey{r: -1}
	s.flashing = false
	s.showLocked()
}

// shownKey is what was painted: a repaint only when it changes (new timer
// data matters only while the timers are shown).
type shownKey struct {
	r      Ring
	night  bool
	timers int
}

// showLocked hands the ring its state if it, night mode, or the timers it
// shows changed. s.mu held.
func (s *session) showLocked() {
	r := s.ringLocked()
	if s.flashing && (r == RingOff || r == RingTimers) {
		return // the outcome flash stays over what is under it, for its length
	}
	s.flashing = false
	k := shownKey{r: r, night: s.night}
	if r == RingTimers {
		k.timers = s.timersGen
	}
	if k == s.shownKey {
		return
	}
	s.shown, s.shownKey = r, k
	s.dev.Show(s.viewLocked(r))
}

func (s *session) viewLocked(r Ring) RingView {
	v := RingView{State: r, Night: s.night}
	if r == RingTimers {
		v.Timers = s.timers
	}
	return v
}

// flashFor is how long an outcome flash lasts: the device clears it itself
// by then, and the satellite then paints what is under it (the timers).
func flashFor(r Ring, night bool) time.Duration {
	if r == RingError && night {
		return 600 * time.Millisecond
	}
	return time.Second
}

// outcomeLocked shows how the call just ended, unless something else is
// to be shown now (a reply still sounding, a ringing timer): s.mu held,
// the call already gone. The flash clears itself; the ring is then dark,
// which is what it records.
func (s *session) outcomeLocked(c *call, reason string) {
	if s.askBack {
		return // the answer's call follows at once: listening, with no flash or gap
	}
	if r := s.ringLocked(); (r != RingOff && r != RingTimers) || s.closed {
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
	case c.hangup == "error" || c.hangup == "timeout" || (s.night && c.hangup == "not_recognized"):
		flash = RingError
	case !c.spoke:
		flash = RingDone // done, nothing said: an acknowledgement
	}
	if flash == RingOff {
		s.showLocked()
		return
	}
	// Shown over whatever is under it (the VPE's show_result), then that is
	// painted again: dark, or the running timers.
	s.shown, s.shownKey = flash, shownKey{r: flash, night: s.night}
	s.dev.Show(RingView{State: flash, Night: s.night})
	s.flashing = true
	s.flashGen++
	g := s.flashGen
	time.AfterFunc(flashFor(flash, s.night)+100*time.Millisecond, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.flashGen != g || !s.flashing {
			return // something else was shown meanwhile
		}
		s.flashing = false
		if s.ringLocked() == RingOff {
			// The flash went dark by itself: that is what is under it.
			s.shown, s.shownKey = RingOff, shownKey{r: RingOff, night: s.night}
			return
		}
		s.showLocked()
	})
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
