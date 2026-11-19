package incubator

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Interop vectors come from the app's own protocol.py
// (tools/incubator_interop/gen.py, with the checkout tools/mixednet_ref/
// build.sh makes):
//
//	MIXEDNET_REF=/tmp/mixednet_ref go test ./internal/incubator -update
var update = flag.Bool("update", false, "regenerate testdata/interop_* with the app's protocol.py (MIXEDNET_REF)")

// ── Frames, from the spec's edges ───────────────────────────────────────

func TestFramesRoundTripAtTheEdgesOfTheLength(t *testing.T) {
	for _, n := range []int{0, 1, 2, 4095, 4096} {
		var b bytes.Buffer
		payload := bytes.Repeat([]byte{0xA5}, n)
		if err := WriteFrame(&b, AudioIn, payload); err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if b.Len() != 3+n || b.Bytes()[0] != 0x03 || binary.BigEndian.Uint16(b.Bytes()[1:]) != uint16(n) {
			t.Fatalf("%d: header %x", n, b.Bytes()[:3])
		}
		m, p, err := NewReader(&b).Read()
		if err != nil || m != AudioIn || !bytes.Equal(p, payload) {
			t.Fatalf("%d: read %v %d %v", n, m, len(p), err)
		}
	}
}

func TestALengthPast4096IsRefusedBothWays(t *testing.T) {
	if err := WriteFrame(io.Discard, AudioIn, make([]byte, 4097)); !errors.Is(err, ErrTooLong) {
		t.Errorf("write: %v", err)
	}
	for _, n := range []uint16{4097, 65535} {
		h := []byte{byte(AudioOut), 0, 0}
		binary.BigEndian.PutUint16(h[1:], n)
		// The reader must refuse at the header, before reading the payload.
		if _, _, err := NewReader(bytes.NewReader(h)).Read(); !errors.Is(err, ErrTooLong) {
			t.Errorf("read %d: %v", n, err)
		}
	}
}

func TestAStreamEndingMidFrameIsNotACleanEnd(t *testing.T) {
	if _, _, err := NewReader(bytes.NewReader(nil)).Read(); err != io.EOF {
		t.Errorf("empty stream: %v, want io.EOF", err)
	}
	for _, b := range [][]byte{{0x10}, {0x10, 0x00}, {0x10, 0x00, 0x04, 1, 2}} {
		if _, _, err := NewReader(bytes.NewReader(b)).Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%x: %v, want io.ErrUnexpectedEOF", b, err)
		}
	}
}

func TestAudioIsSplitIntoWholeSampleFrames(t *testing.T) {
	var b bytes.Buffer
	pcm := make([]byte, 10000)
	if err := WriteAudio(&b, AudioIn, pcm); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&b)
	var sizes []int
	for {
		_, p, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(p)%2 != 0 {
			t.Errorf("a frame of %d bytes splits a sample", len(p))
		}
		sizes = append(sizes, len(p))
	}
	if want := []int{4096, 4096, 1808}; !equalInts(sizes, want) {
		t.Errorf("frames %v, want %v", sizes, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEveryMessageHasOneDirection(t *testing.T) {
	sat := []Message{Hello, CallStart, AudioIn, CallEnd, Status, PlaybackDrained, ModelResult, AudioInAEC,
		DeviceEvent, AudioMonitor, WakeReport, AudioMonitorAEC}
	app := []Message{AudioOut, AudioOutEnd, Event, Hangup, Monitor, ModelStart, ModelData, ModelEnd, ModelClear}
	for _, m := range sat {
		if !m.FromSatellite() || m.FromApp() {
			t.Errorf("%#x", m)
		}
	}
	for _, m := range app {
		if m.FromSatellite() || !m.FromApp() {
			t.Errorf("%#x", m)
		}
	}
	for _, m := range []Message{0x00, 0x0D, 0x0F, 0x15, 0x1F, 0x24, 0xFF} {
		if m.FromSatellite() || m.FromApp() {
			t.Errorf("%#x is not a message of protocol 2", m)
		}
	}
}

func TestDecodeTakesObjectsAndIgnoresUnknownFields(t *testing.T) {
	var h HangupInfo
	if err := Decode(Hangup, []byte(`{"reason":"done","listen":true,"new":[1]}`), &h); err != nil || h.Reason != "done" || !h.Listen {
		t.Errorf("%+v %v", h, err)
	}
	for _, bad := range []string{`[]`, `"x"`, `null`, `{`, ``} {
		if err := Decode(Hangup, []byte(bad), &h); !errors.Is(err, ErrNotObject) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := Decode(AudioOutEnd, nil, &struct{}{}); err != nil {
		t.Errorf("an empty AUDIO_OUT_END: %v", err)
	}
}

// ── HELLO ────────────────────────────────────────────────────────────────

func TestHelloAlwaysSaysProtocol2AndOmitsWhatIsFalse(t *testing.T) {
	b, err := json.Marshal(NewHello("echomuse-x", "02:ec:00:00:00:01"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["protocol"] != float64(2) {
		t.Errorf("protocol %v: the app reads a missing one as 1, a Voice PE", m["protocol"])
	}
	for _, k := range []string{"aec", "monitor_aec", "wake_format", "max_pool", "stop_word", "listen", "monitor", "intercom", "model_slots"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s said before it is implemented", k)
		}
	}
	if m["sample_rate"] != float64(16000) || m["channels"] != float64(1) || m["bits_per_sample"] != float64(16) {
		t.Errorf("format %v %v %v", m["sample_rate"], m["channels"], m["bits_per_sample"])
	}
}

// ── Interop with the app's protocol.py ──────────────────────────────────

// goFrames is what the Go codec sends the app in the interop vectors.
func goFrames() []byte {
	var b bytes.Buffer
	h := NewHello("echomuse-0123456789ab", "02:ec:01:23:45:67")
	h.Model, h.Firmware, h.PrerollMs, h.OutputBuffer = "echo-dot-2", "v9.9.9", 1500, 131072
	h.WakeFormat, h.ModelSlots, h.FeatureSize, h.MaxPool, h.StopWord, h.Listen = "mixednet", 6, 40, true, true, true
	h.Monitor, h.MonitorAEC = true, true // live listening on: monitoring and its second capture
	WriteJSON(&b, Hello, h)
	WriteJSON(&b, Hello, NewHello("echomuse-bare", "02:ec:00:00:00:00")) // nothing implemented yet
	prob, top, cut := 235, 255, 217
	WriteJSON(&b, CallStart, CallStartInfo{WakeWord: "okay_nabu", PrerollBytes: 48000, Probability: &prob, MaxProbability: &top, Cutoff: &cut})
	WriteJSON(&b, CallStart, CallStartInfo{WakeWord: WakeFollowUp})
	pcm := make([]byte, 4096)
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	WriteAudio(&b, AudioIn, pcm)
	WriteAudio(&b, AudioMonitor, pcm)
	WriteAudio(&b, AudioMonitorAEC, pcm)
	WriteJSON(&b, CallEnd, CallEndInfo{Reason: "cancelled"})
	WriteJSON(&b, Status, StatusInfo{FreeBytes: 1, QueuedBytes: 2, QueuedMs: 3, SpeakerBusy: true})
	WriteFrame(&b, PlaybackDrained, nil)
	WriteJSON(&b, ModelResult, ModelResultInfo{OK: false, ID: "okay_nabu", Models: 0, Error: "unexpected feature size"})
	WriteJSON(&b, DeviceEvent, DeviceEventInfo{Type: "stop_timer"})
	return b.Bytes()
}

func TestTheAppReadsWhatGoSends(t *testing.T) {
	if *update {
		dir := os.Getenv("MIXEDNET_REF")
		if dir == "" {
			t.Fatal("-update needs MIXEDNET_REF (tools/mixednet_ref/build.sh's directory)")
		}
		gf := filepath.Join(t.TempDir(), "go_frames.bin")
		os.WriteFile(gf, goFrames(), 0o644)
		out, err := exec.Command("python3", "-I", "../../tools/incubator_interop/gen.py",
			filepath.Join(dir, "voice-incubator"), gf, "testdata").CombinedOutput()
		if err != nil {
			t.Fatalf("gen.py: %v: %s", err, out)
		}
	}
	raw, err := os.ReadFile("testdata/interop_app_view.json")
	if err != nil {
		t.Fatal(err)
	}
	var view []struct {
		Type   string         `json:"type"`
		Length int            `json:"length"`
		Hello  map[string]any `json:"hello"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	// What the app made of the Go frames, by its own parser.
	if len(view) != 12 || view[0].Type != "HELLO" || view[4].Type != "AUDIO_IN" ||
		view[5].Type != "AUDIO_MONITOR" || view[6].Type != "AUDIO_MONITOR_AEC" || view[9].Type != "PLAYBACK_DRAINED" {
		t.Fatalf("the app read %d frames: %+v", len(view), view)
	}
	full, bare := view[0].Hello, view[1].Hello
	for k, want := range map[string]any{"protocol": 2.0, "model": "echo-dot-2", "firmware": "v9.9.9",
		"wake_format": "mixednet", "model_slots": 6.0, "max_pool": true, "stop_word": true, "listen": true,
		"aec": false, "monitor": true, "monitor_aec": true, "intercom": false} {
		if full[k] != want {
			t.Errorf("the app read %s = %v from the full HELLO, want %v", k, full[k], want)
		}
	}
	for k, want := range map[string]any{"protocol": 2.0, "aec": false, "monitor_aec": false, "wake_format": nil, "model": nil, "listen": false} {
		if bare[k] != want {
			t.Errorf("the app read %s = %v from a bare HELLO, want %v", k, bare[k], want)
		}
	}
}

// goFrames is stored as the app read it: on every run, what Go sends now
// must be those bytes, so a renamed tag or a dropped omitempty cannot pass
// without the app's view being regenerated.
func TestGoStillSendsWhatTheAppRead(t *testing.T) {
	if *update {
		os.WriteFile("testdata/interop_go_frames.bin", goFrames(), 0o644)
	}
	want, err := os.ReadFile("testdata/interop_go_frames.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(goFrames(), want) {
		t.Error("the Go frames changed since the app read them: regenerate with -update")
	}
}

func TestAZeroHelloStillSaysProtocol2(t *testing.T) {
	b, _ := json.Marshal(HelloInfo{})
	if !bytes.Contains(b, []byte(`"protocol":2`)) {
		t.Errorf("%s", b)
	}
	var b2 bytes.Buffer
	WriteJSON(&b2, Hello, &HelloInfo{Name: "x"}) // through a pointer too
	if !bytes.Contains(b2.Bytes(), []byte(`"protocol":2`)) {
		t.Errorf("%s", b2.Bytes())
	}
}

func TestGoReadsWhatTheAppSends(t *testing.T) {
	raw, err := os.ReadFile("testdata/interop_app_frames.bin")
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(bytes.NewReader(raw))
	var types []string
	var hangups []HangupInfo
	var events []EventInfo
	var start ModelStartInfo
	for {
		m, p, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !m.FromApp() {
			t.Errorf("%#x from the app", m)
		}
		types = append(types, map[Message]string{AudioOut: "out", AudioOutEnd: "end", Event: "event", Hangup: "hangup",
			Monitor: "monitor", ModelClear: "clear", ModelStart: "start", ModelData: "data", ModelEnd: "mend"}[m])
		if m.Binary() {
			continue
		}
		switch m {
		case Hangup:
			var h HangupInfo
			if err := Decode(m, p, &h); err != nil {
				t.Fatal(err)
			}
			hangups = append(hangups, h)
		case Event:
			var e EventInfo
			if err := Decode(m, p, &e); err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		case ModelStart:
			if err := Decode(m, p, &start); err != nil {
				t.Fatal(err)
			}
		default:
			if err := Decode(m, p, &struct{}{}); err != nil {
				t.Errorf("%#x: %v", m, err)
			}
		}
	}
	want := "out out end event event event hangup hangup monitor monitor clear start data mend event"
	if got := strings.Join(types, " "); got != want {
		t.Errorf("frames %q\nwant   %q", got, want)
	}
	if len(hangups) != 2 || !hangups[0].Listen || hangups[1].Listen || hangups[1].Reason != "not_recognized" {
		t.Errorf("hangups %+v", hangups)
	}
	if len(events) != 4 || events[0].Type != "listening" || events[3].Type != "night" || string(events[2].Data) != "" {
		t.Errorf("events %+v", events)
	}
	if start.Format != "mixednet" || start.ID != "okay_nabu" || start.Kind != "wake" || start.ProbabilityCutoff != 0.85 ||
		start.SlidingWindowSize != 5 || start.FeatureStepSize != 10 || start.Size != 46880 {
		t.Errorf("model start %+v", start)
	}
}

// The reader takes bytes from the network: whatever they are, it returns
// frames of at most 4096 bytes or an error, and never panics.
func FuzzRead(f *testing.F) {
	f.Add(goFrames())
	if b, err := os.ReadFile("testdata/interop_app_frames.bin"); err == nil {
		f.Add(b)
	}
	f.Add([]byte{0x10, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		r := NewReader(bytes.NewReader(b))
		for {
			m, p, err := r.Read()
			if err != nil {
				return
			}
			if len(p) > MaxPayload {
				t.Fatalf("a %d-byte payload", len(p))
			}
			if !m.Binary() {
				var v map[string]any
				_ = Decode(m, p, &v)
			}
		}
	})
}
