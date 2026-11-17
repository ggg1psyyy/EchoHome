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
	// ListenStarted and ListenEnded bracket the microphone going to the
	// app: the duck of music.
	ListenStarted()
	ListenEnded()
	// Show puts the satellite's state on the ring (ring.go). Called under
	// the session's lock, in order: it must not block or call back.
	Show(r Ring)
}

const (
	// PeriodBytes is one voice-plane period: 2048 samples at 48 kHz.
	PeriodBytes = 4096
	// ListenLimit is the Echo's own cap on the microphone going to the app,
	// from CALL_START to the end of the command: the private session's
	// maxOpen, and the controller's satellite's CALL_MAX_S. No app can hold
	// the microphone open longer.
	ListenLimit = 30 * time.Second
	// micQueue is how much microphone audio may wait for the socket: about
	// 2.5 s in the wake stream's 80 ms chunks. Past it, audio is dropped and
	// counted, never left to block the microphone path.
	micQueue      = 32
	writeDeadline = 5 * time.Second
)

// Variables so the tests need not wait them out.
var (
	listenLimit = ListenLimit
	// replyWait is how long the app has, once the command ended, to begin
	// its reply (the controller's TTS wait); hangupWait, once a reply has
	// played, to hang up (its HANGUP_WAIT_S). Past either the Echo ends the
	// call: CALL_END timeout.
	replyWait  = 30 * time.Second
	hangupWait = 10 * time.Second
	// drainGrace is how long past a stream's own length a missing playback
	// report is waited for before PLAYBACK_DRAINED is sent anyway: the app
	// would otherwise stall 30 s.
	drainGrace = 3 * time.Second
)

// commandEnded are the EVENTs that mean the app has stopped listening to
// the command (the controller's satellite's COMMAND_ENDED): the microphone
// closes. So does the first AUDIO_OUT of a call.
var commandEnded = map[string]bool{
	"processing": true, "recognized": true, "not_recognized": true,
	"responding": true, "result": true,
}

// callEvents are the EVENTs about a call itself (the controller's
// satellite's CALL_EVENTS): the first marks where its own traffic begins.
var callEvents = map[string]bool{
	"processing": true, "recognized": true, "not_recognized": true,
	"responding": true, "result": true,
	"listening": true, "wake_rejected": true, "wake_overturned": true,
}

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

// call is one call. While listening its microphone frames go from the
// session's worker to the socket through mic, never blocking it, after the
// pre-roll; done ends both sides.
type call struct {
	mic       chan []byte
	done      chan struct{}
	dropped   atomic.Int64
	listening bool
	preroll   []byte // sent first: the audio before a detection
	from      uint64 // the last chunk before it (wake.heard): later ones are its
	// stale: the call cut audio the app was playing outside it (a ring, an
	// announcement), whose frames still in flight are not its reply. Until
	// the app's first message about the call (callEvents, HANGUP) they are
	// dropped, as the controller's satellite drops them (Call.stale).
	stale bool
	// what the app said of it, for the ring. The app's sounds and its answer
	// share one stream (voice_incubator server.py Cues): the "heard" sound
	// plays the moment the command ends, the answer after `responding`. So
	// the first audio closes the microphone, and only audio after
	// `responding` is the answer.
	responding bool   // `responding`: the answer is being said
	spoke      bool   // the answer has sounded
	failed     bool   // a `result` with ok false
	hangup     string // the HANGUP's reason
}

// push hands a chunk to the call's sender, or drops and counts it.
func (c *call) push(pcm []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.mic <- pcm:
	default:
		c.dropped.Add(int64(len(pcm)))
	}
}

// session is one app connection: calls, playback and the button's meaning.
//
// A call listens from CALL_START until the command ends (an EVENT in
// commandEnded, or the reply's first audio), as a private session does
// until end of speech; then it stays open, microphone closed, until the
// app's HANGUP. The reply is never cut by a limit: the limits are on
// listening, on the reply beginning, and on the hang-up after it.
type session struct {
	w      *writer
	dev    Device
	report func() // the listening state changed; called without the lock

	mu         sync.Mutex // guards call, timer, timerRings, announcing, closed, micOn
	call       *call      // nil when not in a call
	timer      *time.Timer
	timerRings bool
	announcing bool
	closed     bool
	micOn      bool
	muted      bool // the Echo's mute: no microphone at all
	replying   bool // a call's reply is sounding (until its PLAYBACK_DRAINED)
	shown      Ring // what the ring was last given
	askBack    bool // a HANGUP asked for an answer: its call follows at once

	wake *wake // wake.go

	discard atomic.Bool // after a cancel, until the stream's end

	// playback: the reading goroutine's, and a new call's (endStreamHere)
	playMu    sync.Mutex
	up        Upsampler
	pending   []byte // 48 kHz bytes short of a period
	periods   int
	playStart time.Time
	drained   chan struct{}
}

func newSession(c net.Conn, dev Device, report func()) *session {
	s := &session{w: &writer{c: c}, dev: dev, report: report,
		drained: make(chan struct{}, 1), wake: newWake(), muted: dev.Muted()}
	go s.wakeLoop()
	return s
}

// wakeModels is how many wake word models it holds.
func (s *session) wakeModels() int { return int(s.wake.models.Load()) }

// syncMicLocked opens the microphone while something needs it (a wake word
// model to run, a call listening) and closes it once nothing does.
func (s *session) syncMicLocked() {
	want := !s.closed && !s.muted && (s.wake.models.Load() > 0 || (s.call != nil && s.call.listening))
	if want == s.micOn {
		return
	}
	s.micOn = want
	if want {
		s.wake.opened.Add(1)
		s.dev.MicOpen(s.onMic)
	} else {
		s.dev.MicClose()
	}
}

// listening says whether the microphone is going to the app.
func (s *session) listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.call != nil && s.call.listening
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
		s.showLocked()
		s.mu.Unlock()
		s.w.json(DeviceEvent, DeviceEventInfo{Type: "stop_timer"})
	case s.call != nil:
		was := s.endLocked("cancelled")
		s.mu.Unlock()
		s.cut()
		s.after(was)
	case muted:
		s.mu.Unlock()
	default:
		started := s.startLocked(CallStartInfo{WakeWord: WakeManual}, nil, s.wake.heard.Load())
		s.mu.Unlock()
		if started {
			log.Printf("[incubator] call started (manual)")
			s.dev.ListenStarted()
			s.report()
		}
	}
}

// setMuted is the Echo's mute. Muted, a call is cancelled and the
// microphone closed, models or not: nothing is heard, and the pre-roll
// starts over when it opens again, so no audio from while it was muted can
// lead a later call.
func (s *session) setMuted(muted bool) {
	if muted {
		s.cancel()
	}
	s.mu.Lock()
	s.muted = muted
	s.syncMicLocked()
	s.mu.Unlock()
}

// cancel ends a call from the Echo's side.
func (s *session) cancel() {
	s.mu.Lock()
	open := s.call != nil
	was := s.endLocked("cancelled")
	s.mu.Unlock()
	if open {
		s.cut()
		s.after(was)
	}
}

// after is what follows a change, without the lock: the ring and a report
// when the microphone has just closed.
func (s *session) after(wasListening bool) {
	if wasListening {
		s.dev.ListenEnded()
		s.report()
	}
}

// startLocked opens a call: a manual one with no pre-roll (nothing from
// before the press leaves the Echo), or a wake word's with the audio before
// its detection. What plays is cut, as a call means "listen to me", and a
// stream the app left open is ended (flushed first, so a reply blocked on a
// full voice plane lets go of it).
func (s *session) startLocked(info CallStartInfo, preroll []byte, from uint64) bool {
	stale := s.timerRings || s.announcing || s.streamOpen()
	s.dev.Flush()
	s.endStreamHere()
	s.playbackDrained()
	if err := s.w.json(CallStart, info); err != nil {
		return false
	}
	c := &call{mic: make(chan []byte, micQueue), done: make(chan struct{}), listening: true, preroll: preroll, from: from, stale: stale}
	s.call = c
	s.armLocked(c, listenLimit)
	go s.sendMic(c)
	s.syncMicLocked()
	s.replying = false // endStreamHere ended any reply still open
	s.showLocked()
	return true
}

// armLocked (re)starts the call's one timer: past it the Echo ends the call.
func (s *session) armLocked(c *call, d time.Duration) {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(d, func() {
		s.mu.Lock()
		if s.call != c {
			s.mu.Unlock()
			return
		}
		was := s.endLocked("timeout")
		s.mu.Unlock()
		s.after(was)
	})
}

// stopListeningLocked closes the microphone: the command has ended. The
// call stays open for its reply, with replyWait to begin it.
func (s *session) stopListeningLocked(why string) bool {
	c := s.call
	if c == nil || !c.listening {
		return false
	}
	close(c.done)
	c.listening = false
	s.syncMicLocked()
	s.armLocked(c, replyWait)
	if n := c.dropped.Load(); n > 0 {
		log.Printf("[incubator] listening ended (%s), %d microphone bytes dropped", why, n)
	} else {
		log.Printf("[incubator] listening ended (%s)", why)
	}
	return true
}

// endLocked closes the call, the microphone first so nothing more leaves,
// and reports whether it was still listening (the caller then calls after,
// without the lock). reason "" is the app's HANGUP or its going away: no
// CALL_END back. What is queued keeps playing, except after a cancel (the
// caller cuts it).
func (s *session) endLocked(reason string) bool {
	c := s.call
	if c == nil {
		return false
	}
	shown := reason
	if shown == "" {
		shown = "hangup"
	}
	was := s.stopListeningLocked(shown)
	s.timer.Stop()
	s.call = nil
	if reason != "" {
		if reason == "cancelled" {
			s.discard.Store(true)
		}
		s.replying = false // cut (a cancel), or long over (a timeout)
		s.w.json(CallEnd, CallEndInfo{Reason: reason})
	}
	s.outcomeLocked(c, reason)
	log.Printf("[incubator] call ended (%s)", shown)
	return was
}

func (s *session) sendMic(c *call) {
	for pre := c.preroll; len(pre) > 0; {
		select {
		case <-c.done: // ended (a mute, a cancel) before its pre-roll was out
			return
		default:
		}
		n := min(len(pre), MaxPayload)
		if s.w.frame(AudioIn, pre[:n]) != nil {
			return
		}
		pre = pre[n:]
	}
	c.preroll = nil
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

// close ends whatever runs: the app went away.
func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	was := s.endLocked("")
	s.syncMicLocked()
	s.replying, s.timerRings, s.announcing = false, false, false
	s.showLocked()
	s.mu.Unlock()
	s.stopWake()
	s.endStreamHere()
	s.after(was)
}

// handle is one frame from the app, on the reading goroutine.
func (s *session) handle(m Message, p []byte) error {
	switch m {
	case AudioOut:
		if s.discard.Load() {
			return nil // a cancelled call's, first: never a new call's reply
		}
		if s.staleCall() {
			return nil // the ring or announcement the call cut
		}
		s.commandOver("reply")
		return s.audio(p)
	case AudioOutEnd:
		if s.discard.Load() {
			s.streamEnd() // the cancelled stream's end
			return nil
		}
		if s.staleCall() {
			// Its stream was ended here when the call cut it.
			log.Printf("[incubator] AUDIO_OUT_END of the playback a call cut — dropped")
			return nil
		}
		s.streamEnd()
	case Hangup:
		var h HangupInfo
		if len(p) > 0 {
			if err := Decode(m, p, &h); err != nil {
				return err
			}
		}
		// With listen the app asks for an answer (its question and the
		// sound saying it is the speaker's turn have played out: it ends and
		// awaits its stream before this HANGUP, server.py _hang_up): the
		// call that hears it starts at once, with nothing from before it,
		// unless muted (PROTOCOL.md "Calls" 4).
		muted := s.dev.Muted()
		s.mu.Lock()
		c := s.call
		if c != nil {
			c.hangup = h.Reason
		}
		answer := h.Listen && c != nil && !muted && !s.closed
		s.askBack = answer
		was := s.endLocked("")
		started := answer && s.startLocked(CallStartInfo{WakeWord: WakeFollowUp}, nil, s.wake.heard.Load())
		s.askBack = false
		s.mu.Unlock()
		s.after(was)
		if started {
			log.Printf("[incubator] call started (follow_up)")
			s.dev.ListenStarted()
			s.report()
		}
	case ModelStart:
		s.modelStart(p)
	case ModelData:
		s.modelData(p)
	case ModelEnd:
		s.modelEnd()
	case ModelClear:
		s.modelClear()
	case Event:
		var e EventInfo
		if Decode(m, p, &e) == nil {
			s.mu.Lock()
			switch e.Type {
			case "timer_ringing":
				s.timerRings = true
			case "timer_stopped":
				s.timerRings = false
			case "announcement":
				var d struct {
					Active bool `json:"active"`
				}
				json.Unmarshal(e.Data, &d)
				s.announcing = d.Active
			}
			if c := s.call; c != nil && callEvents[e.Type] {
				c.stale = false
				switch e.Type {
				case "result":
					c.failed = resultFailed(e.Data)
				case "responding":
					c.responding = true
				}
			}
			s.showLocked()
			s.mu.Unlock()
			if commandEnded[e.Type] {
				s.commandOver(e.Type)
			}
		}
	}
	return nil
}

// commandOver closes the microphone if the call still listens.
func (s *session) commandOver(why string) {
	s.mu.Lock()
	was := s.stopListeningLocked(why)
	if c := s.call; c != nil && why == "reply" {
		if s.timer != nil {
			s.timer.Stop() // the reply is playing: no limit cuts it
		}
		if c.responding {
			c.spoke, s.replying = true, true
		}
	}
	s.showLocked() // the ring after it: endLocked shows the outcome instead
	s.mu.Unlock()
	s.after(was)
}

// audio plays AUDIO_OUT as it arrives, in calls and out of them. Play
// blocks while the voice plane is full, so the socket is read no faster
// than the reply plays: the app's own pacing.
func (s *session) audio(p []byte) error {
	if s.discard.Load() {
		return nil
	}
	s.playMu.Lock()
	defer s.playMu.Unlock()
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

// staleCall says whether the call in progress still waits for its own
// traffic, the audio arriving being the playback it cut.
func (s *session) staleCall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.call != nil && s.call.stale
}

// streamOpen says whether a stream is playing here: periods sent and no
// AUDIO_OUT_END yet.
func (s *session) streamOpen() bool {
	s.playMu.Lock()
	defer s.playMu.Unlock()
	return s.periods > 0 || len(s.pending) > 0
}

// endStreamHere ends, for the speaker, a stream the app left open: it
// stops sending a stream whose task it cancels (a call it ended, a ring
// stopped), with no AUDIO_OUT_END. Left open, the speaker would discard
// the NEXT stream up to its end after the next cut. Unanswered: nothing
// is waiting for it.
func (s *session) endStreamHere() {
	s.playMu.Lock()
	defer s.playMu.Unlock()
	if s.periods > 0 || len(s.pending) > 0 {
		s.dev.End()
		s.resetStream()
	}
}

// streamEnd is AUDIO_OUT_END: the filter's tail and the last period,
// padded, are played and the stream ended; PLAYBACK_DRAINED follows once
// the speaker reports it played, or at once for a stream that played
// nothing, or anyway if no report comes. The wait is off the reading
// goroutine, so a HANGUP or a cancel is still read meanwhile.
func (s *session) streamEnd() {
	if s.discard.Swap(false) {
		// The cancelled stream's end: the speaker is discarding until it.
		s.playMu.Lock()
		s.dev.End()
		s.resetStream()
		s.playMu.Unlock()
		s.drainedNow()
		return
	}
	s.playMu.Lock()
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
	if played > 0 {
		select {
		case <-s.drained: // a report from before this stream
		default:
		}
		s.dev.End()
	}
	s.playMu.Unlock()
	if played == 0 {
		s.drainedNow()
		return
	}
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
		s.drainedNow()
	}()
}

// drainedNow sends PLAYBACK_DRAINED; a call past its command then has
// hangupWait for the app to hang up.
func (s *session) drainedNow() {
	// Settled here before the app hears it, so what it does next (a HANGUP
	// with listen, say) finds the answer over.
	s.mu.Lock()
	if c := s.call; c != nil && !c.listening {
		s.armLocked(c, hangupWait)
	}
	s.replying = false
	s.showLocked()
	s.mu.Unlock()
	s.w.frame(PlaybackDrained, nil)
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
