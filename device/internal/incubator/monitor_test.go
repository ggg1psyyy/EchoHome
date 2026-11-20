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
	if _, ok := on["aec"]; ok {
		t.Error("HELLO says aec")
	}
	// The intercom needs no switch: it rings and waits for a tap here.
	if off["intercom"] != true || on["intercom"] != true {
		t.Errorf("HELLO intercom = %v / %v", off["intercom"], on["intercom"])
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

// Called, the Echo rings and opens nothing; what the other side says is not
// played. Live listening is off: the intercom needs no switch.
func TestACalledIntercomRingsAndOpensNothing(t *testing.T) {
	_, echo, st, a, _ := connectMonitorApp(t, false)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "a ring", func() bool { return echo.count(&echo.chimes) >= 1 })
	a.send(AudioOut, make([]byte, 4000))
	a.nothing(200 * time.Millisecond)
	if echo.count(&echo.opens) != 0 || st.last().Monitored {
		t.Fatal("the microphone opened before anyone answered")
	}
	if echo.periods() != 0 {
		t.Fatal("the other side was played before anyone answered")
	}
	if v := echo.lastView(); v.State != RingIntercomRinging {
		t.Fatalf("ring %v", v.State)
	}
}

// A tap answers: the first capture alone goes, no report, and the other
// side plays; the next tap hangs up, and the app is told.
func TestATapAnswersTheIntercomAndTheNextEndsIt(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, false)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "a ring", func() bool { return echo.count(&echo.chimes) >= 1 })
	s.Press()
	waitFor(t, "the microphone", func() bool { return echo.count(&echo.opens) == 1 })
	waitFor(t, "reported", func() bool { return st.last().Monitored })
	waitFor(t, "the audio gate", func() bool { return s.session().mon.on.Load() }) // after the report
	chunk := bytes.Repeat([]byte{7, 8}, chunkBytes/2)
	echo.speak(chunk)
	if got := a.next(AudioMonitor); !bytes.Equal(got, chunk) {
		t.Fatal("AUDIO_MONITOR is not the microphone's")
	}
	echo.speak(chunk)
	if got := a.next(AudioMonitor); !bytes.Equal(got, chunk) {
		t.Fatal("a second capture or a report came between two chunks")
	}
	if v := echo.lastView(); v.State != RingIntercom {
		t.Fatalf("ring %v", v.State)
	}
	a.send(AudioOut, make([]byte, 4096))
	waitFor(t, "the other side heard", func() bool { return echo.periods() > 0 })
	s.Press()
	var ev DeviceEventInfo
	json.Unmarshal(a.next(DeviceEvent, AudioMonitor), &ev)
	if ev.Type != "intercom_end" {
		t.Fatalf("DEVICE_EVENT %+v", ev)
	}
	waitFor(t, "no longer reported", func() bool { return !st.last().Monitored })
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
}

func TestAnUnansweredIntercomIsDeclined(t *testing.T) {
	old := intercomAnswerWithin
	intercomAnswerWithin = 300 * time.Millisecond
	t.Cleanup(func() { intercomAnswerWithin = old })
	_, echo, _, a, _ := connectMonitorApp(t, false)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	var ev DeviceEventInfo
	json.Unmarshal(a.expect(DeviceEvent), &ev)
	if ev.Type != "intercom_end" || echo.count(&echo.opens) != 0 {
		t.Fatalf("DEVICE_EVENT %+v, opens %d", ev, echo.opens)
	}
}

func TestMutingARingingIntercomDeclinesIt(t *testing.T) {
	s, echo, _, a, _ := connectMonitorApp(t, false)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "a ring", func() bool { return echo.count(&echo.chimes) >= 1 })
	echo.mu.Lock()
	echo.muted = true
	echo.mu.Unlock()
	s.SetMuted(true)
	var ev DeviceEventInfo
	json.Unmarshal(a.expect(DeviceEvent), &ev)
	if ev.Type != "intercom_end" {
		t.Fatalf("DEVICE_EVENT %+v", ev)
	}
}

// The app ending it: no event back, the microphone closes.
func TestTheAppEndingTheIntercomClosesTheMicrophone(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, false)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "a ring", func() bool { return echo.count(&echo.chimes) >= 1 })
	s.Press()
	waitFor(t, "reported", func() bool { return st.last().Monitored })
	a.send(Monitor, []byte(`{"active":false}`))
	waitFor(t, "no longer reported", func() bool { return !st.last().Monitored })
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	if v := echo.lastView(); v.State == RingIntercom || v.State == RingIntercomRinging {
		t.Fatalf("ring %v after the end", v.State)
	}
}

func TestMonitoringStreamsTheMicrophoneAndSaysSoFirst(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, true)
	monitorOn(a, true)
	waitFor(t, "the microphone", func() bool { return echo.count(&echo.opens) == 1 })
	// The microphone opens before the report and audio passes only after it
	// (updateMonitor): a chunk spoken in between is dropped, rightly. Wait
	// for the gate, or the test races it (CI, 8ecc9a3).
	waitFor(t, "monitoring", func() bool { return s.session().mon.on.Load() })
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
	waitFor(t, "the audio gate", func() bool { return s.session().mon.on.Load() }) // after the report
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

// The caller does not ring: the call that asked for the intercom has just
// ended here, so the microphone opens at once and nothing chimes.
func TestTheEchoThatAskedConnectsWithoutRinging(t *testing.T) {
	s, echo, st, a, _ := connectMonitorApp(t, false)
	s.Press()
	a.next(CallStart, AudioIn)
	a.send(Hangup, []byte(`{"reason":"done"}`))
	waitFor(t, "the call to end", func() bool { return !s.InCall() })
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "the microphone", func() bool { return echo.count(&echo.opens) >= 2 })
	waitFor(t, "reported", func() bool { return st.last().Monitored })
	if echo.count(&echo.chimes) != 0 {
		t.Fatal("the caller rang")
	}
	if v := echo.lastView(); v.State != RingIntercom {
		t.Fatalf("ring %v", v.State)
	}
}

// A call that ended longer ago does not count: the Echo rings.
func TestAnOlderCallStillRings(t *testing.T) {
	old := intercomCallerWithin
	intercomCallerWithin = 50 * time.Millisecond
	t.Cleanup(func() { intercomCallerWithin = old })
	s, echo, st, a, _ := connectMonitorApp(t, false)
	s.Press()
	a.next(CallStart, AudioIn)
	a.send(Hangup, []byte(`{"reason":"done"}`))
	waitFor(t, "the call to end", func() bool { return !s.InCall() })
	time.Sleep(100 * time.Millisecond)
	opens := echo.count(&echo.opens)
	a.send(Monitor, []byte(`{"active":true,"intercom":true}`))
	waitFor(t, "a ring", func() bool { return echo.count(&echo.chimes) >= 1 })
	a.nothing(200 * time.Millisecond)
	if echo.count(&echo.opens) != opens || st.last().Monitored {
		t.Fatal("the microphone opened before anyone answered")
	}
}
