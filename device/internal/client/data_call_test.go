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

// controllerSink is the controller's /data end: it counts what arrives.
type controllerSink struct{ frames atomic.Int64 }

func callTestClient(t *testing.T, connected bool) (*DataClient, *controllerSink) {
	t.Helper()
	mic := newFanoutMic()
	t.Cleanup(mic.close)
	d := NewDataClient("call-test", mic, nil, aec.New(board.BiscuitMics().NumMics()))
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
			if len(msg) > 0 {
				sink.frames.Add(1) // anything at all: nothing should come
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
	t.Cleanup(d.CloseCallMic)
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

func TestTheMicrophoneSendsNothingWithALiveLink(t *testing.T) {
	// EchoHome: the controller hears none of the room, however connected.
	d, sink := callTestClient(t, true)
	var got callFrames
	d.OpenCallMic(got.deliver)
	eventually(t, "the call's frames", func() bool { return got.count() >= 5 })
	if n := sink.frames.Load(); n != 0 {
		t.Fatalf("%d frames reached the controller", n)
	}
}

func TestOpeningTwiceRunsOneStream(t *testing.T) {
	d, _ := callTestClient(t, false)
	var a, b callFrames
	d.OpenCallMic(a.deliver)
	d.OpenCallMic(b.deliver) // the satellite re-opens: the tap moves, the stream stays
	eventually(t, "the second tap's frames", func() bool { return b.count() >= 3 })
	n := a.count()
	time.Sleep(100 * time.Millisecond)
	if a.count() != n {
		t.Fatal("the first tap still gets frames")
	}
	d.CloseCallMic()
	eventually(t, "the stream to stop", func() bool { return !active(d) })
}
