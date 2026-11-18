package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wilbowes/EchoMuse/internal/aec"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// recordingSpeaker is a speaker.Speaker that records what the data client
// asks of it.
type recordingSpeaker struct {
	mu        sync.Mutex
	music     int
	abandoned []time.Time
}

func (r *recordingSpeaker) Init() error             { return nil }
func (r *recordingSpeaker) PumpPeriod([]byte) error { return nil }
func (r *recordingSpeaker) EndStream()              {}
func (r *recordingSpeaker) Flush()                  {}
func (r *recordingSpeaker) EndMusicStream()         {}
func (r *recordingSpeaker) FlushMusic()             {}
func (r *recordingSpeaker) SetDuck(float64)         {}
func (r *recordingSpeaker) Close()                  {}
func (r *recordingSpeaker) PumpMusic(b []byte) error {
	r.mu.Lock()
	r.music++
	r.mu.Unlock()
	return nil
}
func (r *recordingSpeaker) AbandonStreams(at time.Time) {
	r.mu.Lock()
	r.abandoned = append(r.abandoned, at)
	r.mu.Unlock()
}

// Since EchoHome nothing the speaker plays rides the data link: the
// satellite's replies, Home Assistant's media and Sendspin are the Echo's
// own. A lost link must therefore leave every stream alone; it once
// abandoned them (2026-10-23, when the controller sent the music), which
// would now cut a reply whenever the controller restarted. Anything the old
// controller sends is ignored.
func TestALostLinkLeavesTheSpeakerAlone(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.ReadMessage()                                                   // identify
		c.WriteMessage(websocket.BinaryMessage, []byte{0x04, 1, 2, 3, 4}) // an old controller's music
		c.UnderlyingConn().Close()
	}))
	defer srv.Close()
	addr := "ws://" + strings.TrimPrefix(srv.URL, "http://")

	mic := newFanoutMic()
	defer mic.close()
	spk := &recordingSpeaker{}
	d := NewDataClient("link-loss-test", mic, spk, aec.New(board.BiscuitMics().NumMics()))

	done := make(chan error, 1)
	go func() { done <- d.connect(context.Background(), addr) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("connect did not return after the link closed")
	}

	spk.mu.Lock()
	defer spk.mu.Unlock()
	if spk.music != 0 {
		t.Fatalf("an old controller's frame reached the speaker (%d)", spk.music)
	}
	if len(spk.abandoned) != 0 {
		t.Fatal("a lost link abandoned the speaker's streams")
	}
}
