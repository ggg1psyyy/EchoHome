package client

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/wilbowes/EchoMuse/internal/aec"
	"github.com/wilbowes/EchoMuse/internal/beamformer"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/pkg/board"
	"github.com/wilbowes/EchoMuse/pkg/mic"
	"github.com/wilbowes/EchoMuse/pkg/speaker"
)

// ─── WebSocket keepalive (data + control) ─────────────────────────────────────
//
// Neither long-lived socket had transport-level liveness before v2.8.4. The
// data client blocked in ReadMessage with no read deadline and sent no pings,
// so a silently dropped TCP connection (WiFi blip / AP roam — no FIN or RST
// ever delivered) left it half-open forever: connect() never returned, Run()'s
// redial loop never started, and the device kept a zombie data channel — deaf
// and mute — while the control socket kept it looking healthy. Observed
// 2026-07-14: Office wedged this way for 7h; the controller's defensive
// mic_start fired every 10s against a stream writing into the dead socket.
// The control client's app-level pong ticker had the write-side half of the
// same bug: on write error it returned without closing the conn, leaving its
// read loop wedged identically.
//
// Pings prove the full round trip (device → controller → device); the pong
// handler refreshes the read deadline, so a dead path errors the read loop
// out within wsPongWait and the redial loop takes over. Write deadlines bound
// every write so a full kernel send buffer can't hold connMu forever.
const (
	wsPingInterval = 20 * time.Second // WS ping cadence
	wsPongWait     = 45 * time.Second // read deadline; > 2× ping interval
	wsWriteWait    = 10 * time.Second // per-write deadline (frames, identify, pings)
)

// ─── Microphone frames ────────────────────────────────────────────────────────

const vadOwwChunkBytes = 1280 * 2 // 2560 bytes = 80ms, the satellite's frame

func vadPeriodRMS(mono []byte) float64 {
	n := len(mono) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := int16(binary.LittleEndian.Uint16(mono[i*2:]))
		f := float64(s) / 32768.0
		sum += f * f
	}
	return math.Sqrt(sum / float64(n))
}

// ─── DataClient ───────────────────────────────────────────────────────────────

type DataClient struct {
	deviceID string
	mic      mic.Subscribable
	spk      speaker.Speaker

	readyCh chan string

	// The satellite's microphone stream (data_call.go): micActive while one
	// runs, micStopCh its identity and stop signal, callTap where its
	// frames go.
	micMu     sync.Mutex
	micActive bool
	micStopCh chan struct{}
	callTap   atomic.Pointer[func([]byte)]

	conn   *websocket.Conn
	connMu sync.Mutex

	beam *beamformer.Beamformer
	aec  *aec.Canceller

	// Hardware echo reference detection (#385). Ch8 of the mic capture is a
	// loopback of the device's own playback on biscuit, arriving in the same
	// TDM frame as the mic samples, which makes it a far-end reference that
	// needs no alignment. Touched only from the mic goroutine.
	//
	// DETECTED, NEVER ASSUMED, and the discriminator is deliberately narrow:
	// a reference channel is BIT-EXACT ZERO when nothing is playing and
	// carries audio when something is, and only both facts together confirm
	// it. "Has energy" alone would promote a genuine microphone on a board
	// that wires ch8 differently, and cancelling the near-end against
	// another mic is far worse than not cancelling at all. A board that
	// fails either test simply keeps the software tap.
	// headroom is the board's aecHeadroomFor, fixed at construction.
	headroom float64
	// makeupClipped counts samples clamped giving aecHeadroom back after
	// the canceller; reported with the extraction's own clipping.
	makeupClipped uint64

	hwRefSeenSilent bool
	hwRefSeenAudio  bool
	hwRefOn         bool
	refGaps         int       // locked batches with no reference, given silence
	refLogged       [4]int    // moves, losses, late, gaps at the last [echoref] line
	refLogAt        time.Time // when that line was

	// hwRefMode is the operator's override, config.AecRef{Auto,HW,SW}. It
	// is the ONE field here written from another goroutine — the control
	// plane, on every config push — so it is atomic while the rest stay
	// plain: they are the detector's own state and never leave the mic
	// goroutine.
	hwRefMode atomic.Int32

	// pipeMu serialises access to beam, which holds unsynchronised
	// per-period state (reused analysis buffers, smoothers). A stream closed
	// and opened again in quick succession (a mute and unmute) can leave the
	// old goroutine draining a period while the new one starts. Uncontended
	// otherwise: a no-op lock per 160ms batch.
	pipeMu sync.Mutex
}

// NewDataClient wires the mic/speaker pipeline. canceller is the shared AEC
// instance — its far-end side is fed by the speaker's echo tap; this client
// runs its near-end side on the mono mic stream. Disabled cancellers pass
// audio through untouched.
func NewDataClient(deviceID string, microphone mic.Subscribable, spk speaker.Speaker, canceller *aec.Canceller) *DataClient {
	d := &DataClient{
		deviceID: deviceID,
		mic:      microphone,
		spk:      spk,
		readyCh:  make(chan string, 1),
		beam:     beamformer.New(board.CurrentLayout().Mics),
		headroom: aecHeadroomFor(board.CurrentLayout().Mics),
		aec:      canceller,
	}
	// Seeded from the env default so a device that never reaches a
	// controller still honours EM_AEC_HW_REF; the first config push
	// supersedes it (SetAecRefSource).
	d.hwRefMode.Store(aecRefModeOf(config.Get().Snapshot().AecRefSource))
	return d
}

// Far-end reference override, as an int so it can be atomic. Mirrors
// config.AecRef{Auto,HW,SW} — converted once on the control goroutine rather
// than string-compared per period on the mic goroutine.
const (
	hwRefAuto int32 = iota
	hwRefForceHW
	hwRefForceSW
)

func aecRefModeOf(v string) int32 {
	switch v {
	case config.AecRefHW:
		return hwRefForceHW
	case config.AecRefSW:
		return hwRefForceSW
	default:
		return hwRefAuto
	}
}

// SetAecRefSource applies the operator's far-end reference override. Called
// from the control goroutine on every config push; a no-op when unchanged.
//
// The SWITCH itself is left to the mic goroutine (noteEchoRef), which owns
// the canceller's source. Flipping it here would race a period already
// mid-flight: ProcessWithRef checks the canceller's own hwRef flag, and
// changing that between the caller's extraction and the call turns a
// perfectly good reference into a "missing or mismatched" pass-through.
func (d *DataClient) SetAecRefSource(v string) {
	mode := aecRefModeOf(v)
	if d.hwRefMode.Swap(mode) == mode {
		return
	}
	log.Printf("[aec] far-end reference source: %s", v)
}

// aecHeadroom is the gain (6dB) the mics are extracted without while the
// canceller is armed, and applyMakeup gives back after it, unless the board
// states its own (board.MicArray.AECHeadroomDb).
const aecHeadroom = 2.0

// aecHeadroomGain is d.headroom, or the default for a client built without
// NewDataClient (a zero would divide the extraction by nothing).
func (d *DataClient) aecHeadroomGain() float64 {
	if d.headroom <= 0 {
		return aecHeadroom
	}
	return d.headroom
}

// aecHeadroomFor is the board's headroom as a linear gain.
func aecHeadroomFor(a board.MicArray) float64 {
	if a.AECHeadroomDb <= 0 {
		return aecHeadroom
	}
	return math.Pow(10, a.AECHeadroomDb/20)
}

// applyMakeup scales S16LE mono in place by g, clamping, and returns how
// many samples it clamped.
func applyMakeup(b []byte, g float64) (clipped uint64) {
	for i := 0; i+1 < len(b); i += 2 {
		v := math.Round(float64(int16(binary.LittleEndian.Uint16(b[i:]))) * g)
		if v > 32767 {
			v = 32767
			clipped++
		} else if v < -32768 {
			v = -32768
			clipped++
		}
		binary.LittleEndian.PutUint16(b[i:], uint16(int16(v)))
	}
	return clipped
}

// noteEchoRef feeds one period of the candidate reference channel to the
// detector and reports whether the hardware path should be used for it.
//
// Confirmation is one-way. Once ch8 has been seen both bit-exact silent and
// carrying audio, it is a reference and stays one: the alternative is a
// device that flips sources mid-stream every time the room goes quiet, which
// throws away a converged filter for nothing.
func (d *DataClient) noteEchoRef(ref []byte) bool {
	switch d.hwRefMode.Load() {
	case hwRefForceSW:
		// Reversible, unlike the auto latch below: this is somebody running
		// an A/B, and a pin that could not be undone without a reboot would
		// make the comparison a one-way trip.
		if d.hwRefOn {
			d.hwRefOn = false
			d.aec.SetHardwareRef(false)
		}
		return false
	case hwRefForceHW:
		if !d.hwRefOn {
			d.hwRefOn = true
			d.aec.SetHardwareRef(true)
		}
		return true
	}
	if d.hwRefOn {
		return true
	}
	if ref == nil {
		return false
	}
	silent := true
	for _, b := range ref {
		if b != 0 {
			silent = false
			break
		}
	}
	if silent {
		d.hwRefSeenSilent = true
	} else {
		d.hwRefSeenAudio = true
	}
	if d.hwRefSeenSilent && d.hwRefSeenAudio {
		d.hwRefOn = true
		d.aec.SetHardwareRef(true)
		log.Printf("[aec] ch8 confirmed as the playback loopback " +
			"(bit-exact silent when idle, audio when playing) — " +
			"using the frame-aligned hardware reference")
		return true
	}
	return false
}

// loopbackRef is the aligned reference for this batch from a microphone
// whose board has a separate loopback (mic.EchoAligned), or (nil, false) on
// every other board. A nil reference with true means the aligner has not
// found the delay (the capture loop feeds its search, so it is warm before a
// turn starts); mono is only measured for its length.
func (d *DataClient) loopbackRef(raw, mono []byte) ([]byte, bool) {
	ea, ok := d.mic.(mic.EchoAligned)
	if !ok || ea.EchoAligner() == nil {
		return nil, false
	}
	if d.hwRefMode.Load() == hwRefForceSW {
		return nil, true
	}
	idx, ok := ea.BatchIndex(raw)
	if !ok {
		return nil, true
	}
	al := ea.EchoAligner()
	ref := al.Take(idx, len(mono)/2)
	ref, filled := fillRefGap(ref, al.Locked(), len(mono))
	if filled {
		d.refGaps++
	}
	if al.Locked() && time.Since(d.refLogAt) > time.Minute {
		g := [4]int{al.Moves, al.Losses, al.Late, d.refGaps}
		if g != d.refLogged {
			d.refLogged, d.refLogAt = g, time.Now()
			log.Printf("[echoref] offset %d, moves %d, losses %d, late %d, gaps filled %d",
				al.Offset(), g[0], g[1], g[2], g[3])
		}
	}
	return ref, true
}

// fillRefGap gives a locked aligner's missing reference (late, or gone from
// the ring) as n bytes of silence, so the canceller stays on the loopback.
// Returning nil there dropped to the software tap and back, rebuilding the
// canceller each time: hundreds of times an hour on 41C8 at idle, so every
// reply began from the saved path and took ~10s to converge (2026-11-19).
// Unlocked stays nil: no delay, no reference.
func fillRefGap(ref []byte, locked bool, n int) ([]byte, bool) {
	if ref != nil || !locked {
		return ref, false
	}
	return make([]byte, n), true
}

// useLoopbackRef moves the canceller between the aligned loopback and the
// software tap. Reversible both ways, unlike noteEchoRef's latch: the
// aligner lets go when it loses the delay, and a rebuilt filter costs a
// few seconds of playback, where an unaligned one cancels nothing.
func (d *DataClient) useLoopbackRef(on bool) {
	if on == d.hwRefOn {
		return
	}
	d.hwRefOn = on
	d.aec.SetRefClean(on) // echoref low-passes it; nothing for dealias to repair
	d.aec.SetHardwareRef(on)
	if on {
		log.Printf("[aec] loopback aligned (offset %d samples) — using it as the reference",
			d.mic.(mic.EchoAligned).EchoAligner().Offset())
	} else {
		log.Printf("[aec] loopback not aligned — software tap until it is")
	}
}

func (d *DataClient) NotifyReady(serverAddr string) {
	select {
	case d.readyCh <- serverAddr:
	default:
		select {
		case <-d.readyCh:
		default:
		}
		d.readyCh <- serverAddr
	}
}

func (d *DataClient) Run(ctx context.Context) error {
	var lastAddr string
	for {
		var addr string
		if lastAddr == "" {
			// No previous address — block until control signals us.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case addr = <-d.readyCh:
			}
		} else {
			// Lost connection — retry same addr after 5s, or use new addr
			// immediately if control signals one (e.g. controller moved).
			select {
			case <-ctx.Done():
				return ctx.Err()
			case addr = <-d.readyCh:
			case <-time.After(5 * time.Second):
				addr = lastAddr
			}
		}
		// Drain any stale addr queued while we were connected.
		select {
		case addr = <-d.readyCh:
		default:
		}
		lastAddr = addr
		log.Printf("[data] Connecting to %s", addr)
		if err := d.connect(ctx, addr); err != nil && err != context.Canceled {
			log.Printf("[data] Connection lost: %v — retrying", err)
		}
	}
}

// connect dials baseURL+"/data" — baseURL ("ws://…" or "wss://…") comes
// from the control client via NotifyReady, so both planes always ride the
// same listener. Credentials are re-read per dial (see tlscreds.go).
func (d *DataClient) connect(ctx context.Context, baseURL string) error {
	creds := loadLinkCreds()
	dialer := creds.dialer()
	conn, _, err := dialer.DialContext(ctx, baseURL+"/data", creds.headerFor(baseURL))
	if err != nil {
		return err
	}
	defer conn.Close()

	identifyBytes, _ := json.Marshal(map[string]string{
		"type":      "identify",
		"device_id": d.deviceID,
	})
	// Send identify BEFORE publishing conn — same ordering fix as the control
	// client's register message. StartMic can fire independently of controller
	// timing (unmute calls StartMic(false) from the button goroutine); once
	// d.conn is visible, streamMic's sendFrame writes under connMu, and this
	// unlocked write racing it would be a concurrent write on the same
	// gorilla conn (panics).
	conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if err := conn.WriteMessage(websocket.TextMessage, identifyBytes); err != nil {
		return err
	}
	log.Printf("[data] Identified as %s", d.deviceID)

	d.connMu.Lock()
	d.conn = conn
	d.connMu.Unlock()

	// Exit cleanup is ownership-guarded (2026-07-16): the control client
	// cancels this connection's context and spawns a replacement data.Run on
	// every control reconnect, so by the time this defer runs a replacement
	// may already be live. Nothing else rides the connection since EchoHome:
	// the satellite's microphone and every stream the speaker plays are the
	// Echo's own, and outlive it.
	defer func() {
		d.connMu.Lock()
		if d.conn == conn {
			d.conn = nil
		}
		d.connMu.Unlock()
	}()

	// Keepalive — see the wsPingInterval block comment. Deadline refreshes
	// all happen on this (the read) goroutine: the pong handler runs inside
	// ReadMessage, and the per-message refresh below covers connections busy
	// with speaker traffic.
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	done := make(chan struct{})
	defer close(done)

	// Context watcher — cancellation must tear down an ESTABLISHED
	// connection, not just abort a dial. The control client cancels this
	// context on every control-WS reconnect; before this watcher existed,
	// a control-only drop (data TCP path still healthy) left this
	// connection — and its mic stream — alive as a zombie: the stream held
	// micActive against a socket the controller had already superseded, so
	// every subsequent mic_start was refused with "already active" and the
	// device was deaf to wake words until something sent mic_stop (Office,
	// 2026-07-16, 4.7h). Closing conn errors the read loop out; the exit
	// defer then releases the mic stream for the replacement connection.
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			log.Println("[data] context cancelled — closing connection")
			conn.Close()
		}
	}()

	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// WriteControl is safe concurrently with WriteMessage
				// (gorilla's documented exception), so no connMu here — a
				// mic-frame write wedged on a full send buffer can't block
				// the ping that would detect the dead path.
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
					log.Printf("[data] keepalive ping failed: %v — closing connection", err)
					conn.Close() // unblock the read loop now, not at the read deadline
					return
				}
			}
		}
	}()

	// Nothing arrives here that the Echo acts on since EchoHome: the
	// controller sends no audio. The read loop stays for the deadline and
	// the keepalive's pongs.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
	}
}

// streamMic runs the microphone pipeline for the Voice Incubator satellite
// (data_call.go): the wake mic (the beamformer is never locked now), the
// fixed mic gain, the canceller with its far-end reference, then 80 ms
// frames to the satellite's tap. No gate and no AGC, as the wake stream it
// grew from always was. Nothing goes to the controller (EchoHome: its Home
// Assistant voice mode, bounded turns and listening sessions are gone).
// Started by OpenCallMic and stopped by CloseCallMic; stopCh identifies
// this stream.
func (d *DataClient) streamMic(stopCh <-chan struct{}) {
	if d.mic == nil {
		log.Println("[data] streamMic: no mic")
		return
	}
	defer func() {
		d.micMu.Lock()
		if d.micStopCh == stopCh {
			d.micActive = false
		}
		d.micMu.Unlock()
		log.Println("[data] streamMic: exited")
	}()

	ch := d.mic.Subscribe()
	defer d.mic.Unsubscribe(ch)

	cfg := config.Get()
	// The beamformer is never locked now, so it always reads the wake mic
	// and the steering angle has nothing to steer (-1: none).
	const beamAngle = float64(-1)
	buf := make([]byte, 0, vadOwwChunkBytes*4)
	var periodCount uint64 // periodic level diagnostic
	var lastClipped uint64 // clip count at last diag line

	// Memoized linear mic gain — recomputed only when the config dB value
	// changes (config push mid-stream). Sentinel forces computation on the
	// first period.
	gainDb := -1
	gainLin := 1.0

	for {
		select {
		case <-stopCh:
			return

		case raw, ok := <-ch:
			if !ok {
				return
			}
			// Stop has priority: select picks randomly among ready cases.
			select {
			case <-stopCh:
				return
			default:
			}

			snap := cfg.Snapshot()
			if snap.MicGainDb != nil && *snap.MicGainDb != gainDb {
				gainDb = *snap.MicGainDb
				gainLin = math.Pow(10, float64(gainDb)/20.0)
				log.Printf("[data] mic gain: %ddB (linear %.2f)", gainDb, gainLin)
			}

			d.pipeMu.Lock()
			if snap.WakeMic != nil {
				d.beam.SetWakeMic(*snap.WakeMic)
			}
			// With the canceller armed the mics are extracted with
			// aecHeadroom in hand and it is given back after cancellation:
			// at 24dB the echo of loud playback clipped the S16 extraction
			// (943 samples in a minute on VVV, 2026-10-18), clipping is
			// distortion no canceller removes. After cancellation the echo
			// is 15-25dB down, so the make-up cannot clip it; the overall
			// gain is micGainDb either way.
			aecArmed := d.aec.Enabled()
			extractGain := gainLin
			if aecArmed {
				extractGain = gainLin / d.aecHeadroomGain()
			}
			mono, _ := d.beam.Process(raw, beamAngle, extractGain)

			// AEC — subtract the speaker's own output before anything hears
			// the signal. No-op while aecEnabled=false. Two far-end sources
			// (#385): the hardware reference (ch8 of THIS period, the
			// device's own playback looped back in the same TDM frame, so
			// aligned by construction), and the software tap at the speaker's
			// ALSA write for any board where ch8 does not prove itself (see
			// noteEchoRef). Only extracted when cancellation is armed.
			var echoRef []byte
			loopRef := false
			if d.aec.Enabled() {
				echoRef = d.beam.EchoRef(raw)
				if echoRef == nil {
					echoRef, loopRef = d.loopbackRef(raw, mono)
				}
			}
			if loopRef {
				// The Dot 3's loopback, aligned by echoref: the hardware path
				// only while it is locked, never the one-way latch below,
				// since a hardware path with no reference passes audio
				// through uncancelled.
				d.useLoopbackRef(echoRef != nil)
			}
			if echoRef != nil && (loopRef || d.noteEchoRef(echoRef)) {
				// Every microphone is cancelled, not only the one in use:
				// the echo path differs per microphone (#814).
				sel := d.beam.OutputChannel(beamAngle)
				mono = d.aec.ProcessMicsWithRef(d.beam.MicChannels(raw, extractGain, sel, mono), sel, echoRef)
			} else {
				mono = d.aec.Process(mono)
			}
			if aecArmed {
				d.makeupClipped += applyMakeup(mono, d.aecHeadroomGain())
			}
			clipped := d.beam.ClippedSamples() + d.makeupClipped

			// Periodic level diagnostic — every ~10 min, or within ~16s of
			// the mic gain clamping a sample (clipping is the one signal
			// that says micGainDb is too hot for the room).
			if periodCount%3750 == 0 || (clipped != lastClipped && periodCount%100 == 0) {
				log.Printf("[data] mic diag: rms=%.5f gain=%ddB clipped=%d",
					vadPeriodRMS(mono), gainDb, clipped)
				lastClipped = clipped
			}
			periodCount++
			d.pipeMu.Unlock()

			// 80 ms frames, one copy each: the satellite keeps them.
			buf = append(buf, mono...)
			for len(buf) >= vadOwwChunkBytes {
				chunk := make([]byte, vadOwwChunkBytes)
				copy(chunk, buf[:vadOwwChunkBytes])
				buf = buf[vadOwwChunkBytes:]
				if tap := d.callTap.Load(); tap != nil {
					(*tap)(chunk)
				}
			}
		}
	}
}
