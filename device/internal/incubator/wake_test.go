package incubator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wilbowes/EchoMuse/internal/microfeatures"
	"github.com/wilbowes/EchoMuse/internal/mixednet"
)

const chunkBytes = 2560 // the wake stream's 80 ms

func readTestdata(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func nabuBlob(t *testing.T) []byte {
	return readTestdata(t, "..", "mixednet", "testdata", "okay_nabu.blob")
}

func nabuSpeech(t *testing.T) []byte { return readTestdata(t, "testdata", "okay_nabu.pcm") }

// header is a MODEL_START as the app sends it (voice_incubator mixednet.py
// WakeModel.header).
func header(id string, blob []byte) map[string]any {
	sum := sha256.Sum256(blob)
	return map[string]any{
		"format": "mixednet", "slot": 0, "id": id, "wake_word": id, "kind": "wake",
		"size": len(blob), "sha256": hex.EncodeToString(sum[:]),
		"probability_cutoff": 0.85, "sliding_window_size": 5, "feature_step_size": 10,
	}
}

func (a *app) sendJSON(m Message, v any) {
	a.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		a.t.Fatal(err)
	}
	a.send(m, b)
}

// push sends one model as the app does (_push_models) and returns the answer.
func (a *app) push(h map[string]any, blob []byte) ModelResultInfo {
	a.t.Helper()
	a.sendJSON(ModelStart, h)
	for off := 0; off < len(blob); off += MaxPayload {
		a.send(ModelData, blob[off:min(off+MaxPayload, len(blob))])
	}
	a.sendJSON(ModelEnd, map[string]any{"id": h["id"]})
	return a.result()
}

func (a *app) result() ModelResultInfo {
	a.t.Helper()
	var r ModelResultInfo
	if err := json.Unmarshal(a.expect(ModelResult), &r); err != nil {
		a.t.Fatal(err)
	}
	return r
}

// say feeds audio to the microphone path in 80 ms chunks, as the Echo does,
// no faster than the worker takes them (its queue drops when full).
func say(t *testing.T, s *Server, echo *fakeEcho, pcm []byte) {
	t.Helper()
	sess := s.session()
	for off := 0; off < len(pcm); off += chunkBytes {
		echo.speak(pcm[off:min(off+chunkBytes, len(pcm))])
		waitFor(t, "the worker", func() bool { return len(sess.wake.q) == 0 })
	}
}

// heardBy is what the stack heard in pcm, run directly (the oracle: the
// front end and executor are held to the reference firmware by their own
// parity tests): the chunk whose features detected, and the detection.
func heardBy(t *testing.T, blob, pcm []byte) (int, mixednet.Detection) {
	t.Helper()
	fe, err := microfeatures.New(FeatureStepMs)
	if err != nil {
		t.Fatal(err)
	}
	defer fe.Close()
	det := mixednet.NewDetector(FeatureStepMs)
	if err := det.Load(mixednet.ModelConfig{ID: "okay_nabu", WakeWord: "okay_nabu",
		Cutoff: mixednet.CutoffByte(0.85), Window: 5, StepMs: FeatureStepMs}, blob); err != nil {
		t.Fatal(err)
	}
	for k := 0; k*chunkBytes < len(pcm); k++ {
		chunk := pcm[k*chunkBytes : min((k+1)*chunkBytes, len(pcm))]
		samples := make([]int16, len(chunk)/2)
		for i := range samples {
			samples[i] = int16(uint16(chunk[2*i]) | uint16(chunk[2*i+1])<<8)
		}
		var found []mixednet.Detection
		fe.Feed(samples, func(fr *[microfeatures.FeatureSize]int8) {
			found = append(found, det.Process(fr[:])...)
		})
		if len(found) > 0 {
			return k, found[0]
		}
	}
	t.Fatal("the reference run detected nothing in the recording")
	return 0, mixednet.Detection{}
}

func TestHelloSaysWhatTheWakeWordStackRuns(t *testing.T) {
	echo := &fakeEcho{}
	s, err := Listen(Config{Name: "n", MAC: "m", Device: echo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, r := dial(t, s.Port())
	defer c.Close()
	h := readHello(t, r)
	want := map[string]any{"wake_format": "mixednet", "model_slots": float64(ModelSlots),
		"wake_models": float64(0), "feature_size": float64(40), "max_pool": true,
		"preroll_ms": float64(1500)}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("HELLO %s = %v, want %v", k, h[k], v)
		}
	}
	if h["stop_word"] != true {
		t.Error("HELLO does not say stop_word: \"stop\" would be pushed only for a ring")
	}
}

func TestPresenceOnlyAnnouncesNoWakeWord(t *testing.T) {
	s, err := Listen(Config{Name: "n", MAC: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, r := dial(t, s.Port())
	defer c.Close()
	if h := readHello(t, r); h["wake_format"] != nil {
		t.Fatalf("a satellite with no microphone says wake_format %v", h["wake_format"])
	}
}

func TestAPushedModelLoadsAndOpensTheMicrophone(t *testing.T) {
	s, echo, st, a := connectApp(t)
	a.send(ModelClear, nil)
	blob := nabuBlob(t)
	if r := a.push(header("okay_nabu", blob), blob); !r.OK || r.ID != "okay_nabu" || r.Models != 1 {
		t.Fatalf("MODEL_RESULT %+v", r)
	}
	waitFor(t, "wakeModels reported", func() bool { return st.last().WakeModels == 1 })
	if echo.count(&echo.opens) != 1 {
		t.Fatal("the microphone did not open for the model")
	}
	if s.WakeModels() != 1 {
		t.Fatal("WakeModels")
	}
	// The same id again replaces it.
	if r := a.push(header("okay_nabu", blob), blob); !r.OK || r.Models != 1 {
		t.Fatalf("a repeated push: %+v", r)
	}
	a.send(ModelClear, nil)
	waitFor(t, "the microphone closed with the models", func() bool { return echo.count(&echo.closes) == 1 })
	waitFor(t, "wakeModels cleared", func() bool { return st.last().WakeModels == 0 })
}

func TestAWakeWordStartsACallWithTheAudioBeforeIt(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob, speech := nabuBlob(t), nabuSpeech(t)
	a.push(header("okay_nabu", blob), blob)
	k, want := heardBy(t, blob, speech)

	for round := 0; round < 2; round++ {
		say(t, s, echo, speech)
		var cs CallStartInfo
		if err := json.Unmarshal(a.expect(CallStart), &cs); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			end := (k + 1) * chunkBytes
			pre := min(end, prerollBytes)
			if cs.WakeWord != "okay_nabu" || cs.PrerollBytes != pre ||
				cs.Probability == nil || *cs.Probability != int(want.Average) ||
				*cs.MaxProbability != int(want.Highest) || *cs.Cutoff != int(want.Cutoff) {
				t.Fatalf("CALL_START %+v (probability %v), want okay_nabu, %d bytes, %+v",
					cs, cs.Probability, pre, want)
			}
			// The pre-roll ends at the detecting chunk, and the live audio
			// follows it with nothing lost or repeated.
			want := speech[end-pre:]
			var got []byte
			a.c.SetReadDeadline(time.Now().Add(3 * time.Second))
			for len(got) < len(want) {
				m, p, err := a.r.Read()
				if err != nil || m != AudioIn {
					t.Fatalf("%#x %v after %d of %d bytes", m, err, len(got), len(want))
				}
				got = append(got, p...)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("AUDIO_IN is not the recording from the pre-roll on")
			}
		}
		// The app rejects the wake word (HANGUP before any command): the
		// microphone stays open for the models, and the next wake works.
		a.send(Hangup, nil)
		waitFor(t, "the call to end", func() bool {
			sess := s.session()
			sess.mu.Lock()
			defer sess.mu.Unlock()
			return sess.call == nil
		})
		if echo.count(&echo.closes) != 0 {
			t.Fatal("the hang-up closed the microphone the models need")
		}
	}
}

func TestAWakeWordDuringACallIsIgnored(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	a.push(header("okay_nabu", blob), blob)
	s.Press()
	a.expect(CallStart)
	say(t, s, echo, nabuSpeech(t)) // the call's own audio
	a.nothing(300 * time.Millisecond)
}

func TestMutedAWakeWordStartsNothing(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	a.push(header("okay_nabu", blob), blob)
	echo.mu.Lock()
	echo.muted = true
	echo.mu.Unlock()
	say(t, s, echo, nabuSpeech(t))
	a.nothing(300 * time.Millisecond)
}

func TestTheAppLeavingClosesTheModelsMicrophone(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	a.push(header("okay_nabu", blob), blob)
	a.c.Close()
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	waitFor(t, "the session gone", func() bool { return s.session() == nil })
}

// The reference firmware's refusals (handle_model_start_, begin_model,
// append_model, finish_model), and the slot budget.
func TestModelPushesAreRefusedAsTheReferenceRefusesThem(t *testing.T) {
	blob := nabuBlob(t)
	with := func(k string, v any) map[string]any { h := header("w", blob); h[k] = v; return h }
	cases := []struct {
		name string
		h    map[string]any
		blob []byte
		want string
	}{
		{"another format", with("format", "tflite"), blob, "unsupported format"},
		{"no id", with("id", ""), blob, "malformed header"},
		{"no size", with("size", 0), blob, "malformed header"},
		{"too big", with("size", mixednet.MaxModelBytes+1), blob, "rejected"},
		{"another step", with("feature_step_size", 20), blob, "rejected"},
		{"no window", with("sliding_window_size", 0), blob, "rejected"},
		{"short", header("w", blob), blob[:len(blob)-1], "short transfer"},
		{"overrun", with("size", len(blob)-1), blob, "no model staged"},
		{"wrong sha256", with("sha256", "00"), blob, "sha256 mismatch"},
		{"not a model", header("w", bytes.Repeat([]byte{7}, 64)), bytes.Repeat([]byte{7}, 64), ""},
	}
	_, _, _, a := connectApp(t)
	for _, c := range cases {
		a.sendJSON(ModelStart, c.h)
		if c.want == "unsupported format" || c.want == "malformed header" || c.want == "rejected" {
			if r := a.result(); r.OK || r.Error != c.want {
				t.Errorf("%s: %+v, want %q", c.name, r, c.want)
			}
		}
		for off := 0; off < len(c.blob); off += MaxPayload {
			a.send(ModelData, c.blob[off:min(off+MaxPayload, len(c.blob))])
		}
		a.sendJSON(ModelEnd, map[string]any{"id": c.h["id"]})
		r := a.result()
		want := c.want
		if want == "unsupported format" || want == "malformed header" || want == "rejected" {
			want = "no model staged" // its END, after the refused START
		}
		if r.OK || (want != "" && r.Error != want) || r.Models != 0 {
			t.Errorf("%s: %+v, want %q", c.name, r, want)
		}
	}
	// MODEL_END with nothing staged.
	a.sendJSON(ModelEnd, map[string]any{"id": "x"})
	if r := a.result(); r.OK || r.Error != "no model staged" {
		t.Errorf("a bare MODEL_END: %+v", r)
	}
	// The slots: four load, a fifth is refused, and replacing one still works.
	for i := 0; i < ModelSlots; i++ {
		id := string(rune('a' + i))
		if r := a.push(header(id, blob), blob); !r.OK || r.Models != i+1 {
			t.Fatalf("model %d: %+v", i, r)
		}
	}
	if r := a.push(header("e", blob), blob); r.OK || r.Error != "too many models" || r.Models != ModelSlots {
		t.Errorf("a fifth model: %+v", r)
	}
	if r := a.push(header("a", blob), blob); !r.OK || r.Models != ModelSlots {
		t.Errorf("replacing a model in full slots: %+v", r)
	}
}

func TestMuteClosesTheModelsMicrophoneAndNoAudioFromBeforeItLeads(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob, speech := nabuBlob(t), nabuSpeech(t)
	a.push(header("okay_nabu", blob), blob)
	k, _ := heardBy(t, blob, speech)
	if (k+1)*chunkBytes >= prerollBytes {
		t.Skip("the detection comes too late in the recording to see the pre-roll's start")
	}
	before := bytes.Repeat([]byte{0x55, 0x55}, prerollBytes/2)
	say(t, s, echo, before) // heard before the mute
	s.SetMuted(true)
	waitFor(t, "the microphone closed", func() bool { return echo.count(&echo.closes) == 1 })
	s.SetMuted(false)
	waitFor(t, "the microphone opened again", func() bool { return echo.count(&echo.opens) == 2 })
	say(t, s, echo, speech)
	var cs CallStartInfo
	json.Unmarshal(a.expect(CallStart), &cs)
	if cs.PrerollBytes != (k+1)*chunkBytes {
		t.Fatalf("a %d byte pre-roll, want the %d since the microphone opened", cs.PrerollBytes, (k+1)*chunkBytes)
	}
	got := a.expect(AudioIn)
	if bytes.Contains(got, []byte{0x55, 0x55, 0x55, 0x55}) {
		t.Fatal("the pre-roll carries audio from before the mute")
	}
}

// CALL_START says the wake_word the model was pushed with, as the reference
// firmware does (take_detection): the app pushes its id as both.
func TestAWakeCallSaysTheWakeWordTheModelWasPushedWith(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	h := header("model-id", blob)
	h["wake_word"] = "the_word"
	a.push(h, blob)
	say(t, s, echo, nabuSpeech(t))
	var cs CallStartInfo
	json.Unmarshal(a.expect(CallStart), &cs)
	if cs.WakeWord != "the_word" {
		t.Fatalf("CALL_START wake_word %q", cs.WakeWord)
	}
}

// With models loaded the microphone is open before a press; a chunk heard
// before it, still waiting for the worker, is not the call's.
func TestAManualCallTakesNothingFromBeforeThePress(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	a.push(header("okay_nabu", blob), blob)
	w := s.session().wake
	w.detMu.Lock() // the worker stops at the detector, after the first chunk
	echo.speak(bytes.Repeat([]byte{1, 0}, chunkBytes/2))
	waitFor(t, "the worker to take it", func() bool { return len(w.q) == 0 })
	before := bytes.Repeat([]byte{0x55, 0x55}, chunkBytes/2)
	echo.speak(before) // queued behind it
	s.Press()
	a.expect(CallStart)
	w.detMu.Unlock()
	after := bytes.Repeat([]byte{0x22, 0x22}, chunkBytes/2)
	echo.speak(after)
	if got := a.expect(AudioIn); !bytes.Equal(got, after) {
		t.Fatalf("the call's first audio is not from after the press (%x...)", got[:4])
	}
}

// "stop" stays loaded (HELLO stop_word) and runs only while armed: while a
// timer rings and while the answer is said (the Voice PE's listen_for_stop).
// Heard while a timer rings it starts a call the app reads as "stop the
// ring". (Any blob will do for the id: okay_nabu's, on its speech.)
func TestStopRunsOnlyWhileArmed(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	if r := a.push(header(mixednet.StopModelID, blob), blob); !r.OK {
		t.Fatalf("%+v", r)
	}
	say(t, s, echo, nabuSpeech(t))
	a.nothing(200 * time.Millisecond) // idle: not armed
	a.send(Event, []byte(`{"type":"timer_ringing"}`))
	waitFor(t, "armed by the ring", func() bool { return s.session().wake.stopWant.Load() })
	say(t, s, echo, nabuSpeech(t))
	var cs CallStartInfo
	json.Unmarshal(a.expect(CallStart), &cs)
	if cs.WakeWord != WakeStop {
		t.Fatalf("CALL_START wake_word %q, want stop", cs.WakeWord)
	}
}

func TestStopSaidOverTheAnswerCancelsTheCall(t *testing.T) {
	s, echo, _, a := connectApp(t)
	blob := nabuBlob(t)
	if r := a.push(header(mixednet.StopModelID, blob), blob); !r.OK {
		t.Fatalf("%+v", r)
	}
	s.Press()
	a.expect(CallStart)
	if s.session().wake.stopWant.Load() {
		t.Fatal("armed while the command is still being said")
	}
	a.send(Event, []byte(`{"type":"responding"}`))
	waitFor(t, "armed by the answer", func() bool { return s.session().wake.stopWant.Load() })
	say(t, s, echo, nabuSpeech(t))
	var ce CallEndInfo
	json.Unmarshal(a.expect(CallEnd), &ce)
	if ce.Reason != "cancelled" {
		t.Fatalf("CALL_END %+v", ce)
	}
	if echo.count(&echo.annStops) != 1 {
		t.Fatal("an announcement Home Assistant was saying was left playing")
	}
	waitFor(t, "disarmed with the call", func() bool { return !s.session().wake.stopWant.Load() })
}
