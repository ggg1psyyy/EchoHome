package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wilbowes/EchoMuse/internal/aec"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// controllerSink is the controller's /data end: it counts mic frames.
type controllerSink struct{ frames atomic.Int64 }

func callTestClient(t *testing.T, connected bool) (*DataClient, *controllerSink) {
	t.Helper()
	mic := newFanoutMic()
	t.Cleanup(mic.close)
	d := NewDataClient("call-test", mic, nil, aec.New(board.BiscuitMics().NumMics()))
	d.newSpeechStream = func() speechScorer { return nil }
	sink := &controllerSink{}
	if !connected {
		return d, sink
	}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if len(msg) > 3 && msg[0] == frameTypeMic {
				sink.frames.Add(1)
			}
		}
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+strings.TrimPrefix(srv.URL, "http://"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	d.connMu.Lock()
	d.conn = conn
	d.connMu.Unlock()
	t.Cleanup(d.StopMic)
	return d, sink
}

type callFrames struct {
	mu   sync.Mutex
	n    int
	size int
}

func (c *callFrames) deliver(b []byte) { c.mu.Lock(); c.n++; c.size = len(b); c.mu.Unlock() }
func (c *callFrames) count() int       { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func active(d *DataClient) bool { d.micMu.Lock(); defer d.micMu.Unlock(); return d.micActive }

func TestACallWithNoWakeStreamRunsItsOwnAndSendsNothing(t *testing.T) {
	// The wake word off: the controller keeps the wake stream down.
	d, sink := callTestClient(t, true)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "the call's frames", func() bool { return got.count() >= 3 })
	if got.size != vadOwwChunkBytes {
		t.Fatalf("%d-byte frames, want the wake stream's %d", got.size, vadOwwChunkBytes)
	}
	if n := sink.frames.Load(); n != 0 {
		t.Fatalf("a call's own stream sent %d frames to the controller", n)
	}
	d.CloseCallMic()
	eventually(t, "the stream to stop", func() bool { return !active(d) })
	n := got.count()
	time.Sleep(100 * time.Millisecond)
	if got.count() != n {
		t.Fatal("frames after the call closed")
	}
}

func TestACallWithNoDataLinkStillHasAMicrophone(t *testing.T) {
	d, _ := callTestClient(t, false)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "the call's frames", func() bool { return got.count() >= 3 })
	d.CloseCallMic()
}

func TestACallRidesTheControllersWakeStreamAndLeavesItRunning(t *testing.T) {
	d, sink := callTestClient(t, true)
	d.StartMic(false)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "frames to both", func() bool { return got.count() >= 3 && sink.frames.Load() >= 3 })
	d.CloseCallMic()
	before := sink.frames.Load()
	eventually(t, "the wake stream still running", func() bool { return sink.frames.Load() > before+2 })
}

func TestAMicStopDuringACallStopsOnlyWhatTheControllerGets(t *testing.T) {
	d, sink := callTestClient(t, true)
	d.StartMic(false)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "frames", func() bool { return got.count() >= 2 && sink.frames.Load() >= 2 })
	d.StopMic()
	time.Sleep(50 * time.Millisecond)
	before, call := sink.frames.Load(), got.count()
	time.Sleep(150 * time.Millisecond)
	if sink.frames.Load() != before {
		t.Fatal("the controller still receives the room after its mic_stop")
	}
	if got.count() <= call {
		t.Fatal("the controller's mic_stop ended a call")
	}
	d.CloseCallMic()
	eventually(t, "the stream to stop", func() bool { return !active(d) })
	if want, _ := micIntent(d); want {
		t.Fatal("the mic_stop's intent was lost")
	}
}

func TestAMicStartDuringACallFeedsBoth(t *testing.T) {
	d, sink := callTestClient(t, true)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "the call's frames", func() bool { return got.count() >= 2 })
	d.StartMic(false)
	call := got.count()
	eventually(t, "frames to both", func() bool { return sink.frames.Load() >= 3 && got.count() > call+2 })
	d.CloseCallMic()
	before := sink.frames.Load()
	eventually(t, "the controller's stream kept", func() bool { return sink.frames.Load() > before+2 })
}

func TestAMicStartWithNoConnectionLeavesTheCallItsMicrophone(t *testing.T) {
	d, _ := callTestClient(t, false)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "the call's frames", func() bool { return got.count() >= 2 })
	d.StartMic(false) // remembered for the connection, nothing to stream to yet
	n := got.count()
	eventually(t, "the call's frames to go on", func() bool { return got.count() > n+2 })
	d.CloseCallMic()
}
