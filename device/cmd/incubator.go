package main

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wilbowes/EchoMuse/internal/bindings/speaker"
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/cue"
	"github.com/wilbowes/EchoMuse/internal/firewall"
	"github.com/wilbowes/EchoMuse/internal/incubator"
	"github.com/wilbowes/EchoMuse/internal/server"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// The Voice Incubator satellite (internal/incubator), run when the
// controller's incubator_satellite message says so. Like the Sendspin player
// it stops when the controller link goes and starts again on the next
// message, which the controller sends on connect, unless it is standalone
// (the Echo's opt-in switch): then it is saved on the Echo, started from it
// at boot, and keeps running without the controller, the buttons and the
// ring its own while its app is connected (cmd/server.go showLinkLost). It
// announces itself over mDNS; the controller announces it too, which is
// what crosses VLANs. The decisions are incubator.Manager's, tested there.

type echoFirewall struct{}

func (echoFirewall) Open(port int) error { return firewall.Open(port) }
func (echoFirewall) Close(port int)      { firewall.Close(port) }

var incubatorMgr *incubator.Manager

// incubatorOwnsVoice is set from a satellite stream's first period to the
// speaker's report of its end, so that report goes to the satellite
// (PLAYBACK_DRAINED) and not to the controller as a turn's playback_stats.
// The voice plane has one producer at a time in practice: under
// incubator_echo the controller runs no turns on the Echo. A controller
// cue or announcement played over a satellite reply would mix with it and
// end as one stream, reported to the satellite.
var incubatorOwnsVoice atomic.Bool

// echoParts is the Echo as a satellite call sees it.
type echoParts struct {
	dc  *client.DataClient
	spk *speaker.PcmSpeaker
	srv *server.Server

	mu     sync.Mutex
	ducked bool
	gen    int // the duck's last change: a pending unduck stands only if none came since
}

func (e *echoParts) MicOpen(deliver func([]byte)) { e.dc.OpenCallMic(deliver) }
func (e *echoParts) MicClose()                    { e.dc.CloseCallMic() }

func (e *echoParts) Play(period []byte) error {
	incubatorOwnsVoice.Store(true)
	return e.spk.PumpPeriod(period)
}

func (e *echoParts) End() { e.spk.EndStream() }

// Flush is the satellite cutting its own reply (a cancel, a new call). A
// stream cut while playing still reports its end, and that report is the
// satellite's: sent to the controller, it read as a turn's playback_stats
// (seen on 00LQ, 2026-11-17). One cut before it began to play never
// reports, so the plane is given up, or the next controller stream's report
// would come here. Whether it was playing is the ALSA loop's to know; a
// period sounding now, or within a period of now, is the sign of it.
func (e *echoParts) Flush() {
	playing := time.Now().Before(e.spk.VoiceSoundingUntil().Add(50 * time.Millisecond))
	e.spk.Flush()
	if !playing {
		incubatorOwnsVoice.Store(false)
	}
}
func (e *echoParts) Muted() bool      { return e.srv.IsMuted() }
func (e *echoParts) MusicPress() bool { return musicPress() }
func (e *echoParts) StopAnnouncement() {
	if mediaPlayer != nil {
		mediaPlayer.StopAnnouncement()
	}
}

// Show puts the satellite's state on the ring with the scene the controller
// pushed: its listening ring (listeningAnim) and the rest (incubatorRing).
// A state with no spec goes dark, so an older controller leaves the ring as
// it was before incubator_ring (lit while listening, dark otherwise). The
// meter and the timer's pulse are held until the satellite clears them:
// their TTLs are a dead man for a controller that sends them from afar, and
// here the satellite in this process clears every path itself (a reply
// longer than any TTL would otherwise go dark while it plays).
func (e *echoParts) Show(v incubator.RingView) {
	r := v.State
	if spec, ok := intercomAnims[r]; ok {
		// The Echo's own, never a scene's, never dimmed at night: someone is
		// calling, or this room is heard in another.
		e.srv.StartAnim(spec)
		e.duckFor(r)
		return
	}
	if r == incubator.RingMonitored {
		// The Echo's own, never a scene's: a privacy indicator, as the mute
		// ring is. Red as the VPE's ring blinks while monitored; a pulse,
		// never the mute ring's solid red, which means the opposite. Never
		// dimmed or recoloured at night.
		e.srv.StartAnim(monitoredAnim)
		e.duckFor(r)
		return
	}
	snap := config.Get().Snapshot()
	var specs map[string]json.RawMessage
	json.Unmarshal(snap.IncubatorRing, &specs)
	var spec server.AnimSpec
	switch {
	case r == incubator.RingTimers:
		spec = timersAnim(v, specs["spin"])
	case v.Night && r == incubator.RingDone:
		spec = nightDoneAnim
	case v.Night && r == incubator.RingError:
		spec = nightErrorAnim
	default:
		var raw json.RawMessage
		if r == incubator.RingListening {
			raw = snap.ListeningAnim
		} else if r != incubator.RingOff {
			raw = specs[ringSpecKey[r]]
		}
		if len(raw) == 0 || json.Unmarshal(raw, &spec) != nil || spec.Pattern == "" {
			spec = server.AnimSpec{Pattern: "off"}
		}
		if r == incubator.RingThinking && v.Speaker != nil {
			spec = speakerSpin(spec, *v.Speaker)
		}
		// Dark between calls: the ring while music plays, if chosen
		// (cmd/musicring.go), else the LED Ring light, if it is on
		// (cmd/ledring.go).
		if r == incubator.RingOff {
			if buttonResuming() {
				spec = resumeWaitSpec()
			} else if music, ok := musicRingSpec(specs); ok {
				spec = music
			} else if idle, ok := ledRingIdle(); ok {
				spec = idle
			}
		}
		// At night dim and warm, a ringing timer as bright as ever.
		if v.Night && r != incubator.RingTimer && spec.Pattern != "off" {
			spec.Colors = nightColors(spec)
		}
	}
	if r == incubator.RingSpeaking || r == incubator.RingAnnouncement || r == incubator.RingTimer {
		spec.TTLSec = 0
	}
	e.srv.StartAnim(spec)
	e.duckFor(r)
}

// Night mode's colours, as the VPE's ring_rgb and ring_brightness have them
// (devices/voice-pe.yaml): amber at a fifth of full.
var (
	nightAmber    = [3]uint8{51, 13, 0} // 0xFF4000 x 0.2
	nightAmberDim = [3]uint8{17, 4, 0}  // a spinner's trail, a third of it
	// At night a command's outcome, the app saying nothing: one slow green
	// pulse when it was done, two quick red blinks when it wasn't (the VPE's
	// "Result").
	nightDoneAnim  = server.AnimSpec{Pattern: "pulse", Colors: [][3]uint8{{0, 255, 64}}, PeriodMs: 1000, TTLMs: 1000}
	nightErrorAnim = server.AnimSpec{Pattern: "pulse", Colors: [][3]uint8{{255, 0, 0}}, PeriodMs: 300, TTLMs: 600}
)

func nightColors(spec server.AnimSpec) [][3]uint8 {
	if spec.Pattern == "spin" {
		return [][3]uint8{nightAmber, nightAmberDim}
	}
	out := make([][3]uint8, max(len(spec.Colors), 1))
	for i := range out {
		out[i] = nightAmber
	}
	return out
}

// timersAnim draws the app's running timers (server's "timers" pattern):
// each in its colour (-1, the ring's own: the scene's thinking colour), at
// the VPE's brightness (a fifth for one, a half for several, so their
// colours tell apart; at night all amber at a fifth), counted down from
// when the app sent them.
func timersAnim(v incubator.RingView, spin json.RawMessage) server.AnimSpec {
	own := [3]uint8{255, 255, 255}
	var s server.AnimSpec
	if json.Unmarshal(spin, &s) == nil && len(s.Colors) > 0 {
		own = s.Colors[0]
	}
	t := v.Timers
	gain := 0.2
	if len(t.Lefts) > 1 {
		gain = 0.5
	}
	var since time.Duration
	if !t.Paused && !t.At.IsZero() {
		since = time.Since(t.At)
	}
	arcs := make([]server.TimerArc, len(t.Lefts))
	for k, left := range t.Lefts {
		c := own
		if k < len(t.Colors) && t.Colors[k] >= 0 {
			x := t.Colors[k]
			c = [3]uint8{uint8(x >> 16), uint8(x >> 8), uint8(x)}
		}
		c = [3]uint8{uint8(float64(c[0]) * gain), uint8(float64(c[1]) * gain), uint8(float64(c[2]) * gain)}
		if v.Night {
			c = nightAmber
		}
		ms := int(left*1000) - int(since.Milliseconds())
		arcs[k] = server.TimerArc{LeftMs: max(ms, 0), Color: c}
	}
	return server.AnimSpec{Pattern: "timers", Timers: arcs, TimerScaleMs: int(t.Scale * 1000), TimersPaused: t.Paused}
}

// unduckGrace is how long music waits, once the satellite goes quiet, before
// it comes back up: a question's call for its answer starts a moment after
// the question has played (the app hears PLAYBACK_DRAINED, then asks), and
// music swelling in that gap and ducking again is what it would sound like.
const unduckGrace = 800 * time.Millisecond

// duckFor ducks music for as long as the satellite is doing something a
// person listens to or speaks into, as a voice turn does under Home
// Assistant (from the wake to the end of the reply): listening, thinking,
// the reply, an announcement, a ringing timer. Dark, or an outcome flash,
// and it comes back up after unduckGrace. Only music already playing is
// ducked when it starts (MusicAudible), as at a wake.
func (e *echoParts) duckFor(r incubator.Ring) {
	active := r.Ducks()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gen++
	if active {
		if !e.ducked && e.spk.MusicAudible(0) {
			e.ducked = true
			duckHold(true)
		}
		return
	}
	if !e.ducked {
		return
	}
	gen := e.gen
	time.AfterFunc(unduckGrace, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.gen == gen && e.ducked {
			e.ducked = false
			duckHold(false)
		}
	})
}

// monitoredAnim is shown while the app listens live: no TTL, cleared by the
// satellite the moment it stops.
var monitoredAnim = server.AnimSpec{Pattern: "pulse", Colors: [][3]uint8{{255, 0, 0}}, PeriodMs: 1000}

// The intercom's rings (internal/incubator/intercom.go): green, quick while
// it rings for an answer, slow once answered.
var intercomAnims = map[incubator.Ring]server.AnimSpec{
	incubator.RingIntercomRinging: {Pattern: "pulse", Colors: [][3]uint8{{0, 255, 64}}, PeriodMs: 600},
	incubator.RingIntercom:        {Pattern: "pulse", Colors: [][3]uint8{{0, 255, 64}}, PeriodMs: 2000},
}

// IntercomChime is one ring of a called intercom, at the Echo's volume, on
// the cue path (no stream: the app's audio is not played while it rings).
func (e *echoParts) IntercomChime() {
	e.spk.PlayCue(cue.IntercomRing(speakerRate, e.spk.CueGain(e.srv.VolumeLevel())))
}

// speakerSpin is the thinking spinner in the identified speaker's color, as
// the app's own satellites show it: their color as the head and the trail at
// the scene's standard ratio (30%), the scene's speed and time-out kept.
func speakerSpin(scene server.AnimSpec, c [3]uint8) server.AnimSpec {
	trail := [3]uint8{uint8(uint16(c[0]) * 3 / 10), uint8(uint16(c[1]) * 3 / 10), uint8(uint16(c[2]) * 3 / 10)}
	spec := server.AnimSpec{Pattern: "spin", Colors: [][3]uint8{c, trail}, PeriodMs: scene.PeriodMs, TTLSec: scene.TTLSec}
	if spec.PeriodMs <= 0 {
		spec.PeriodMs = 80
	}
	if spec.TTLSec <= 0 {
		spec.TTLSec = 135
	}
	return spec
}

// ringSpecKey names each state's spec in incubatorRing.
var ringSpecKey = map[incubator.Ring]string{
	incubator.RingThinking:     "spin",
	incubator.RingSpeaking:     "meter",
	incubator.RingAnnouncement: "meter",
	incubator.RingTimer:        "timer",
	incubator.RingDone:         "done",
	incubator.RingError:        "error",
}

var incubatorEcho *echoParts

func setupIncubator(cc *client.ControlClient, dc *client.DataClient, spk *speaker.PcmSpeaker, srv *server.Server) {
	var dev incubator.Device
	if dc != nil && spk != nil && srv != nil && client.MicUnavailable() == "" {
		incubatorEcho = &echoParts{dc: dc, spk: spk, srv: srv}
		dev = incubatorEcho
	}
	incubatorMgr = &incubator.Manager{
		Firewall: echoFirewall{},
		Send: func(st incubator.State) {
			cc.SendIncubatorState(st)
			// Off this goroutine: it runs under the satellite's lock, and
			// deciding the link ring asks the satellite (Serving).
			go incubatorServingChanged()
		},
		Model:    "echohome-" + board.IDOf(board.Current()),
		Firmware: client.Version,
		Device:   dev,
		Announce: announceSatellite,
	}
	cc.OnIncubatorSatellite(func(m client.IncubatorSatellite) {
		// Always standalone (EchoHome): the satellite is kept on the Echo
		// and outlives the controller, whatever the controller's switch says.
		incubatorMgr.Apply(incubator.Settings{Enabled: m.Enabled, Name: m.Name, MAC: satelliteMAC(m.MAC), Port: m.Port,
			Monitor: reconcileOwnedMonitor(m.Monitor), Standalone: m.Enabled})
		if m.Enabled {
			incubatorSave()
		} else if err := incubatorKeeper.Remove(); err != nil {
			log.Printf("[incubator] removing the saved satellite: %v", err)
		}
	})
	cc.OnIncubatorPress(func() {
		if !incubatorMgr.Press() {
			log.Println("[incubator] press with no satellite running — ignored")
		}
	})
}

// The standalone satellite (incubator.Saved): kept on disk while the
// controller says standalone, started from it at boot, and of use with no
// controller while its app is connected (incubatorServing).
var incubatorKeeper = &incubator.Keeper{Path: incubator.StandalonePath}

// incubatorServing: a standalone satellite has its app connected.
func incubatorServing() bool { return incubatorMgr != nil && incubatorMgr.Serving() }

// incubatorSave writes what a standalone satellite needs, if it changed.
func incubatorSave() {
	if incubatorMgr == nil || incubatorEcho == nil {
		return
	}
	want := incubatorMgr.Want()
	if !want.Standalone {
		return
	}
	snap := config.Get().Snapshot()
	muteRing := config.Get().MuteRingOn()
	saved := incubator.Saved{Name: want.Name, MAC: want.MAC, Port: want.Port, Monitor: want.Monitor,
		ListeningAnim: snap.ListeningAnim, IncubatorRing: snap.IncubatorRing, MuteRing: &muteRing}
	if incubatorEcho.srv.VolumeSeeded() {
		v := incubatorEcho.srv.VolumeLevel()
		saved.Volume = &v
	}
	if err := incubatorKeeper.Save(saved); err != nil {
		log.Printf("[incubator] saving the standalone satellite: %v", err)
	}
}

var (
	saveMu    sync.Mutex
	saveTimer *time.Timer
)

// incubatorSaveSoon saves a moment after the last change (a volume held
// down is many changes; the file is written once).
func incubatorSaveSoon() {
	saveMu.Lock()
	defer saveMu.Unlock()
	if saveTimer != nil {
		saveTimer.Stop()
	}
	saveTimer = time.AfterFunc(2*time.Second, incubatorSave)
}

// startStandaloneIncubator starts the saved satellite at boot, before any
// controller: its ring, mute ring and volume as they were, then the
// satellite. Nothing saved, or anything unreadable, is no standalone.
func startStandaloneIncubator(srv *server.Server) {
	if incubatorMgr == nil || incubatorEcho == nil {
		return
	}
	saved, err := incubator.LoadSaved(incubator.StandalonePath)
	if err != nil {
		return
	}
	config.Get().Apply(config.ConfigMessage{Type: "config", ListeningAnim: saved.ListeningAnim,
		IncubatorRing: saved.IncubatorRing, MuteRing: saved.MuteRing})
	if saved.MuteRing != nil {
		srv.SetMuteRing(*saved.MuteRing)
	}
	if saved.Volume != nil {
		srv.SeedVolume(*saved.Volume)
	}
	log.Printf("[incubator] starting the saved standalone satellite as %s", saved.Name)
	st := saved.Settings()
	st.MAC = satelliteMAC(st.MAC)
	incubatorMgr.Apply(st)
}

// incubatorHoldMs is a hold, not a tap: the controller's BUTTON_HOLD_MS
// (em_esphome.py), test-pinned. A hold does nothing standalone: its Home
// Assistant event has no one to go to.
const incubatorHoldMs = 750

// incubatorLocalPress is the action button with no controller, while a
// standalone satellite serves: a tap is the satellite's press, as the
// controller passes it on (a ringing timer stops, muted or not; a call is
// cancelled; music is paused, or resumed if a press paused it; else a call
// starts, unless muted).
func incubatorLocalPress(heldMs int) {
	if incubatorMgr == nil {
		return // a tap during start-up
	}
	if heldMs >= incubatorHoldMs {
		log.Printf("[incubator] action button held %d ms with no controller — nothing to do", heldMs)
		return
	}
	if !incubatorMgr.Press() {
		log.Println("[incubator] press with no satellite running — ignored")
	}
}

// satelliteMAC is the MAC the satellite announces and says in HELLO: the
// Echo's own (echoMAC, its WiFi chip's), the one its ESPHome device reports,
// so the app links the two. The controller's, from the message or the saved
// copy, only where the Echo has none of its own: the controller's record
// predates the Echo's ESPHome device and names no chip.
func satelliteMAC(pushed string) string {
	if mac := strings.ToLower(echoMAC()); mac != "" && !strings.HasPrefix(mac, "02:ec:") {
		return mac
	}
	return pushed
}

// announceSatellite puts the Echo's own satellite on the network as the app
// browses for it (voice_incubator discover.py): _voice-incubator._tcp, the
// node name as the instance, TXT mac and protocol (PROTOCOL.md). The host
// is the satellite's own name, "<name>-vi", as the controller announced it,
// never the emOS hostname: Echoes share that, and two answering for one name
// would send the app to the wrong Echo. Its addresses are the Echo's IPv4
// ones; emOS admits the app's queries (sport 5353, init.c firewall()).
func announceSatellite(name, mac string, port int) (func(), error) {
	txt := []string{"mac=" + mac, "protocol=" + strconv.Itoa(incubator.Protocol)}
	return announce("incubator", name, "_voice-incubator._tcp", name+"-vi", port,
		func() []string { return txt }, nil), nil
}

// incubatorStreamEnded takes the speaker's end-of-stream report when a
// satellite stream owns the voice plane; false leaves it to the controller.
func incubatorStreamEnded() bool {
	if !incubatorOwnsVoice.Swap(false) {
		return false
	}
	if incubatorMgr != nil {
		incubatorMgr.Drained() // the ring goes dark, and the duck with it
	}
	return true
}

// incubatorIsVoice: this Echo's voice is its own satellite (asked for by
// the controller's incubator_satellite, or started from the saved one).
// The Home Assistant voice path is then off on the Echo: no session opens
// on its own wake word, the controller's mic_start is refused, and the
// action button goes to the satellite here, never through the controller.
// EchoHome's Echo is always so; the controller is only its setup tool.
func incubatorIsVoice() bool { return incubatorMgr != nil && incubatorMgr.Want().Enabled }

// incubatorInCall: a call holds the microphone.
func incubatorInCall() bool { return incubatorMgr != nil && incubatorMgr.InCall() }

// incubatorMute passes the Echo's mute on: muted, before the microphone is
// stopped, which otherwise leaves the satellite's stream running
// (data_call.go); unmuted, after it is started again.
func incubatorMute(muted bool) {
	if incubatorMgr != nil {
		incubatorMgr.SetMuted(muted)
	}
}

// incubatorHearsWake: the app has pushed the satellite wake word models,
// which it listens for instead of the Echo's own wake word.
func incubatorHearsWake() bool { return incubatorMgr != nil && incubatorMgr.WakeModels() > 0 }

// incubatorLinkDown stops the satellite with the controller link.
func incubatorLinkDown() {
	if incubatorMgr != nil {
		incubatorMgr.LinkDown()
	}
}

// watchAppMissing shows that the satellite's app is away: a short dim orange
// flash every appMissingEvery, once it has been away for appMissingAfter
// (an app restarting, or a link the app lost). A flash holds the ring for a
// moment and hands it back, so the music ring, the LED Ring light and the
// satellite's own states carry on under it; never over the mute ring.
const (
	appMissingAfter = 15 * time.Second
	appMissingEvery = 5 * time.Second
	appMissingFlash = 200 * time.Millisecond
)

func watchAppMissing(s *server.Server) {
	var since time.Time
	for range time.Tick(appMissingEvery) {
		if incubatorMgr == nil || !incubatorMgr.Serving() || incubatorMgr.AppConnected() {
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = time.Now()
			continue
		}
		if time.Since(since) >= appMissingAfter && !s.IsMuted() {
			s.Flash(153, 24, 0, appMissingFlash) // the link pulse's orange at its peak
		}
	}
}
