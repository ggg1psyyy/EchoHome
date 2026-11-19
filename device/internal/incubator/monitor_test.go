package incubator

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func connectMonitorApp(t *testing.T, allow bool) (*Server, *fakeEcho, *states, *app, map[string]any) {
	t.Helper()
	echo := &fakeEcho{}
	st := &states{}
	s, err := Listen(Config{Name: "n", MAC: "m", Device: echo, Monitor: allow}, st.add)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	c, r := dial(t, s.Port())
	t.Cleanup(func() { c.Close() })
	h := readHello(t, r)
	waitFor(t, "the app to be connected", func() bool { return st.last().AppConnected })
	return s, echo, st, &app{t, c, r}, h
}

// next reads frames until one of want, failing on any other but those in skip.
func (a *app) next(want Message, skip ...Message) []byte {
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
		ok := false
		for _, s := range skip {
			// A monitored chunk is two frames, AUDIO_MONITOR then its
			// second capture: skipping one skips both.
			ok = ok || m == s || (s == AudioMonitor && m == AudioMonitorAEC)
		}
		if !ok {
			a.t.Fatalf("got %#x, want %#x", m, want)
		}
	}
}

func (s *states) sawMonitored() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.all {
		if st.Monitored {
			return true
		}
	}
	return false
}

func monitorOn(a *app, on bool) {
	b, _ := json.Marshal(MonitorInfo{Active: on})
	a.send(Monitor, b)
}

func TestHelloOffersMonitoringOnlyWhenAllowed(t *testing.T) {
	_, _, _, _, off := connectMonitorApp(t, false)
	if _, ok := off["monitor"]; ok {
		t.Error("HELLO offers monitoring though it is not allowed")
	}
	if _, ok := off["monitor_aec"]; ok {
		t.Error("HELLO offers the second capture though monitoring is not allowed")
	}
	_, _, _, _, on := connectMonitorApp(t, true)
	if on["monitor"] != true {
		t.Errorf("HELLO monitor = %v with it allowed", on["monitor"])
	}
	// Enrollment asks for the second capture by monitor_aec; calls carry
	// none, so `aec` (AUDIO_IN_AEC) is never said.
	if on["monitor_aec"] != true {
		t.Errorf("HELLO monitor_aec = %v with monitoring allowed", on["monitor_aec"])
	}
	for _, k := range []string{"intercom", "aec"} {
		if _, ok := on[k]; ok {
			t.Errorf("HELLO says %s", k)
		}
	}
}

func TestAMonitorThatIsNotAllowedStreamsNothing(t *testing.T) {
	_, echo, st, a, _ := connectMonitorApp(t, false)
	monitorOn(a, true)
	a.nothing(200 * time.Millisecond)
	if echo.count(&echo.opens) != 0 || st.sawMonitored() {
		t.Fatal("not allowed, and yet the microphone opened or it reported monitoring")
	}
}

func TestTheIntercomIsNotOffered(t *testing.T) {
	_, echo, _, a, _ := connectMonitorApp(t, true)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	a.nothing(200 * time.Millisecond)
	if echo.count(&echo.opens) != 0 {
		t.Fatal("the intercom opened the microphone")
	}
}

func TestMonitoringStreamsTheMicrophoneAndSaysSoFirst(t *testing.T) {
	_, echo, st, a, _ := connectMonitorApp(t, true)
	monitorOn(a, true)
	waitFor(t, "the microphone", func() bool { return echo.count(&echo.opens) == 1 })
	chunk := bytes.Repeat([]byte{5, 6}, chunkBytes/2)
	echo.speak(chunk)
	got := a.next(AudioMonitor)
	if !st.sawMonitored() {
		t.Fatal("audio left before the Echo reported it")
	}
	if !bytes.Equal(got, chunk) {
		t.Fatal("AUDIO_MONITOR is not the microphone's")
	}
	// Its second capture follows, the same frame: what enrollment collects.
	if aec := a.next(AudioMonitorAEC); !bytes.Equal(aec, chunk) {
		t.Fatal("AUDIO_MONITOR_AEC is not the same frame")
	}
	var r map[string]any
	json.Unmarshal(a.next(WakeReport, AudioMonitor), &r)
	if _, ok := r["models"].([]any); !ok || r["detected"] == nil {
		t.Fatalf("WAKE_REPORT %v", r)
	}
	rings(t, echo, RingMonitored)
	monitorOn(a, false)
	waitFor(t, "monitoring reported off", func() bool { return !st.last().Monitored })
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	echo.speak(chunk) // late
	a.nothing(150 * time.Millisecond)
	rings(t, echo, RingMonitored, RingOff)
}

func TestAWakeWordWhileMonitoredIsReportedNotAnswered(t *testing.T) {
	s, echo, _, a, _ := connectMonitorApp(t, true)
	blob := nabuBlob(t)
	a.push(header("okay_nabu", blob), blob)
	monitorOn(a, true)
	waitFor(t, "monitoring", func() bool { return s.session().mon.on.Load() })
	say(t, s, echo, nabuSpeech(t))
	detected, peak := false, 0
	a.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for !detected {
		m, p, err := a.r.Read()
		if err != nil {
			t.Fatalf("no detection reported (highest window %d): %v", peak, err)
		}
		switch m {
		case CallStart:
			t.Fatal("a wake word heard while monitored started a call")
		case WakeReport:
			var r wakeReport
			json.Unmarshal(p, &r)
			for _, mr := range r.Models {
				peak = max(peak, int(mr.Window))
			}
			for _, id := range r.Detected {
				detected = detected || id == "okay_nabu"
			}
		}
	}
	if peak < int(mixnetCutoff()) {
		t.Fatalf("detected, yet no report's window reached the cutoff (%d)", peak)
	}
}

func TestACallPausesMonitoringAndItResumesAfter(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, true)
	monitorOn(a, true)
	waitFor(t, "monitoring", func() bool { return st.last().Monitored })
	s.Press()
	a.next(CallStart, AudioMonitor, WakeReport)
	waitFor(t, "monitoring paused", func() bool { return !st.last().Monitored })
	chunk := bytes.Repeat([]byte{7, 8}, chunkBytes/2)
	echo.speak(chunk)
	if got := a.next(AudioIn, AudioMonitor, WakeReport); !bytes.Equal(got, chunk) {
		t.Fatal("the call's audio")
	}
	echo.speak(chunk)
	a.c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	for {
		m, _, err := a.r.Read()
		if err != nil {
			break
		}
		if m == AudioMonitor || m == WakeReport {
			t.Fatalf("%#x during a call", m)
		}
	}
	a.send(Hangup, []byte(`{"reason":"done"}`))
	waitFor(t, "monitoring again", func() bool { return st.last().Monitored })
	echo.speak(chunk)
	a.next(AudioMonitor, WakeReport, AudioIn)
}

func TestMuteStopsMonitoringUntilUnmuted(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, true)
	monitorOn(a, true)
	waitFor(t, "monitoring", func() bool { return st.last().Monitored })
	s.SetMuted(true)
	waitFor(t, "monitoring off", func() bool { return !st.last().Monitored })
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	s.SetMuted(false)
	waitFor(t, "monitoring again", func() bool { return st.last().Monitored })
	echo.speak(bytes.Repeat([]byte{1, 1}, chunkBytes/2))
	a.next(AudioMonitor, WakeReport)
}

// What a model heard before monitoring began is not reported as heard
// while monitored (set_monitoring_ drops a stale report).
func TestTheFirstReportIsNotWhatCameBeforeMonitoring(t *testing.T) {
	s, echo, _, a, _ := connectMonitorApp(t, true)
	blob := nabuBlob(t)
	a.push(header("okay_nabu", blob), blob)
	speech := nabuSpeech(t)
	k, _ := heardBy(t, blob, speech)
	say(t, s, echo, speech[:(k+1)*chunkBytes]) // up to the detection: a call
	a.next(CallStart, AudioIn)
	a.send(Hangup, []byte(`{"reason":"wake_rejected"}`))
	waitFor(t, "the call to end", func() bool { return s.InCall() == false })
	monitorOn(a, true)
	waitFor(t, "monitoring", func() bool { return s.session().mon.on.Load() })
	echo.speak(make([]byte, chunkBytes)) // silence
	var r wakeReport
	json.Unmarshal(a.next(WakeReport, AudioMonitor, AudioIn), &r)
	if len(r.Models) != 1 || r.Models[0].Window >= mixnetCutoff() {
		t.Fatalf("the first report carries what came before: %+v", r)
	}
}

func mixnetCutoff() uint8 { return 217 } // CutoffByte(0.85), header()'s

// Muted during a call while the page listens: the call's end must not
// resume monitoring before the mute takes it (no state after the call says
// monitored, and no audio leaves).
func TestMuteDuringACallNeverResumesMonitoring(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, true)
	monitorOn(a, true)
	waitFor(t, "monitoring", func() bool { return st.last().Monitored })
	s.Press()
	a.next(CallStart, AudioMonitor, WakeReport)
	waitFor(t, "paused", func() bool { return !st.last().Monitored })
	st.mu.Lock()
	from := len(st.all)
	st.mu.Unlock()
	s.SetMuted(true)
	a.next(CallEnd, AudioMonitor, WakeReport, AudioIn)
	echo.speak(make([]byte, chunkBytes))
	a.c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		m, _, err := a.r.Read()
		if err != nil {
			break
		}
		if m == AudioMonitor || m == WakeReport {
			t.Fatalf("%#x after a mute", m)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, x := range st.all[from:] {
		if x.Monitored {
			t.Fatal("monitoring resumed between the call's end and the mute")
		}
	}
}

// The intercom asked of an Echo already monitored stops the stream: the app
// would send it on to another satellite.
func TestAnIntercomRequestStopsMonitoring(t *testing.T) {
	_, echo, st, a, _ := connectMonitorApp(t, true)
	monitorOn(a, true)
	waitFor(t, "monitoring", func() bool { return st.last().Monitored })
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "monitoring stopped", func() bool { return !st.last().Monitored })
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
}
