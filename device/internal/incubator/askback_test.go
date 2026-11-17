package incubator

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// The app's order for a question (server.py _respond, _hang_up, Cues.stop):
// the question is said, then its "listening" sound in the same stream; the
// stream is ended and the Echo's PLAYBACK_DRAINED awaited (its own speaker's
// report), and only then HANGUP with listen: nothing is playing when the
// answer's call starts and flushes the speaker.
func askAndHangUp(t *testing.T, s *Server, echo *fakeEcho, a *app, listen bool) {
	t.Helper()
	a.send(AudioOut, make([]byte, 4000)) // heard
	event(a, "processing", "")
	event(a, "responding", `{"text":"Which room?"}`)
	a.send(AudioOut, make([]byte, 4000))
	a.send(AudioOut, make([]byte, 4000)) // the "listening" sound
	a.send(AudioOutEnd, nil)
	waitFor(t, "the stream's end", func() bool { return echo.count(&echo.ends) >= 1 })
	s.Drained()
	a.expect(PlaybackDrained)
	h := map[string]any{"reason": "done", "listen": listen}
	b, _ := json.Marshal(h)
	a.send(Hangup, b)
}

func TestHelloSaysItListensForAnAnswer(t *testing.T) {
	s, err := Listen(Config{Name: "n", MAC: "m", Device: &fakeEcho{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, r := dial(t, s.Port())
	defer c.Close()
	if h := readHello(t, r); h["listen"] != true {
		t.Fatalf("HELLO listen = %v", h["listen"])
	}
}

func TestAHangUpWithListenStartsTheAnswersCall(t *testing.T) {
	s, echo, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	askAndHangUp(t, s, echo, a, true)
	var cs CallStartInfo
	json.Unmarshal(a.expect(CallStart), &cs)
	if cs.WakeWord != WakeFollowUp || cs.PrerollBytes != 0 {
		t.Fatalf("CALL_START %+v, want follow_up with no pre-roll", cs)
	}
	waitFor(t, "inCall reported", func() bool { return st.last().InCall })
	answer := bytes.Repeat([]byte{3, 4}, chunkBytes/2)
	echo.speak(answer)
	if got := a.expect(AudioIn); !bytes.Equal(got, answer) {
		t.Fatal("the answer's audio did not reach the app")
	}
	// Dark once the question has played (the app waits for that), then
	// listening for the answer: no outcome flash between.
	rings(t, echo, RingListening, RingThinking, RingSpeaking, RingOff, RingListening)
	// The answer is a call like any other.
	event(a, "processing", "")
	waitFor(t, "the answer's command to end", func() bool { return !st.last().InCall })
}

func TestNoAnswersCallWithoutListenOrWhileMuted(t *testing.T) {
	for _, muted := range []bool{false, true} {
		s, echo, _, a := connectApp(t)
		s.Press()
		a.expect(CallStart)
		if muted {
			echo.mu.Lock()
			echo.muted = true
			echo.mu.Unlock()
		}
		askAndHangUp(t, s, echo, a, muted) // listen asked only when muted
		a.nothing(200 * time.Millisecond)
		if n := echo.count(&echo.opens); n != 1 {
			t.Fatalf("muted %v: the microphone opened %d times", muted, n)
		}
	}
}

func TestAHangUpWithListenAndNoCallStartsNothing(t *testing.T) {
	_, _, _, a := connectApp(t)
	a.send(Hangup, []byte(`{"reason":"done","listen":true}`))
	a.nothing(200 * time.Millisecond)
}

func TestAnAnswersCallIsNotPrecededByAnOutcomeFlash(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	event(a, "processing", "")
	a.send(Hangup, []byte(`{"reason":"done","listen":true}`)) // nothing said
	a.expect(CallStart)
	rings(t, echo, RingListening, RingThinking, RingListening)
}
