package incubator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type states struct {
	mu  sync.Mutex
	all []State
}

func (s *states) add(st State) { s.mu.Lock(); s.all = append(s.all, st); s.mu.Unlock() }

func (s *states) sawListening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.all {
		if st.Listening {
			return true
		}
	}
	return false
}

func (s *states) last() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.all) == 0 {
		return State{}
	}
	return s.all[len(s.all)-1]
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func dial(t *testing.T, port int) (net.Conn, *Reader) {
	t.Helper()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	return c, NewReader(c)
}

func readHello(t *testing.T, r *Reader) map[string]any {
	t.Helper()
	m, p, err := r.Read()
	if err != nil || m != Hello {
		t.Fatalf("first frame %#x %v", m, err)
	}
	var h map[string]any
	if err := json.Unmarshal(p, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func start(t *testing.T) (*Server, *states) {
	t.Helper()
	st := &states{}
	s, err := Listen(Config{Name: "echomuse-0123456789ab", MAC: "02:ec:01:23:45:67", Model: "echo-dot-2", Firmware: "v9"}, st.add)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, st
}

func TestTheSatelliteSaysHelloFirstWithItsIdentity(t *testing.T) {
	s, st := start(t)
	if !st.last().Listening || st.last().Port != s.Port() {
		t.Fatalf("state %+v", st.last())
	}
	c, r := dial(t, s.Port())
	defer c.Close()
	h := readHello(t, r)
	for k, want := range map[string]any{"protocol": 3.0, "name": "echomuse-0123456789ab", "mac": "02:ec:01:23:45:67",
		"model": "echo-dot-2", "firmware": "v9", "sample_rate": 16000.0} {
		if h[k] != want {
			t.Errorf("%s = %v, want %v", k, h[k], want)
		}
	}
	// Presence only: no capability is said, so the app pushes nothing.
	for _, k := range []string{"wake_format", "listen", "stop_word", "monitor", "aec"} {
		if _, ok := h[k]; ok {
			t.Errorf("%s said before it is implemented", k)
		}
	}
	waitFor(t, "the app reported connected", func() bool { return st.last().AppConnected })
}

func TestASecondAppIsRefusedWhileOneIsConnected(t *testing.T) {
	s, _ := start(t)
	first, r1 := dial(t, s.Port())
	defer first.Close()
	readHello(t, r1)
	second, r2 := dial(t, s.Port())
	defer second.Close()
	if _, _, err := r2.Read(); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("the second app got %v, want the connection closed", err)
	}
	// The first is still served: it may send what an app sends.
	if err := WriteJSON(first, Event, EventInfo{Type: "listening"}); err != nil {
		t.Fatalf("the first app lost its connection: %v", err)
	}
}

func TestAnAppCanComeBackAfterItLeaves(t *testing.T) {
	s, st := start(t)
	c, r := dial(t, s.Port())
	readHello(t, r)
	waitFor(t, "connected", func() bool { return st.last().AppConnected })
	c.Close()
	waitFor(t, "disconnected", func() bool { s := st.last(); return s.Listening && !s.AppConnected })
	c2, r2 := dial(t, s.Port())
	defer c2.Close()
	readHello(t, r2)
}

func TestUnknownAndBinaryFramesAreReadAndDropped(t *testing.T) {
	s, _ := start(t)
	c, r := dial(t, s.Port())
	defer c.Close()
	readHello(t, r)
	WriteFrame(c, 0x7F, []byte("?"))            // unknown type
	WriteFrame(c, AudioOut, make([]byte, 4096)) // audio it will not play yet
	WriteJSON(c, ModelClear, struct{}{})
	// Still connected: a frame from the app goes through.
	if err := WriteJSON(c, Event, EventInfo{Type: "listening"}); err != nil {
		t.Fatal(err)
	}
}

func TestATooLongFrameDropsTheApp(t *testing.T) {
	s, st := start(t)
	c, r := dial(t, s.Port())
	defer c.Close()
	readHello(t, r)
	c.Write([]byte{byte(AudioOut), 0xff, 0xff})
	waitFor(t, "the app dropped", func() bool { return !st.last().AppConnected })
}

func TestCloseDropsTheAppAndStopsListening(t *testing.T) {
	st := &states{}
	s, err := Listen(Config{Name: "n", MAC: "m"}, st.add)
	if err != nil {
		t.Fatal(err)
	}
	c, r := dial(t, s.Port())
	defer c.Close()
	readHello(t, r)
	s.Close()
	if _, _, err := r.Read(); err == nil {
		t.Fatal("the app is still connected")
	}
	if st.last().Listening {
		t.Fatal("still reported listening")
	}
	if _, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port())); err == nil {
		t.Fatal("still accepting")
	}
	s.Close() // twice is harmless
}

func TestASatelliteNeedsANameAndAMAC(t *testing.T) {
	for _, cfg := range []Config{{MAC: "m"}, {Name: "n"}} {
		if _, err := Listen(cfg, nil); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}
