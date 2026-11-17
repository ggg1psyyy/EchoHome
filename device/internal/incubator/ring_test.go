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
		RingMonitored: false}
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
