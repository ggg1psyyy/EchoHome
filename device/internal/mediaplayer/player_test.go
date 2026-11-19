package mediaplayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// plane is a stand-in for one of the speaker's planes, with stream.go's
// rules: a pump on a discarding stream is swallowed; a flush drops the queue
// and arms discard on an open stream; an end clears a discard (and is then
// no end), else marks the end. It plays one period per tick. Breaking the
// rules the player must keep is recorded: an end with no stream open, a
// flush with nothing open or queued (each arms an end with nothing behind
// it, which ends the next stream early on the real speaker).
type plane struct {
	mu         sync.Mutex
	q          [][]byte
	active     bool
	discarding bool
	played     []byte
	broken     []string
	periods    int
	onDrained  func()
	ended      bool
	lastTake   time.Time
}

func (pl *plane) pump(p []byte) error {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.discarding {
		return nil
	}
	pl.active = true
	pl.q = append(pl.q, append([]byte(nil), p...))
	pl.periods++
	return nil
}

func (pl *plane) flush() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if !pl.active && len(pl.q) == 0 {
		pl.broken = append(pl.broken, "flush with nothing open or queued")
	}
	if pl.active {
		pl.discarding = true
	}
	pl.q = nil
}

func (pl *plane) end() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if !pl.active && !pl.discarding {
		pl.broken = append(pl.broken, "end with no stream open")
	}
	was := pl.discarding
	pl.active, pl.discarding = false, false
	if !was {
		pl.ended = true
	}
}

func (pl *plane) tick() {
	pl.mu.Lock()
	var drained func()
	if len(pl.q) > 0 {
		pl.played = append(pl.played, pl.q[0]...)
		pl.q = pl.q[1:]
		pl.lastTake = time.Now()
		if len(pl.q) == 0 && pl.ended && !pl.active {
			pl.ended = false
			drained = pl.onDrained
		}
	}
	pl.mu.Unlock()
	if drained != nil {
		drained()
	}
}

func (pl *plane) lead() time.Duration {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return time.Duration(len(pl.q)) * 2048 * time.Second / rate
}

type fakeOut struct{ music, voice plane }

func (f *fakeOut) PumpMusic(p []byte) error { return f.music.pump(p) }
func (f *fakeOut) EndMusicStream()          { f.music.end() }
func (f *fakeOut) FlushMusic()              { f.music.flush() }
func (f *fakeOut) MusicLead() time.Duration { return f.music.lead() }
func (f *fakeOut) PumpVoice(p []byte) error { return f.voice.pump(p) }
func (f *fakeOut) EndVoice()                { f.voice.end() }
func (f *fakeOut) FlushVoice()              { f.voice.flush() }
func (f *fakeOut) VoiceSoundingUntil() time.Time {
	f.voice.mu.Lock()
	defer f.voice.mu.Unlock()
	return f.voice.lastTake
}

// run plays both planes, one period per millisecond, until the test ends.
func (f *fakeOut) run(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				f.music.tick()
				f.voice.tick()
			}
		}
	}()
}

// pcm is n periods of distinct samples, so any gap, repeat or swap shows.
func pcm(periods int, seed uint16) []byte {
	out := make([]byte, 0, periods*PeriodBytes)
	for i := 0; i < periods*2048; i++ {
		out = binary.LittleEndian.AppendUint16(out, uint16(i)+seed)
	}
	return out
}

func serve(t *testing.T, files map[string][]byte) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

type states struct {
	mu sync.Mutex
	s  []State
}

func (st *states) add(s State) { st.mu.Lock(); st.s = append(st.s, s); st.mu.Unlock() }
func (st *states) last() State {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s) == 0 {
		return Idle
	}
	return st.s[len(st.s)-1]
}

func newPlayer(t *testing.T) (*Player, *fakeOut, *states) {
	out := &fakeOut{}
	out.run(t)
	st := &states{}
	p := &Player{Out: out, Fetch: fetch, OnState: st.add, TargetLead: 300 * time.Millisecond}
	out.voice.onDrained = func() { p.StreamEnded() }
	t.Cleanup(p.Stop)
	return p, out, st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func played(pl *plane) []byte {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return append([]byte(nil), pl.played...)
}

func noBrokenRules(t *testing.T, out *fakeOut) {
	t.Helper()
	for _, pl := range []*plane{&out.music, &out.voice} {
		pl.mu.Lock()
		b := pl.broken
		pl.mu.Unlock()
		if len(b) > 0 {
			t.Fatalf("the speaker's rules broken: %v", b)
		}
	}
}

func TestASongPlaysWholeAndEnds(t *testing.T) {
	src := append(pcm(200, 0), 1, 2, 3, 4) // a short last period, padded
	base := serve(t, map[string][]byte{"/a.wav": wav(src, 0xFFFFFFFF, wireFmt())})
	p, out, st := newPlayer(t)
	p.Play(base + "/a.wav")
	if p.State() != Playing {
		t.Fatal("not playing")
	}
	waitFor(t, "idle", func() bool { return p.State() == Idle && st.last() == Idle })
	want := append(append([]byte(nil), src...), make([]byte, PeriodBytes-4)...)
	if got := played(&out.music); !bytes.Equal(got, want) {
		t.Fatalf("played %d bytes, want %d (or different)", len(got), len(want))
	}
	noBrokenRules(t, out)
}

// The invariant pause rests on: what is heard, across pauses, is the song,
// with nothing skipped or played twice.
func TestPausedAndResumedNothingIsLostOrRepeated(t *testing.T) {
	src := pcm(600, 7)
	base := serve(t, map[string][]byte{"/a.wav": wav(src, 0xFFFFFFFF, wireFmt())})
	p, out, st := newPlayer(t)
	p.Play(base + "/a.wav")
	for i := 0; i < 5; i++ {
		time.Sleep(40 * time.Millisecond)
		p.Pause()
		if p.State() != Paused || st.last() != Paused {
			t.Fatalf("pause %d: %v", i, p.State())
		}
		n := len(played(&out.music))
		time.Sleep(30 * time.Millisecond)
		if m := len(played(&out.music)); m != n {
			t.Fatalf("pause %d: %d bytes played while paused", i, m-n)
		}
		p.Resume()
	}
	waitFor(t, "idle", func() bool { return p.State() == Idle })
	if got := played(&out.music); !bytes.Equal(got, src) {
		t.Fatalf("heard %d bytes, the song is %d; first difference at %d", len(got), len(src), firstDiff(got, src))
	}
	noBrokenRules(t, out)
}

// A pause in the song's last second (its end already sent) is undone too.
func TestAPauseAfterTheLastPeriodIsSafe(t *testing.T) {
	src := pcm(40, 3)
	base := serve(t, map[string][]byte{"/a.wav": wav(src, 0xFFFFFFFF, wireFmt())})
	out := &fakeOut{}
	st := &states{}
	p := &Player{Out: out, Fetch: fetch, OnState: st.add, TargetLead: 10 * time.Second} // all of it queued at once
	t.Cleanup(p.Stop)
	p.Play(base + "/a.wav")
	waitFor(t, "all pumped", func() bool {
		out.music.mu.Lock()
		defer out.music.mu.Unlock()
		return out.music.periods == 40 && !out.music.active
	})
	p.Pause()
	out.run(t)
	time.Sleep(20 * time.Millisecond)
	if len(played(&out.music)) != 0 {
		t.Fatal("played while paused")
	}
	p.Resume()
	waitFor(t, "idle", func() bool { return p.State() == Idle })
	if got := played(&out.music); !bytes.Equal(got, src) {
		st.mu.Lock()
		t.Fatalf("heard %d of %d, first difference at %d; states %v; periods %d", len(got), len(src), firstDiff(got, src), st.s, out.music.periods)
	}
	noBrokenRules(t, out)
}

func TestStopSilencesAndIsIdle(t *testing.T) {
	src := pcm(2000, 0)
	base := serve(t, map[string][]byte{"/a.wav": wav(src, 0xFFFFFFFF, wireFmt())})
	p, out, st := newPlayer(t)
	p.Play(base + "/a.wav")
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	if p.State() != Idle || st.last() != Idle {
		t.Fatal("not idle")
	}
	n := len(played(&out.music))
	time.Sleep(30 * time.Millisecond)
	if m := len(played(&out.music)); m != n || out.music.lead() != 0 {
		t.Fatal("still playing after stop")
	}
	if got := played(&out.music); !bytes.Equal(got, src[:len(got)]) {
		t.Fatal("what played is not the song's start")
	}
	noBrokenRules(t, out)
}

// The Echo has no decoder: anything but 48 kHz mono S16 never reaches the
// speaker (it would be full-scale noise).
func TestOnlyWirePCMReachesTheSpeaker(t *testing.T) {
	flac := append([]byte("fLaC\x00\x00\x00\x22"), make([]byte, 50000)...)
	stereo := wav(pcm(50, 0), 0xFFFFFFFF, fmtChunk(1, 2, 48000, 16, nil))
	base := serve(t, map[string][]byte{"/f.flac": flac, "/s.wav": stereo})
	for _, path := range []string{"/f.flac", "/s.wav", "/missing.wav"} {
		p, out, _ := newPlayer(t)
		p.Play(base + path)
		waitFor(t, "idle", func() bool { return p.State() == Idle })
		if out.music.periods != 0 {
			t.Fatalf("%s: %d periods reached the speaker", path, out.music.periods)
		}
		noBrokenRules(t, out)
	}
}

func TestPlayingAnotherReplacesTheFirst(t *testing.T) {
	a, b := pcm(2000, 0), pcm(30, 40000)
	base := serve(t, map[string][]byte{"/a.wav": wav(a, 0xFFFFFFFF, wireFmt()), "/b.wav": wav(b, 0xFFFFFFFF, wireFmt())})
	p, out, _ := newPlayer(t)
	p.Play(base + "/a.wav")
	time.Sleep(40 * time.Millisecond)
	p.Play(base + "/b.wav")
	waitFor(t, "idle", func() bool { return p.State() == Idle })
	got := played(&out.music)
	if !bytes.HasSuffix(got, b) {
		t.Fatal("the second did not play whole, last")
	}
	noBrokenRules(t, out)
}

// An announcement plays on the voice plane, ducks the music (held to its
// end report) and leaves it playing underneath.
func TestAnAnnouncementDucksTheMusicAndPlaysWhole(t *testing.T) {
	song, ann := pcm(3000, 0), pcm(60, 9000)
	base := serve(t, map[string][]byte{"/s.wav": wav(song, 0xFFFFFFFF, wireFmt()), "/n.wav": wav(ann, 0xFFFFFFFF, wireFmt())})
	p, out, st := newPlayer(t)
	var ducks []bool
	var mu sync.Mutex
	p.Duck = func(on bool) { mu.Lock(); ducks = append(ducks, on); mu.Unlock() }
	owned := 0
	p.OwnVoice = func() { owned++ }
	p.Play(base + "/s.wav")
	time.Sleep(30 * time.Millisecond)
	p.Announce(base + "/n.wav")
	if p.State() != Announcing {
		t.Fatal("not announcing")
	}
	waitFor(t, "back to playing", func() bool { return p.State() == Playing && st.last() == Playing })
	if got := played(&out.voice); !bytes.Equal(got, ann) {
		t.Fatalf("announcement: %d of %d bytes", len(got), len(ann))
	}
	mu.Lock()
	if len(ducks) != 2 || !ducks[0] || ducks[1] || owned != 1 {
		t.Fatalf("ducks %v owned %d", ducks, owned)
	}
	mu.Unlock()
	if p.StreamEnded() {
		t.Fatal("an end report taken after the announcement had its own")
	}
	p.Stop()
	noBrokenRules(t, out)
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func TestStoppingAnAnnouncementLeavesTheMusic(t *testing.T) {
	song, ann := pcm(3000, 0), pcm(3000, 9000)
	base := serve(t, map[string][]byte{"/s.wav": wav(song, 0xFFFFFFFF, wireFmt()), "/n.wav": wav(ann, 0xFFFFFFFF, wireFmt())})
	p, out, _ := newPlayer(t)
	p.Play(base + "/s.wav")
	time.Sleep(30 * time.Millisecond)
	p.Announce(base + "/n.wav")
	if p.State() != Announcing {
		t.Fatal("not announcing")
	}
	p.StopAnnouncement()
	if p.State() != Playing {
		t.Fatalf("state %v after the announcement was stopped, want the music playing", p.State())
	}
	p.StopAnnouncement() // none: nothing
	p.Stop()
	noBrokenRules(t, out)
}
