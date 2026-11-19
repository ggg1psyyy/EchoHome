// Package mediaplayer is the Echo's own media player for Home Assistant (the
// ESPHome media player entity): HA hands it a URL, it fetches the audio and
// plays it, as a Voice PE does. HA's ffmpeg proxy delivers WAV at the
// speaker's own format (wav.go), so there is no decoder here.
//
// Music goes to the speaker's music plane, where a voice reply ducks it;
// announcements go to the voice plane and duck the music themselves. The
// speaker is fed only TargetLead ahead from a buffer held here, so a pause
// is a flush of that little and a rewind of this buffer by exactly what was
// flushed: resumed, the song carries on where it was heard to stop.
//
// Pure: the speaker, the fetch and the duck are given to it, so it is tested
// on the host against a stand-in with the music plane's flush rules.
package mediaplayer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// State is what Home Assistant is shown.
type State int

const (
	Idle State = iota
	Playing
	Paused
	Announcing
)

func (s State) String() string {
	return [...]string{"idle", "playing", "paused", "announcing"}[s]
}

// Output is the Echo's speaker (speaker.PcmSpeaker): periods of mono S16LE
// at 48 kHz.
type Output interface {
	PumpMusic(period []byte) error
	EndMusicStream()
	FlushMusic()
	// MusicLead is how much music is queued and not yet played.
	MusicLead() time.Duration
	PumpVoice(period []byte) error
	EndVoice()
	FlushVoice()
	// VoiceSoundingUntil is when the last voice period taken stops sounding
	// (zero if none has).
	VoiceSoundingUntil() time.Time
}

const (
	rate        = 48000
	bytesPerSec = rate * 2
	// PeriodBytes is one speaker period of mono S16 (2048 frames).
	PeriodBytes = 2048 * 2
	// defaultLead is how far ahead the speaker is fed: past its 1 s prime,
	// so a new stream starts, and little enough that a pause costs nothing
	// to undo.
	defaultLead = 1500 * time.Millisecond
	// readAhead is how much is fetched ahead of the speaker: a WiFi stall's
	// worth. A pause holds the rest of the stream in TCP.
	readAhead = 10 * bytesPerSec
	// history is how much already pumped is kept for a pause's rewind: more
	// than the lead.
	history = 4 * bytesPerSec
	// drainTail: after the last queued period, the speaker's own ring still
	// plays (alsaBufferFrames, ~171 ms).
	drainTail = 250 * time.Millisecond
	// announceDrainMax bounds the wait for an announcement's end report.
	announceDrainMax = 30 * time.Second
	poll             = 20 * time.Millisecond
)

// Player plays HA's URLs. Safe for concurrent use.
type Player struct {
	Out Output
	// Fetch opens a URL (HTTP GET); the body is read until EOF or ctx ends.
	Fetch func(ctx context.Context, url string) (io.ReadCloser, error)
	// OnState is told every change of what HA should be shown.
	OnState func(State)
	// Duck holds (true) and releases (false) the music's duck for an
	// announcement; counted by the caller, which shares it.
	Duck func(on bool)
	// OwnVoice is called before an announcement's first period: the voice
	// plane's end report is then the player's (StreamEnded).
	OwnVoice func()
	// TargetLead is how far ahead the speaker is fed (defaultLead if 0).
	TargetLead time.Duration

	mu       sync.Mutex
	music    *session
	ann      *announcement
	shown    State
	voiceOwn atomic.Bool
}

func (p *Player) lead() time.Duration {
	if p.TargetLead > 0 {
		return p.TargetLead
	}
	return defaultLead
}

// State is what HA is shown now.
func (p *Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stateLocked()
}

func (p *Player) stateLocked() State {
	if p.ann != nil {
		return Announcing
	}
	if p.music == nil {
		return Idle
	}
	if p.music.isPaused() {
		return Paused
	}
	return Playing
}

// report tells OnState if what HA is shown changed. Called with mu held, so
// OnState must not call back into the player.
func (p *Player) reportLocked() {
	st := p.stateLocked()
	if st == p.shown {
		return
	}
	p.shown = st
	if p.OnState != nil {
		p.OnState(st)
	}
}

// Play starts a URL as music, replacing what plays.
func (p *Player) Play(url string) {
	p.mu.Lock()
	if old := p.music; old != nil {
		p.music = nil
		old.stop()
	}
	s := newSession(p, url)
	p.music = s
	p.reportLocked()
	p.mu.Unlock()
	go s.run()
}

// Pause holds the music where it is heard.
func (p *Player) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.music != nil {
		p.music.pause()
	}
	p.reportLocked()
}

// Resume carries on from a pause.
func (p *Player) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.music != nil {
		p.music.resume()
	}
	p.reportLocked()
}

// Stop ends the music and any announcement.
func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.music; s != nil {
		p.music = nil
		s.stop()
	}
	if a := p.ann; a != nil {
		p.ann = nil
		a.cancel()
	}
	p.reportLocked()
}

// StopAnnouncement ends an announcement, the music under it carrying on:
// the button cancelling a call or stopping a ringing timer quiets what is
// being announced too, as the Voice PE's does. Nothing without one.
func (p *Player) StopAnnouncement() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.ann; a != nil {
		p.ann = nil
		a.cancel()
		p.reportLocked()
	}
}

// ended is a session's own end (the stream ran out, or failed).
func (p *Player) ended(s *session, why error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if why != nil {
		log.Printf("[media] %s: %v", s.url, why)
	}
	if p.music == s {
		p.music = nil
		p.reportLocked()
	}
}

// --- music ------------------------------------------------------------------

type session struct {
	p      *Player
	url    string
	ctx    context.Context
	cancel context.CancelFunc

	// pumpMu orders pumping against pause and stop: no period is pumped
	// between a pause's flush and its end of stream.
	pumpMu sync.Mutex

	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte // fetched PCM; data[:pos] pumped (history), data[pos:] not yet
	pos    int
	eof    bool
	err    error
	paused bool
	// open: a speaker stream of ours is open (pumped, not yet ended). The
	// speaker's rules (stream.go): an open stream is flushed AND ended, an
	// ended one only flushed, and nothing is flushed or ended with nothing
	// behind it, or the next stream's end is armed before it starts.
	open bool
}

func newSession(p *Player, url string) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{p: p, url: url, ctx: ctx, cancel: cancel}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *session) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

func (s *session) run() {
	body, err := s.p.Fetch(s.ctx, s.url)
	if err != nil {
		s.finish(fmt.Errorf("fetch: %w", err))
		return
	}
	go s.read(body)
	why := s.pumpLoop()
	s.finish(why)
}

// read fetches into data, at most readAhead ahead of the speaker.
func (s *session) read(body io.ReadCloser) {
	defer body.Close()
	go func() { <-s.ctx.Done(); body.Close() }()
	var w WavStream
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			pcm, perr := w.Push(buf[:n])
			if perr == nil && w.Ready() && !IsWirePCM(w.Format) {
				perr = fmt.Errorf("%w: %+v is not 48 kHz mono S16 (HA should convert it)", ErrNotWav, *w.Format)
			}
			s.mu.Lock()
			if perr != nil {
				s.err, s.eof = perr, true
				s.cond.Broadcast()
				s.mu.Unlock()
				return
			}
			s.data = append(s.data, pcm...)
			s.cond.Broadcast()
			for len(s.data)-s.pos > readAhead && s.ctx.Err() == nil {
				s.cond.Wait()
			}
			s.mu.Unlock()
		}
		if err != nil {
			s.mu.Lock()
			s.eof = true
			if !errors.Is(err, io.EOF) && s.ctx.Err() == nil {
				s.err = err
			}
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
	}
}

// pumpLoop feeds the speaker TargetLead ahead until the stream runs out.
func (s *session) pumpLoop() error {
	out := s.p.Out
	for {
		if s.ctx.Err() != nil {
			return nil
		}
		s.pumpMu.Lock()
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			s.pumpMu.Unlock()
			return err
		}
		avail := len(s.data) - s.pos
		switch {
		case s.paused || out.MusicLead() >= s.p.lead():
			s.mu.Unlock()
			s.pumpMu.Unlock()
			sleep(s.ctx, poll)
			continue
		case avail == 0 && s.eof:
			open := s.open
			s.open = false
			s.mu.Unlock()
			if open {
				out.EndMusicStream()
			}
			s.pumpMu.Unlock()
			// Played out: the queue, then the speaker's own ring. A pause in
			// this last stretch flushes it and rewinds, and may be resumed
			// before this looks again: what it put back is played first.
			for out.MusicLead() > 0 && s.ctx.Err() == nil && !s.isPaused() && !s.rewound() {
				sleep(s.ctx, poll)
			}
			if s.isPaused() || s.rewound() {
				continue
			}
			sleep(s.ctx, drainTail)
			if s.rewound() {
				continue
			}
			return nil
		case avail < PeriodBytes && !s.eof:
			s.mu.Unlock()
			s.pumpMu.Unlock()
			sleep(s.ctx, poll) // waiting for the network
			continue
		}
		n := min(avail, PeriodBytes)
		period := make([]byte, PeriodBytes) // a short last one is padded with silence
		copy(period, s.data[s.pos:s.pos+n])
		s.pos += n
		s.open = true
		// Keep history for a pause's rewind; drop what is older.
		if s.pos > history+PeriodBytes {
			drop := s.pos - history
			s.data = append(s.data[:0], s.data[drop:]...)
			s.pos -= drop
		}
		s.cond.Broadcast() // the reader may read further
		s.mu.Unlock()
		err := out.PumpMusic(period)
		s.pumpMu.Unlock()
		if err != nil {
			return fmt.Errorf("speaker: %w", err)
		}
	}
}

// pause flushes what the speaker holds and rewinds by exactly that, so the
// resumed stream starts with the first period not heard.
func (s *session) pause() {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	s.mu.Lock()
	if s.paused {
		s.mu.Unlock()
		return
	}
	s.paused = true
	open := s.open
	s.open = false
	s.mu.Unlock()
	out := s.p.Out
	queued := out.MusicLead()
	silence(out, open, queued)
	back := int(queued.Seconds()*rate+0.5) * 2
	s.mu.Lock()
	s.pos -= min(back, s.pos)
	s.mu.Unlock()
}

// rewound reports unpumped audio left after the stream's end: put back by
// a pause.
func (s *session) rewound() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data) > s.pos
}

func (s *session) resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
}

// stop ends the session and silences what the speaker holds of it.
func (s *session) stop() {
	s.cancel()
	s.mu.Lock()
	s.cond.Broadcast()
	s.mu.Unlock()
	s.pumpMu.Lock()
	s.mu.Lock()
	open := s.open
	s.open = false
	s.mu.Unlock()
	silence(s.p.Out, open, s.p.Out.MusicLead())
	s.pumpMu.Unlock()
}

// silence drops what the speaker holds of ours. An open stream is flushed
// and ended (the end clears the discard the flush arms, so the next stream
// plays); an ended one with periods still queued is only flushed (the drain
// is its end); nothing queued and nothing open, nothing is done.
func silence(out Output, open bool, queued time.Duration) {
	switch {
	case open:
		out.FlushMusic()
		out.EndMusicStream()
	case queued > 0:
		out.FlushMusic()
	}
}

func (s *session) finish(why error) {
	s.cancel()
	s.mu.Lock()
	s.cond.Broadcast()
	s.mu.Unlock()
	if why != nil {
		s.stop() // a failed stream leaves nothing playing
	}
	s.p.ended(s, why)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// --- announcements -----------------------------------------------------------

type announcement struct {
	ctx     context.Context
	cancel  context.CancelFunc
	drained chan struct{}
	once    sync.Once
}

// Announce plays a URL on the voice plane, over the music (ducked), which
// carries on underneath. A new one replaces one still playing.
func (p *Player) Announce(url string) {
	p.mu.Lock()
	if old := p.ann; old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &announcement{ctx: ctx, cancel: cancel, drained: make(chan struct{})}
	p.ann = a
	p.reportLocked()
	p.mu.Unlock()
	go p.announce(a, url)
}

// StreamEnded takes the speaker's voice end-of-stream report when an
// announcement owns the voice plane; false leaves it to whoever else.
func (p *Player) StreamEnded() bool {
	if !p.voiceOwn.Swap(false) {
		return false
	}
	p.mu.Lock()
	a := p.ann
	p.mu.Unlock()
	if a != nil {
		a.once.Do(func() { close(a.drained) })
	}
	return true
}

func (p *Player) announce(a *announcement, url string) {
	defer p.announceDone(a)
	body, err := p.Fetch(a.ctx, url)
	if err != nil {
		log.Printf("[media] announcement %s: %v", url, err)
		return
	}
	defer body.Close()
	go func() { <-a.ctx.Done(); body.Close() }()
	var w WavStream
	buf := make([]byte, 32*1024)
	var pending []byte
	started := false
	start := func() {
		started = true
		if p.OwnVoice != nil {
			p.OwnVoice()
		}
		p.voiceOwn.Store(true)
		if p.Duck != nil {
			p.Duck(true)
		}
	}
	pump := func(period []byte) bool {
		if !started {
			start()
		}
		return p.Out.PumpVoice(period) == nil && a.ctx.Err() == nil
	}
	for a.ctx.Err() == nil {
		n, rerr := body.Read(buf)
		if n > 0 {
			pcm, perr := w.Push(buf[:n])
			if perr == nil && w.Ready() && !IsWirePCM(w.Format) {
				perr = fmt.Errorf("%w: %+v", ErrNotWav, *w.Format)
			}
			if perr != nil {
				log.Printf("[media] announcement %s: %v", url, perr)
				break
			}
			pending = append(pending, pcm...)
			for len(pending) >= PeriodBytes {
				if !pump(append([]byte(nil), pending[:PeriodBytes]...)) {
					break
				}
				pending = pending[PeriodBytes:]
			}
		}
		if rerr != nil {
			if len(pending) > 0 && a.ctx.Err() == nil {
				period := make([]byte, PeriodBytes)
				copy(period, pending)
				pump(period)
			}
			break
		}
	}
	if !started {
		return
	}
	if a.ctx.Err() != nil {
		// Cut: a stream cut while sounding still reports its end; one cut
		// before it sounded never does, so the plane is given back.
		sounding := time.Now().Before(p.Out.VoiceSoundingUntil().Add(50 * time.Millisecond))
		p.Out.FlushVoice()
		if !sounding {
			p.voiceOwn.Store(false)
		}
	} else {
		p.Out.EndVoice()
		t := time.NewTimer(announceDrainMax)
		select {
		case <-a.drained:
		case <-a.ctx.Done():
			p.Out.FlushVoice()
		case <-t.C:
			log.Printf("[media] announcement %s: no end report in %v", url, announceDrainMax)
			p.voiceOwn.Store(false)
		}
		t.Stop()
	}
	if p.Duck != nil {
		p.Duck(false)
	}
}

func (p *Player) announceDone(a *announcement) {
	a.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ann == a {
		p.ann = nil
		p.reportLocked()
	}
}
