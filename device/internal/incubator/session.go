package incubator

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Device is what a satellite session needs of the Echo: its processed
// microphone (16 kHz S16LE mono, delivered to the callback while open), its
// speaker's voice plane (48 kHz S16LE mono periods), and the local signs of
// a call (ring, duck). The firmware adapts its own parts to it; tests fake it.
type Device interface {
	// MicOpen starts delivering processed microphone audio to deliver, which
	// must not block; MicClose stops it.
	MicOpen(deliver func(pcm []byte))
	MicClose()
	// Play queues one period of PeriodBytes, blocking while the plane is
	// full: the app is paced by not reading the socket faster than that.
	Play(period []byte) error
	// End marks the end of the stream; its playback is then reported through
	// Server.Drained. Flush cuts what is queued now, and what arrives
	// until the next End.
	End()
	Flush()
	Muted() bool
	CallStarted()
	CallEnded()
}

const (
	// PeriodBytes is one voice-plane period: 2048 samples at 48 kHz.
	PeriodBytes = 4096
	// CallLimit is the Echo's own cap on a call, from CALL_START, reply
	// included (the reference firmware's 30 s): no app can hold the
	// microphone open longer.
	CallLimit = 30 * time.Second
	// micQueue is how much microphone audio may wait for the socket: about
	// 2.5 s in the wake stream's 80 ms chunks. Past it, audio is dropped and
	// counted, never left to block the microphone path.
	micQueue      = 32
	writeDeadline = 5 * time.Second
)

// Variables so the tests need not wait them out.
var (
	callLimit = CallLimit
	// drainGrace is how long past a stream's own length a missing playback
	// report is waited for before PLAYBACK_DRAINED is sent anyway: the app
	// would otherwise stall 30 s.
	drainGrace = 3 * time.Second
)

// writer sends whole frames from several goroutines: one lock, one write a
// frame, a deadline so a stalled app cannot hold a sender forever.
type writer struct {
	mu  sync.Mutex
	c   net.Conn
	buf []byte
}

func (w *writer) frame(m Message, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrTooLong
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf[:0], byte(m), byte(len(payload)>>8), byte(len(payload)))
	w.buf = append(w.buf, payload...)
	w.c.SetWriteDeadline(time.Now().Add(writeDeadline))
	_, err := w.c.Write(w.buf)
	return err
}

func (w *writer) json(m Message, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.frame(m, b)
}

// call is one call's microphone: frames go from the microphone path to the
// socket through mic, never blocking the microphone; done ends both sides.
type call struct {
	mic     chan []byte
	done    chan struct{}
	dropped atomic.Int64
}

// session is one app connection: calls, playback and the button's meaning.
type session struct {
	w      *writer
	dev    Device
	report func() // the call state changed; called without the lock

	mu         sync.Mutex
	call       *call // nil when not in a call
	callTimer  *time.Timer
	timerRings bool
	closed     bool

	discard atomic.Bool // after a cancel, until the stream's end

	// playback, owned by the reading goroutine
	up        Upsampler
	pending   []byte // 48 kHz bytes short of a period
	periods   int
	playStart time.Time
	drained   chan struct{}
}

func newSession(c net.Conn, dev Device, report func()) *session {
	return &session{w: &writer{c: c}, dev: dev, report: report,
		drained: make(chan struct{}, 1)}
}

func (s *session) inCall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.call != nil
}

// press is the action button as the controller passes it on, having
// decided it is neither a hold nor an event: the reference firmware's
// order. A ringing timer is stopped; a call is cancelled; otherwise, unless
// muted, a call starts.
func (s *session) press() {
	muted := s.dev.Muted() // before the lock: the Echo's, not the session's
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
	case s.timerRings:
		s.timerRings = false
		s.mu.Unlock()
		s.w.json(DeviceEvent, DeviceEventInfo{Type: "stop_timer"})
	case s.call != nil:
		s.endLocked("cancelled")
		s.mu.Unlock()
		s.cut()
		s.dev.CallEnded()
		s.report()
	case muted:
		s.mu.Unlock()
	default:
		started := s.startLocked()
		s.mu.Unlock()
		if started {
			s.dev.CallStarted()
			s.report()
		}
	}
}

// cancel ends a call from the Echo's side (mute): the microphone stops at
// once, the app is told.
func (s *session) cancel() {
	s.mu.Lock()
	ended := s.endLocked("cancelled")
	s.mu.Unlock()
	if ended {
		s.cut()
		s.dev.CallEnded()
		s.report()
	}
}

// startLocked opens a manual call. No pre-roll: nothing from before the
// press leaves the Echo. What plays is cut, as a press means "listen to me".
func (s *session) startLocked() bool {
	s.cut()
	if err := s.w.json(CallStart, CallStartInfo{WakeWord: WakeManual}); err != nil {
		return false
	}
	c := &call{mic: make(chan []byte, micQueue), done: make(chan struct{})}
	s.call = c
	s.callTimer = time.AfterFunc(callLimit, func() {
		s.mu.Lock()
		ended := s.call == c && s.endLocked("timeout")
		s.mu.Unlock()
		if ended {
			s.dev.CallEnded()
			s.report()
		}
	})
	go s.sendMic(c)
	s.dev.MicOpen(func(pcm []byte) {
		select {
		case <-c.done:
			return
		default:
		}
		select {
		case c.mic <- append([]byte(nil), pcm...):
		default:
			c.dropped.Add(int64(len(pcm)))
		}
	})
	log.Printf("[incubator] call started (manual)")
	return true
}

// endLocked closes the call, the microphone first so nothing more leaves.
// The caller then calls dev.CallEnded and report, without the lock.
// reason "" is the app's HANGUP or its going away: no CALL_END back. What is
// queued to play keeps playing, except after a cancel (the caller flushes).
func (s *session) endLocked(reason string) bool {
	c := s.call
	if c == nil {
		return false
	}
	s.dev.MicClose()
	close(c.done)
	s.call = nil
	s.callTimer.Stop()
	if reason != "" {
		if reason == "cancelled" {
			s.discard.Store(true)
		}
		s.w.json(CallEnd, CallEndInfo{Reason: reason})
	}
	if reason == "" {
		reason = "hangup"
	}
	if n := c.dropped.Load(); n > 0 {
		log.Printf("[incubator] call ended (%s), %d microphone bytes dropped", reason, n)
	} else {
		log.Printf("[incubator] call ended (%s)", reason)
	}
	return true
}

func (s *session) sendMic(c *call) {
	for {
		select {
		case <-c.done:
			return
		case pcm := <-c.mic:
			for len(pcm) > 0 {
				n := min(len(pcm), MaxPayload)
				if s.w.frame(AudioIn, pcm[:n]) != nil {
					return
				}
				pcm = pcm[n:]
			}
		}
	}
}

// close ends whatever runs: the app went away. On the reading goroutine,
// so a stream cut short by the app's leaving is ended for the speaker too.
func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	ended := s.endLocked("")
	s.mu.Unlock()
	if s.periods > 0 || len(s.pending) > 0 {
		s.dev.End()
	}
	if ended {
		s.dev.CallEnded()
		s.report()
	}
}

// handle is one frame from the app, on the reading goroutine.
func (s *session) handle(m Message, p []byte) error {
	switch m {
	case AudioOut:
		return s.audio(p)
	case AudioOutEnd:
		s.streamEnd()
	case Hangup:
		var h HangupInfo
		if len(p) > 0 {
			if err := Decode(m, p, &h); err != nil {
				return err
			}
		}
		s.mu.Lock()
		ended := s.endLocked("")
		s.mu.Unlock()
		if ended {
			s.dev.CallEnded()
			s.report()
		}
	case Event:
		var e EventInfo
		if Decode(m, p, &e) == nil {
			s.mu.Lock()
			switch e.Type {
			case "timer_ringing":
				s.timerRings = true
			case "timer_stopped":
				s.timerRings = false
			}
			s.mu.Unlock()
		}
	}
	return nil
}

// audio plays AUDIO_OUT as it arrives, in calls and out of them. Play
// blocks while the voice plane is full, so the socket is read no faster
// than the reply plays: the app's own pacing.
func (s *session) audio(p []byte) error {
	if s.discard.Load() {
		return nil
	}
	if s.periods == 0 && len(s.pending) == 0 {
		s.playStart = time.Now()
	}
	s.pending = s.up.Process(s.pending, p)
	for len(s.pending) >= PeriodBytes {
		if err := s.dev.Play(s.pending[:PeriodBytes]); err != nil {
			return err
		}
		s.periods++
		s.pending = append(s.pending[:0], s.pending[PeriodBytes:]...)
	}
	return nil
}

// streamEnd is AUDIO_OUT_END: the filter's tail and the last period,
// padded, are played and the stream ended; PLAYBACK_DRAINED follows once
// the speaker reports it played, or at once for a stream that played
// nothing, or anyway if no report comes. The wait is off the reading
// goroutine, so a HANGUP or a cancel is still read meanwhile.
func (s *session) streamEnd() {
	if s.discard.Swap(false) {
		// The cancelled stream's end: the speaker is discarding until it.
		s.dev.End()
		s.resetStream()
		s.w.frame(PlaybackDrained, nil)
		return
	}
	s.pending = s.up.Flush(s.pending)
	for len(s.pending) > 0 {
		period := make([]byte, PeriodBytes)
		n := copy(period, s.pending)
		s.pending = s.pending[n:]
		if s.dev.Play(period) != nil {
			break
		}
		s.periods++
	}
	played, start := s.periods, s.playStart
	s.resetStream()
	if played == 0 {
		s.w.frame(PlaybackDrained, nil)
		return
	}
	select {
	case <-s.drained: // a report from before this stream
	default:
	}
	s.dev.End()
	length := time.Duration(played) * PeriodBytes / 2 * time.Second / 48000
	wait := time.Until(start.Add(length)) + length + drainGrace
	go func() {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-s.drained:
		case <-t.C:
			log.Printf("[incubator] no playback report; PLAYBACK_DRAINED sent anyway")
		}
		s.w.frame(PlaybackDrained, nil)
	}()
}

func (s *session) resetStream() {
	s.up = Upsampler{}
	s.pending = s.pending[:0]
	s.periods = 0
}

// cut stops what plays (a cancel; a new call) and lets a PLAYBACK_DRAINED
// already awaited go at once: what it waited for will not play now, and a
// stream cut before it began never reports its end. A report that comes
// anyway is cleared by the next stream's end.
func (s *session) cut() {
	s.dev.Flush()
	s.playbackDrained()
}

// playbackDrained is the speaker's report that the stream played out.
func (s *session) playbackDrained() {
	select {
	case s.drained <- struct{}{}:
	default:
	}
}
