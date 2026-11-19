package incubator

import (
	"bytes"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wilbowes/EchoMuse/internal/mixednet"
)

// fakeEcho is the Echo's side of a call: a microphone the test feeds and a
// speaker that records what it is asked to play.
type fakeEcho struct {
	mu      sync.Mutex
	deliver func([]byte)
	opens   int
	closes  int
	played  [][]byte
	ends    int
	flushes int
	muted   bool
	block   chan struct{} // when set, Play waits on it
	rings   []Ring
	views   []RingView
	// music: MusicPress finds music to pause or resume; musicPresses
	// counts the presses it took.
	music        bool
	musicPresses int
}

func (f *fakeEcho) MicOpen(d func([]byte)) { f.mu.Lock(); f.deliver = d; f.opens++; f.mu.Unlock() }
func (f *fakeEcho) MicClose()              { f.mu.Lock(); f.closes++; f.mu.Unlock() }
func (f *fakeEcho) Play(p []byte) error {
	f.mu.Lock()
	b := f.block
	f.played = append(f.played, append([]byte(nil), p...))
	f.mu.Unlock()
	if b != nil {
		<-b
	}
	return nil
}
func (f *fakeEcho) End()        { f.mu.Lock(); f.ends++; f.mu.Unlock() }
func (f *fakeEcho) Flush()      { f.mu.Lock(); f.flushes++; f.mu.Unlock() }
func (f *fakeEcho) Muted() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.muted }
func (f *fakeEcho) MusicPress() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.music {
		f.musicPresses++
	}
	return f.music
}
func (f *fakeEcho) Show(v RingView) {
	f.mu.Lock()
	f.rings = append(f.rings, v.State)
	f.views = append(f.views, v)
	f.mu.Unlock()
}

// lastView is the last view the ring was given.
func (f *fakeEcho) lastView() RingView {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.views) == 0 {
		return RingView{}
	}
	return f.views[len(f.views)-1]
}

// shown is what the ring was given, in order.
func (f *fakeEcho) shown() []Ring {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Ring(nil), f.rings...)
}

func (f *fakeEcho) speak(pcm []byte) {
	f.mu.Lock()
	d := f.deliver
	f.mu.Unlock()
	d(pcm)
}

func (f *fakeEcho) count(p *int) int { f.mu.Lock(); defer f.mu.Unlock(); return *p }

func (f *fakeEcho) periods() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.played) }

// app is the Voice Incubator app's end of the connection.
type app struct {
	t *testing.T
	c net.Conn
	r *Reader
}

func connectApp(t *testing.T) (*Server, *fakeEcho, *states, *app) {
	t.Helper()
	echo := &fakeEcho{}
	st := &states{}
	s, err := Listen(Config{Name: "n", MAC: "m", Device: echo}, st.add)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	c, r := dial(t, s.Port())
	t.Cleanup(func() { c.Close() })
	readHello(t, r)
	waitFor(t, "the app to be connected", func() bool { return st.last().AppConnected })
	return s, echo, st, &app{t, c, r}
}

func (a *app) send(m Message, p []byte) {
	a.t.Helper()
	if err := WriteFrame(a.c, m, p); err != nil {
		a.t.Fatal(err)
	}
}

func (a *app) expect(want Message) []byte {
	a.t.Helper()
	a.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		m, p, err := a.r.Read()
		if err != nil {
			a.t.Fatalf("waiting for %#x: %v", want, err)
		}
		if m == want {
			return append([]byte(nil), p...)
		}
		if m != AudioIn {
			a.t.Fatalf("got %#x, want %#x", m, want)
		}
	}
}

// nothing says no frame but AUDIO_IN arrives for a while.
func (a *app) nothing(d time.Duration) {
	a.t.Helper()
	a.c.SetReadDeadline(time.Now().Add(d))
	for {
		m, _, err := a.r.Read()
		if err != nil {
			return
		}
		if m != AudioIn {
			a.t.Fatalf("unexpected %#x", m)
		}
	}
}

func TestAPressStartsAManualCallWithNoPreroll(t *testing.T) {
	s, echo, st, a := connectApp(t)
	s.Press()
	var cs map[string]any
	json.Unmarshal(a.expect(CallStart), &cs)
	if cs["wake_word"] != "manual" {
		t.Fatalf("CALL_START %v", cs)
	}
	if n, _ := cs["preroll_bytes"].(float64); n != 0 {
		t.Fatalf("a press sent %v bytes from before it", n)
	}
	waitFor(t, "inCall reported", func() bool { return st.last().InCall })
	speech := bytes.Repeat([]byte{1, 2}, 1280)
	echo.speak(speech)
	if got := a.expect(AudioIn); !bytes.Equal(got, speech) {
		t.Fatalf("AUDIO_IN %d bytes, want the microphone's %d", len(got), len(speech))
	}
	a.send(Hangup, nil)
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	waitFor(t, "inCall cleared", func() bool { return !st.last().InCall })
	echo.speak(speech) // a late frame from the microphone path
	a.c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if m, _, err := a.r.Read(); err == nil {
		t.Fatalf("%#x after the hang-up", m)
	}
}

func TestMutedAPressStartsNothing(t *testing.T) {
	s, echo, _, a := connectApp(t)
	echo.muted = true
	s.Press()
	a.nothing(200 * time.Millisecond)
	if echo.opens != 0 {
		t.Fatal("the microphone opened while muted")
	}
}

func TestAPressDuringACallCancelsItAndItsReply(t *testing.T) {
	s, echo, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	s.Press()
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "cancelled" {
		t.Fatalf("CALL_END %+v", ce)
	}
	if echo.count(&echo.flushes) < 2 || echo.count(&echo.closes) != 1 || st.last().InCall {
		t.Fatalf("flushes %d closes %d inCall %v", echo.flushes, echo.closes, st.last().InCall)
	}
	// The reply the app had in flight is dropped, up to its end.
	a.send(AudioOut, make([]byte, 4000))
	a.send(AudioOutEnd, nil)
	a.expect(PlaybackDrained)
	if echo.periods() != 0 {
		t.Fatalf("%d periods of a cancelled reply played", echo.periods())
	}
	if echo.count(&echo.ends) != 1 {
		t.Fatal("the speaker was not given the cancelled stream's end")
	}
	// The next one plays.
	a.send(AudioOut, make([]byte, 4000))
	a.send(AudioOutEnd, nil)
	waitFor(t, "the next reply to play", func() bool { return echo.periods() > 0 })
}

func TestAReplyPlaysAt48kHzAndDrainsWhenTheSpeakerSaysSo(t *testing.T) {
	s, echo, _, a := connectApp(t)
	in := make([]byte, 16000) // half a second at 16 kHz
	for i := 0; i < len(in); i += 4000 {
		a.send(AudioOut, in[i:i+4000])
	}
	a.send(AudioOutEnd, nil)
	// 8000 samples in, 24000 out plus the filter's 36-sample tail: 48072
	// bytes, twelve periods, the last padded.
	waitFor(t, "the reply's periods", func() bool { return echo.periods() == 12 && echo.count(&echo.ends) == 1 })
	for _, p := range echo.played {
		if len(p) != PeriodBytes {
			t.Fatalf("a %d-byte period", len(p))
		}
	}
	a.nothing(200 * time.Millisecond) // not drained until the speaker says so
	s.Drained()
	a.expect(PlaybackDrained)
}

func TestAStreamWithNothingInItDrainsAtOnce(t *testing.T) {
	_, _, _, a := connectApp(t)
	a.send(AudioOutEnd, nil)
	a.expect(PlaybackDrained)
}

func TestAMissingPlaybackReportDoesNotStallTheApp(t *testing.T) {
	old := drainGrace
	drainGrace = 50 * time.Millisecond
	defer func() { drainGrace = old }()
	_, _, _, a := connectApp(t)
	a.send(AudioOut, make([]byte, 320))
	a.send(AudioOutEnd, nil)
	a.expect(PlaybackDrained)
}

func TestTheAppIsStillHeardWhileADrainIsAwaited(t *testing.T) {
	s, _, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	a.send(AudioOut, make([]byte, 320))
	a.send(AudioOutEnd, nil) // no speaker report comes
	a.send(Hangup, nil)
	time.Sleep(100 * time.Millisecond)
	// Had the hang-up gone unread, this press would cancel the call. The
	// press also lets the awaited PLAYBACK_DRAINED go (a cut), which may
	// reach the app first.
	s.Press()
	a.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		m, _, err := a.r.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m == CallStart {
			return
		}
		if m != PlaybackDrained {
			t.Fatalf("got %#x, want CALL_START", m)
		}
	}
}

func TestARingingTimerIsStoppedNotTalkedOver(t *testing.T) {
	s, echo, _, a := connectApp(t)
	a.send(Event, []byte(`{"type":"timer_ringing"}`))
	time.Sleep(50 * time.Millisecond)
	s.Press()
	var ev DeviceEventInfo
	json.Unmarshal(a.expect(DeviceEvent), &ev)
	if ev.Type != "stop_timer" || echo.opens != 0 {
		t.Fatalf("event %+v, microphone opened %d times", ev, echo.opens)
	}
	s.Press() // rung out: the next press is a call
	a.expect(CallStart)
}

func TestTheEchoEndsACallTheAppNeverDoes(t *testing.T) {
	old := listenLimit
	listenLimit = 100 * time.Millisecond
	defer func() { listenLimit = old }()
	s, echo, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "timeout" || echo.count(&echo.closes) != 1 {
		t.Fatalf("CALL_END %+v, closes %d", ce, echo.closes)
	}
	waitFor(t, "inCall cleared", func() bool { return !st.last().InCall })
}

func TestMuteEndsACall(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	s.SetMuted(true)
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "cancelled" || echo.count(&echo.closes) != 1 {
		t.Fatalf("CALL_END %+v, closes %d", ce, echo.closes)
	}
}

func TestTheAppLeavingClosesTheMicrophone(t *testing.T) {
	s, echo, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	a.c.Close()
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	waitFor(t, "the app gone", func() bool { return !st.last().AppConnected && !st.last().InCall })
}

func TestTheMicrophoneNeverWaitsForTheApp(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	// The app stops reading; the microphone path must not notice.
	frame := make([]byte, 2560)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ { // 400 s of audio
			echo.speak(frame)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the microphone path blocked on a stalled app")
	}
}

func TestPresenceOnlyTakesNoCalls(t *testing.T) {
	st := &states{}
	s, err := Listen(Config{Name: "n", MAC: "m"}, st.add)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, r := dial(t, s.Port())
	defer c.Close()
	readHello(t, r)
	s.Press()
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if m, _, err := r.Read(); err == nil {
		t.Fatalf("%#x from a presence-only satellite", m)
	}
}

func TestACancelledCallLeavesTheNextReplyAlone(t *testing.T) {
	// Cancelled while listening, before any reply: the app still ends the
	// (empty) stream, and the next call's reply plays.
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	s.Press()
	a.expect(CallEnd)
	a.send(AudioOutEnd, nil) // what the app sends on every cancelled
	a.expect(PlaybackDrained)
	s.Press()
	a.expect(CallStart)
	a.send(AudioOut, make([]byte, 4000))
	a.send(AudioOutEnd, nil)
	waitFor(t, "the next call's reply to play", func() bool { return echo.periods() > 0 })
}

func TestACancelDoesNotKeepTheAppWaitingForADrain(t *testing.T) {
	// On 00LQ a cancel during a reply that had fully arrived left the app
	// waiting out the fallback for a report the cut stream never gave.
	s, _, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	for i := 0; i < 8; i++ { // a second of reply, all sent
		a.send(AudioOut, make([]byte, 4000))
	}
	a.send(AudioOutEnd, nil)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	s.Press() // cancel
	a.expect(CallEnd)
	a.expect(PlaybackDrained)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("PLAYBACK_DRAINED %v after the cancel", d)
	}
}

func TestTheMicrophoneClosesWhenTheCommandEnds(t *testing.T) {
	s, echo, st, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	waitFor(t, "inCall", func() bool { return st.last().InCall })
	a.send(Event, []byte(`{"type":"processing"}`))
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	waitFor(t, "inCall cleared", func() bool { return !st.last().InCall })
	echo.speak(make([]byte, 2560))
	a.nothing(150 * time.Millisecond) // no AUDIO_IN after the command
	// The call itself is still open: a press cancels it.
	s.Press()
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "cancelled" {
		t.Fatalf("CALL_END %+v", ce)
	}
}

func TestTheReplysFirstAudioEndsTheCommandToo(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	a.send(AudioOut, make([]byte, 320))
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
}

func TestNoLimitCutsALongReply(t *testing.T) {
	defer func(l, r, h time.Duration) { listenLimit, replyWait, hangupWait = l, r, h }(listenLimit, replyWait, hangupWait)
	listenLimit, replyWait, hangupWait = 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond
	s, _, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	a.send(Event, []byte(`{"type":"processing"}`))
	for i := 0; i < 10; i++ { // 500 ms of reply, past every limit
		a.send(AudioOut, make([]byte, 640))
		time.Sleep(50 * time.Millisecond)
	}
	a.send(AudioOutEnd, nil)
	a.nothing(100 * time.Millisecond) // no CALL_END while it played
	s.Drained()
	a.expect(PlaybackDrained)
	// Played, and no HANGUP: the Echo ends the call.
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "timeout" {
		t.Fatalf("CALL_END %+v", ce)
	}
}

func TestAReplyThatNeverBeginsEndsTheCall(t *testing.T) {
	defer func(r time.Duration) { replyWait = r }(replyWait)
	replyWait = 100 * time.Millisecond
	s, _, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	a.send(Event, []byte(`{"type":"processing"}`))
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "timeout" {
		t.Fatalf("CALL_END %+v", ce)
	}
}

func TestAStreamTheAppLeftOpenDoesNotSwallowTheNextCallsReply(t *testing.T) {
	// The app cancels a ring or a reply mid-send with no AUDIO_OUT_END. A
	// new call ends that stream for the speaker, which would otherwise
	// discard everything after the call's cut up to the next end.
	s, echo, _, a := connectApp(t)
	a.send(AudioOut, make([]byte, 4000)) // a ring, abandoned
	waitFor(t, "the ring's periods", func() bool { return echo.periods() > 0 })
	s.Press()
	a.expect(CallStart)
	if echo.count(&echo.ends) != 1 {
		t.Fatalf("the abandoned stream was ended %d times, want once", echo.ends)
	}
	before := echo.periods()
	// The app's first message about a call is its `listening` event (sent
	// for every call before the command can end: server.py after the wake
	// word's check, and _listen), which is how the call's own audio is told
	// from the cut ring's frames still in flight.
	a.send(Event, []byte(`{"type":"listening"}`))
	a.send(AudioOut, make([]byte, 4000))
	a.send(AudioOutEnd, nil)
	waitFor(t, "the call's reply", func() bool { return echo.periods() > before })
}

func TestACutRingsFramesAreNotTheCallsReply(t *testing.T) {
	// A wake over a ringing timer or an announcement: the app cancels that
	// playback, but frames already sent still arrive. Taken for the reply
	// they would close the microphone at once.
	s, echo, st, a := connectApp(t)
	a.send(Event, []byte(`{"type":"timer_ringing"}`))
	a.send(AudioOut, make([]byte, 4000))
	waitFor(t, "the ring's periods", func() bool { return echo.periods() > 0 })
	s.session().wakeHeard(mixednet.Detection{WakeWord: "w"}, 0)
	a.expect(CallStart)
	before := echo.periods()
	a.send(AudioOut, make([]byte, 4000)) // the ring's, in flight
	a.send(AudioOutEnd, nil)
	a.nothing(200 * time.Millisecond)
	if !st.last().InCall || echo.periods() != before {
		t.Fatalf("the ring's frames were taken for the reply (inCall %v, %d periods played)",
			st.last().InCall, echo.periods()-before)
	}
	a.send(Event, []byte(`{"type":"processing"}`))
	waitFor(t, "the command to end", func() bool { return !st.last().InCall })
	a.send(AudioOut, make([]byte, 4000))
	waitFor(t, "the reply", func() bool { return echo.periods() > before })
}

// The Voice PE's order: with music playing a press pauses it (and the next
// resumes it), muted or not, and starts no call; a call is cancelled first.
func TestAPressOverMusicPausesItAndStartsNoCall(t *testing.T) {
	s, echo, _, a := connectApp(t)
	echo.mu.Lock()
	echo.music = true
	echo.mu.Unlock()
	s.Press()
	a.nothing(200 * time.Millisecond)
	if echo.count(&echo.musicPresses) != 1 || echo.count(&echo.opens) != 0 {
		t.Fatalf("music presses %d, microphone opens %d", echo.musicPresses, echo.opens)
	}
	echo.mu.Lock()
	echo.muted = true
	echo.mu.Unlock()
	s.Press()
	waitFor(t, "a muted press paused the music", func() bool { return echo.count(&echo.musicPresses) == 2 })
}

func TestAPressCancelsACallBeforeItTouchesTheMusic(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	echo.mu.Lock()
	echo.music = true
	echo.mu.Unlock()
	s.Press()
	a.expect(CallEnd)
	if echo.count(&echo.musicPresses) != 0 {
		t.Fatal("the press went to the music, not the call")
	}
}

func TestWithNoMusicAPressStartsACall(t *testing.T) {
	s, echo, _, a := connectApp(t)
	s.Press()
	a.expect(CallStart)
	if echo.count(&echo.musicPresses) != 0 {
		t.Fatal("counted a music press with no music")
	}
}
