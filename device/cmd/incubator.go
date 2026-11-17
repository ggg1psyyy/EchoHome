package main

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wilbowes/EchoMuse/internal/bindings/speaker"
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/firewall"
	"github.com/wilbowes/EchoMuse/internal/incubator"
	"github.com/wilbowes/EchoMuse/internal/server"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// The Voice Incubator satellite (internal/incubator), run when the
// controller's incubator_satellite message says so. Like the Sendspin player
// it stops when the controller link goes (EchoMuse is one system: the
// controller is also what advertises the Echo on the app's network) and
// starts again on the next message, which the controller sends on connect.
// The decisions are incubator.Manager's, tested there.

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
func (e *echoParts) Muted() bool { return e.srv.IsMuted() }

// ListenStarted lights the listening ring the controller pushed for this
// Echo's own wake (none pushed: the ring is left alone) and ducks music.
func (e *echoParts) ListenStarted() {
	if raw := config.Get().Snapshot().ListeningAnim; len(raw) > 0 {
		var spec server.AnimSpec
		if err := json.Unmarshal(raw, &spec); err == nil && spec.Pattern != "" {
			e.srv.StartAnim(spec)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ducked && e.spk.MusicAudible(0) {
		e.ducked = true
		e.spk.SetDuck(config.Get().DuckDb)
	}
}

// ListenEnded clears the ring once the command has ended; the duck lasts
// until the reply has played.
func (e *echoParts) ListenEnded() {
	e.srv.StartAnim(server.AnimSpec{Pattern: "off"})
	if !incubatorOwnsVoice.Load() {
		e.unduck()
	}
}

func (e *echoParts) unduck() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ducked {
		e.ducked = false
		e.spk.SetDuck(0)
	}
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
		Send:     func(st incubator.State) { cc.SendIncubatorState(st) },
		Model:    "echomuse-" + board.IDOf(board.Current()),
		Firmware: client.Version,
		Device:   dev,
	}
	cc.OnIncubatorSatellite(func(m client.IncubatorSatellite) {
		incubatorMgr.Apply(incubator.Settings{Enabled: m.Enabled, Name: m.Name, MAC: m.MAC, Port: m.Port})
	})
	cc.OnIncubatorPress(func() {
		if !incubatorMgr.Press() {
			log.Println("[incubator] press with no satellite running — ignored")
		}
	})
}

// incubatorStreamEnded takes the speaker's end-of-stream report when a
// satellite stream owns the voice plane; false leaves it to the controller.
func incubatorStreamEnded() bool {
	if !incubatorOwnsVoice.Swap(false) {
		return false
	}
	if incubatorMgr != nil {
		incubatorMgr.Drained()
	}
	if incubatorEcho != nil && !incubatorMgr.InCall() {
		incubatorEcho.unduck()
	}
	return true
}

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
