package incubator

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// rings waits for the ring to have been given want, in order, and nothing
// after it.
func rings(t *testing.T, echo *fakeEcho, want ...Ring) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(echo.shown()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := echo.shown(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the ring was given %v, want %v", got, want)
	}
}

func event(a *app, typ, data string) {
	if data == "" {
		a.send(Event, []byte(`{"type":"`+typ+`"}`))
		return
	}
	a.send(Event, []byte(`{"type":"`+typ+`","data":`+data+`}`))
}

// The app's own order (voice_incubator server.py _respond): the "heard"
// sound the moment the command ends, then `processing`, then `responding`
// and the answer, in the same stream as the sound.
func TestTheRingFollowsACall(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	rings(t, echo, RingListening)
	a.send(AudioOut, make([]byte, 4000)) // the heard sound: the command ended
	rings(t, echo, RingListening, RingThinking)
	event(a, "processing", "")
	event(a, "recognized", `{"text":"x"}`)
	rings(t, echo, RingListening, RingThinking)
	event(a, "responding", `{"text":"hi"}`) // before its sound: still thinking
	rings(t, echo, RingListening, RingThinking)
	a.send(AudioOut, make([]byte, 4000))
	rings(t, echo, RingListening, RingThinking, RingSpeaking)
	a.send(AudioOutEnd, nil)
	waitFor(t, "the stream's end", func() bool { return echo.count(&echo.ends) == 1 })
	s.Drained() // the speaker's report
	a.expect(PlaybackDrained)
	rings(t, echo, RingListening, RingThinking, RingSpeaking, RingOff)
	a.send(Hangup, []byte(`{"reason":"done"}`))
	rings(t, echo, RingListening, RingThinking, RingSpeaking, RingOff)
}

func TestTheRingShowsHowACallEnded(t *testing.T) {
	cases := []struct {
		name string
		run  func(s *Server, a *app)
		want []Ring
	}{
		{"done, nothing said", func(s *Server, a *app) {
			event(a, "processing", "")
			event(a, "result", `{"ok":true}`)
			a.send(Hangup, nil)
		}, []Ring{RingListening, RingThinking, RingDone}},
		{"a failed result", func(s *Server, a *app) {
			event(a, "processing", "")
			event(a, "result", `{"ok":false}`)
			a.send(Hangup, nil)
		}, []Ring{RingListening, RingThinking, RingError}},
		{"the wake word rejected", func(s *Server, a *app) {
			event(a, "wake_rejected", `{"reason":"x"}`)
			a.send(Hangup, []byte(`{"reason":"wake_rejected"}`))
		}, []Ring{RingListening, RingOff}},
		{"answered elsewhere", func(s *Server, a *app) {
			a.send(Hangup, []byte(`{"reason":"answered_elsewhere"}`))
		}, []Ring{RingListening, RingOff}},
		{"the app's error", func(s *Server, a *app) {
			a.send(AudioOut, make([]byte, 4000)) // heard, then "something went wrong"
			a.send(AudioOut, make([]byte, 4000))
			a.send(AudioOutEnd, nil)
			a.send(Hangup, []byte(`{"reason":"error"}`))
		}, []Ring{RingListening, RingThinking, RingError}},
		{"not understood, said", func(s *Server, a *app) {
			event(a, "processing", "")
			event(a, "not_recognized", `{"heard":"x"}`)
			event(a, "responding", `{"text":"sorry"}`)
			a.send(AudioOut, make([]byte, 4000))
			a.send(AudioOutEnd, nil)
			time.Sleep(50 * time.Millisecond)
			s.Drained()
			a.expect(PlaybackDrained)
			a.send(Hangup, []byte(`{"reason":"not_recognized"}`))
		}, []Ring{RingListening, RingThinking, RingSpeaking, RingOff}},
		{"cancelled while listening", func(s *Server, a *app) {
			s.Press()
			a.expect(CallEnd)
		}, []Ring{RingListening, RingOff}},
		{"cancelled during the reply", func(s *Server, a *app) {
			event(a, "processing", "")
			event(a, "responding", `{"text":"hi"}`)
			a.send(AudioOut, make([]byte, 4000))
			time.Sleep(50 * time.Millisecond)
			s.Press()
			a.expect(CallEnd)
		}, []Ring{RingListening, RingThinking, RingSpeaking, RingOff}},
		{"muted", func(s *Server, a *app) {
			s.SetMuted(true)
			a.expect(CallEnd)
		}, []Ring{RingListening, RingOff}},
		{"the app gone", func(s *Server, a *app) {
			event(a, "processing", "")
			time.Sleep(50 * time.Millisecond)
			a.c.Close()
		}, []Ring{RingListening, RingThinking, RingOff}},
		{"an unknown event", func(s *Server, a *app) {
			event(a, "something_new", `{"x":1}`)
		}, []Ring{RingListening}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, echo, _, a := connectApp(t)
			s.Press()
			a.expect(CallStart)
			c.run(s, a)
			rings(t, echo, c.want...)
		})
	}
}

func TestATimeoutShowsAnError(t *testing.T) {
	old := listenLimit
	listenLimit = 50 * time.Millisecond
	defer func() { listenLimit = old }()
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	a.expect(CallEnd)
	rings(t, echo, RingListening, RingError)
}

func TestTheRingShowsATimerAndAnAnnouncement(t *testing.T) {
	_, echo, _, a := connectApp(t)
	event(a, "timer_ringing", `{"timers":1,"name":""}`)
	rings(t, echo, RingTimer)
	event(a, "timer_stopped", "")
	rings(t, echo, RingTimer, RingOff)
	event(a, "announcement", `{"active":true}`)
	rings(t, echo, RingTimer, RingOff, RingAnnouncement)
	event(a, "announcement", `{"active":false}`)
	rings(t, echo, RingTimer, RingOff, RingAnnouncement, RingOff)
}

func TestATimerRingingDuringACallShowsWhenItEnds(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	event(a, "timer_ringing", `{"timers":1}`)
	rings(t, echo, RingListening) // the call keeps the ring
	event(a, "processing", "")
	a.send(Hangup, nil)
	rings(t, echo, RingListening, RingThinking, RingTimer) // no outcome flash over it
}

func TestAPressStoppingATimerClearsItsRing(t *testing.T) {
	s, echo, _, a := connectApp(t)
	event(a, "timer_ringing", `{"timers":1}`)
	rings(t, echo, RingTimer)
	s.Press()
	a.expect(DeviceEvent)
	rings(t, echo, RingTimer, RingOff)
}

func TestAReplyStillSoundingAtTheHangUpKeepsTheMeter(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	event(a, "responding", `{"text":"hi"}`)
	a.send(AudioOut, make([]byte, 4000))
	a.send(AudioOutEnd, nil)
	a.send(Hangup, nil) // before the speaker's report
	rings(t, echo, RingListening, RingThinking, RingSpeaking)
	waitFor(t, "the stream's end", func() bool { return echo.count(&echo.ends) == 1 })
	s.Drained()
	rings(t, echo, RingListening, RingThinking, RingSpeaking, RingOff)
}

// A press just after a cancel: the cancelled reply's tail and its end still
// arrive, and must neither close the new call's microphone nor leave the
// discard on for the new call's reply.
func TestACancelledReplysTailDoesNotTouchTheNextCall(t *testing.T) {
	s, echo, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	event(a, "responding", `{"text":"hi"}`)
	a.send(AudioOut, make([]byte, 4000))
	waitFor(t, "the reply playing", func() bool { return echo.periods() > 0 })
	s.Press() // cancel
	a.expect(CallEnd)
	s.Press() // and ask again at once
	a.expect(CallStart)
	a.send(AudioOut, make([]byte, 4000)) // the cancelled reply's tail
	a.send(AudioOutEnd, nil)
	a.expect(PlaybackDrained) // the app's, for its cancelled stream
	a.nothing(100 * time.Millisecond)
	if !st.last().InCall {
		t.Fatal("the cancelled reply's tail closed the new call's microphone")
	}
	before := echo.periods()
	a.send(AudioOut, make([]byte, 4000)) // the new call's heard sound
	event(a, "processing", "")
	event(a, "responding", `{"text":"again"}`)
	a.send(AudioOut, make([]byte, 4000))
	waitFor(t, "the new call's reply", func() bool { return echo.periods() > before })
}

// Cancelled before its reply played anything: no stream is open when the
// next call starts (nothing makes it stale), so only the discard keeps the
// cancelled reply's frames, sent before the app saw the cancel, from
// closing the new call's microphone.
func TestARepliesFramesAfterACancelBeforeItPlayedAreDiscarded(t *testing.T) {
	s, _, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	waitFor(t, "the command to end", func() bool { return !st.last().InCall })
	s.Press() // cancel while thinking
	a.expect(CallEnd)
	s.Press()
	a.expect(CallStart)
	waitFor(t, "the new call listening", func() bool { return st.last().InCall })
	a.send(AudioOut, make([]byte, 4000)) // the cancelled call's answer, in flight
	a.nothing(150 * time.Millisecond)
	if !st.last().InCall {
		t.Fatal("the cancelled call's answer closed the new call's microphone")
	}
}

func TestMusicIsDuckedFromListeningToTheEndOfTheAnswer(t *testing.T) {
	want := map[Ring]bool{RingOff: false, RingListening: true, RingThinking: true,
		RingSpeaking: true, RingTimer: true, RingAnnouncement: true, RingDone: false, RingError: false,
		RingMonitored: false, RingTimers: false}
	for r, d := range want {
		if r.Ducks() != d {
			t.Errorf("%v ducks %v, want %v", r, r.Ducks(), d)
		}
	}
}

// Muted, a tap still stops a ringing timer (the reference's order: the
// timer before mute), and otherwise does nothing.
func TestAMutedPressStopsARingingTimer(t *testing.T) {
	s, echo, _, a := connectApp(t)
	echo.mu.Lock()
	echo.muted = true
	echo.mu.Unlock()
	event(a, "timer_ringing", `{"timers":1}`)
	rings(t, echo, RingTimer)
	s.Press()
	var de DeviceEventInfo
	json.Unmarshal(a.expect(DeviceEvent), &de)
	if de.Type != "stop_timer" {
		t.Fatalf("DEVICE_EVENT %+v", de)
	}
	s.Press() // nothing rings now: muted, nothing happens
	a.nothing(200 * time.Millisecond)
	if echo.count(&echo.opens) != 0 {
		t.Fatal("a muted press opened the microphone")
	}
}

const twoTimers = `{"shown":1,"remaining":30,"total":60,"paused":0,"count":2,"scale":120,` +
	`"left0":30,"color0":-1,"left1":90,"color1":16776960}`

func TestRunningTimersShowBetweenCalls(t *testing.T) {
	_, echo, _, a := connectApp(t)
	event(a, "timer_progress", twoTimers)
	rings(t, echo, RingTimers)
	v := echo.lastView()
	if len(v.Timers.Lefts) != 2 || v.Timers.Lefts[1] != 90 || v.Timers.Colors[1] != 0xFFFF00 ||
		v.Timers.Colors[0] != -1 || v.Timers.Scale != 120 || v.Timers.At.IsZero() {
		t.Fatalf("the timers given: %+v", v.Timers)
	}
	// New figures repaint; the same state otherwise would not.
	event(a, "timer_progress", `{"shown":1,"remaining":20,"total":60,"count":1,"scale":60,"left0":20,"color0":-1}`)
	rings(t, echo, RingTimers, RingTimers)
	event(a, "timer_progress", `{"shown":0}`)
	rings(t, echo, RingTimers, RingTimers, RingOff)
}

func TestTimerProgressIsClamped(t *testing.T) {
	now := time.Now()
	v := parseTimerProgress([]byte(`{"shown":1,"count":9,"scale":0,"remaining":-5,"left0":-1,"color0":99999999,"left3":"x"}`), now)
	if len(v.Lefts) != TimerLayers || v.Scale != 1 || v.Lefts[0] != 0 || v.Colors[0] != -1 || v.Lefts[3] != 0 {
		t.Fatalf("%+v", v)
	}
	if v := parseTimerProgress([]byte(`not json`), now); len(v.Lefts) != 0 {
		t.Fatal("garbage shows timers")
	}
	if v := parseTimerProgress([]byte(`{"shown":1,"remaining":5,"total":10}`), now); len(v.Lefts) != 1 || v.Lefts[0] != 5 || v.Scale != 10 {
		t.Fatalf("an older app's (no count): %+v", v)
	}
}

// A call while timers run: the call's ring, its outcome flash over the
// timers, then the timers again (the VPE's show_result).
func TestACallOverRunningTimersFlashesThenShowsThemAgain(t *testing.T) {
	s, echo, _, a := connectApp(t)
	event(a, "timer_progress", twoTimers)
	rings(t, echo, RingTimers)
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	a.send(Hangup, []byte(`{"reason":"done"}`)) // nothing said: done
	rings(t, echo, RingTimers, RingListening, RingThinking, RingDone)
	time.Sleep(flashFor(RingDone, false) + 200*time.Millisecond)
	rings(t, echo, RingTimers, RingListening, RingThinking, RingDone, RingTimers)
}

// After an outcome flash, idle is painted again: it is not always dark (the
// Echo's LED Ring light). Bench, 00LQ, 2026-11-18: a call that timed out with
// nothing said left the ring dark.
func TestAfterAFlashIdleIsPaintedAgain(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	a.send(Hangup, []byte(`{"reason":"done"}`))
	rings(t, echo, RingListening, RingThinking, RingDone)
	time.Sleep(flashFor(RingDone, false) + 200*time.Millisecond)
	rings(t, echo, RingListening, RingThinking, RingDone, RingOff)
}

// With no app connected the ring is idle, and a repaint paints idle: not
// always dark (the Echo's LED Ring light). Bench, 00LQ, 2026-11-18: after a
// restart the ring stayed dark until the first call.
func TestARepaintWithNoAppPaintsIdle(t *testing.T) {
	echo := &fakeEcho{}
	s, err := Listen(Config{Name: "n", MAC: "m", Device: echo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Repaint()
	if got := echo.shown(); len(got) != 1 || got[0] != RingOff {
		t.Fatalf("the ring was given %v, want [off]", got)
	}
}

func TestNightRepaintsWhatIsShown(t *testing.T) {
	s, echo, _, a := connectApp(t)
	event(a, "timer_progress", twoTimers)
	rings(t, echo, RingTimers)
	event(a, "night", `{"on":true}`)
	rings(t, echo, RingTimers, RingTimers)
	if !echo.lastView().Night {
		t.Fatal("night not passed to the ring")
	}
	// At night a command is not understood without a word: the red blinks.
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	a.send(Hangup, []byte(`{"reason":"not_recognized"}`))
	rings(t, echo, RingTimers, RingTimers, RingListening, RingThinking, RingError)
	if v := echo.lastView(); !v.Night {
		t.Fatal("the night flash is not marked night")
	}
	event(a, "night", `{"on":false}`)
	time.Sleep(flashFor(RingError, true) + 200*time.Millisecond)
	if v := echo.lastView(); v.State != RingTimers || v.Night {
		t.Fatalf("after the flash, by day: %+v", v)
	}
}

func TestTheAppLeavingClearsTimersAndNight(t *testing.T) {
	_, echo, _, a := connectApp(t)
	event(a, "night", `{"on":true}`)
	event(a, "timer_progress", twoTimers)
	waitFor(t, "the timers", func() bool { return echo.lastView().State == RingTimers })
	a.c.Close()
	waitFor(t, "dark", func() bool { v := echo.lastView(); return v.State == RingOff && !v.Night })
}

func TestARepaintPaintsTheSameStateAgain(t *testing.T) {
	s, echo, _, a := connectApp(t)
	event(a, "timer_progress", twoTimers)
	rings(t, echo, RingTimers)
	s.Repaint()
	rings(t, echo, RingTimers, RingTimers)
}
