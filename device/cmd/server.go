package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wilbowes/EchoMuse/internal/aec"
	"github.com/wilbowes/EchoMuse/internal/bindings/als"
	internalbuttons "github.com/wilbowes/EchoMuse/internal/bindings/buttons"
	"github.com/wilbowes/EchoMuse/internal/bindings/jack"
	"github.com/wilbowes/EchoMuse/internal/bindings/mic"
	"github.com/wilbowes/EchoMuse/internal/bindings/mixer"
	"github.com/wilbowes/EchoMuse/internal/bindings/speaker"
	"github.com/wilbowes/EchoMuse/internal/bluetooth"
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/cue"
	"github.com/wilbowes/EchoMuse/internal/platform"
	"github.com/wilbowes/EchoMuse/internal/server"
	"github.com/wilbowes/EchoMuse/internal/wifi"
	"github.com/wilbowes/EchoMuse/pkg/board"
	pkgbuttons "github.com/wilbowes/EchoMuse/pkg/buttons"
	"github.com/wilbowes/EchoMuse/pkg/led"
	pkgmic "github.com/wilbowes/EchoMuse/pkg/mic"
)

const usage = `usage: server [command]

With no command, runs the EchoHome device daemon (normally started by
start_server.sh, which restarts it; do not run a second copy by hand).

  version         print the firmware version and build time
  platform-init   apply the board's platform settings, for emOS's boot
  board           print the board and where each part was found; changes nothing
  help            this text
`

func main() {
	// Only a bare invocation runs the daemon. `server --version` used to
	// start a second instance in the foreground, fighting the supervised one
	// for the mic and the controller link, so anything unrecognised is
	// refused rather than ignored.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "platform-init":
			os.Exit(platformInit())
		case "board":
			// Read-only, so it is safe beside a running server: what a
			// tester on a new board pastes back (#541).
			layout := board.CurrentLayout()
			fmt.Printf("board: %s\n", board.IDOf(layout.Board))
			for _, n := range layout.Notes {
				fmt.Println(n)
			}
			fmt.Printf("mute led gpio: %q\n", layout.MuteLEDGPIO)
			for _, ls := range layout.LightSensors {
				fmt.Printf("light sensor: %q (%s, %s)\n", ls.Driver, ls.Attr, ls.Bus)
			}
			fmt.Printf("bluetooth hci: %q\n", layout.HCI)
			os.Exit(0)
		case "version", "--version", "-v":
			built := "unknown"
			if sec, err := strconv.ParseInt(client.BuildUnix, 10, 64); err == nil {
				built = time.Unix(sec, 0).UTC().Format(time.RFC3339)
			}
			fmt.Printf("EchoHome %s (built %s)\n", client.Version, built)
			os.Exit(0)
		case "help", "--help", "-h":
			fmt.Print(usage)
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}
	log.SetOutput(os.Stdout)
	log.Printf("EchoHome %s starting", client.Version)
	// Before anything dials: emOS has no netd for bionic's resolver.
	platform.UseResolverFor(net.DefaultResolver, platform.Base())

	deviceID := client.GetSerialNo()
	log.Printf("Device ID: %s", deviceID)

	// A WiFi change that never got committed (crash/power cycle mid-switch)
	// is rolled back before anything tries to use the network — same
	// self-healing philosophy as the A/B binary slots.
	wifi.RecoverIfPending()

	// Amazon's WiFi Simple Setup daemon (BLE+WiFi provisioning of
	// neighbouring Amazon devices) is useless on a repurposed device and
	// was caught busy-looping at ~50% CPU / 40% sys on one unit (Office,
	// 2026-07-13 — likely retrying the Bluetooth transport the BLE proxy
	// takes over). Same stock-service takeover as `stop mixer` /
	// `stop acebutton` / `stop ledcontroller` in the hardware bindings.
	// Idempotent: a no-op on boots where init never starts it (Lounge).
	exec.Command("stop", "smarthomewifid").Run()

	// Keep a second CPU core online. procfs, so this has to be re-applied on
	// every start — see applyCoreFloor for why the mic pipeline's 160ms
	// deadline makes it worth doing.
	applyCoreFloor()

	// Which board this is, and where each part the bindings open was found
	// (#541). A part found by its old number instead of by name says so here.
	var boardReport sync.Once
	layout := board.CurrentLayout()
	if layout.Board == nil {
		log.Printf("[board] not identified — using biscuit's layout")
	} else {
		log.Printf("[board] %s", layout.Board.ID)
	}
	for _, n := range layout.Notes {
		log.Printf("[board] %s", n)
	}

	buttonController, err := internalbuttons.NewButtonController()
	if err != nil {
		log.Fatalf("Failed to initialize Button controller: %v", err)
	}

	// A board whose mics this firmware cannot read runs without them as a
	// speaker, rather than exiting into start_server.sh's rollback: no "mic"
	// capability, listen_state degraded with the reason, and the controller
	// starts no turn from the button (em_button.NO_MIC).
	var micIn pkgmic.Microphone
	var micSub pkgmic.Subscribable
	if microphone, err := mic.NewMicrophone(); err != nil {
		log.Printf("[mic] %v — running without microphones", err)
		client.SetMicUnavailable("no microphone this firmware can read — speaker only")
	} else {
		micIn, micSub = microphone, microphone
	}

	// AEC canceller — far end fed by the speaker's echo tap, near end run
	// by the data client on the mono mic stream. Starts disabled; armed by
	// applyAecConfig from env defaults below and on every config push.
	canceller := aec.New(board.CurrentLayout().Mics.NumMics())

	// The level tap drives the energy-reactive LED ring ("meter" pattern).
	// The Server doesn't exist yet when the speaker starts its pump loop,
	// so the tap goes through an atomic pointer armed just below.
	var srvPtr atomic.Pointer[server.Server]
	pcmSpeaker, err := speaker.NewPcmSpeaker(canceller.WriteFar, func(rms float64) {
		if srv := srvPtr.Load(); srv != nil {
			srv.SetAudioLevel(rms)
		}
	})
	if err != nil {
		log.Fatalf("Failed to initialize PCM Speaker: %v", err)
	}

	s := server.NewServer(buttonController, micIn, pcmSpeaker)
	s.SetMusicLevel(pcmSpeaker.MusicLevel) // the ring while music plays
	srvPtr.Store(s)

	// Local duck at the device's own wake crossing, confirmed or released by
	// the controller's duck message (OnDuck below).
	if benchSpeaker != nil {
		benchSpeaker(pcmSpeaker)
	}

	buttonController.SetVolumeCallback(func(direction string) {
		// Inert without a controller: nothing is playing to be louder or
		// quieter, and showing the arc would acknowledge a device that
		// cannot act. The mute button stays live — see Server.SetLinkDown.
		if s.LinkDown() {
			log.Println("[cmd] volume button ignored — no controller session")
			return
		}
		changed := false
		if direction == "up" {
			changed = s.VolumeStepUp()
		} else {
			changed = s.VolumeStepDown()
		}
		// The cue is for the person pressing the physical button. Remote
		// volume changes stay silent, and active voice/music already provides
		// the audible reference this setting exists to supply while idle.
		const playbackTail = 100 * time.Millisecond
		if cue.VolumeButtonPreviewDue(direction, changed, s.VolumeAtMax()) &&
			config.Get().VolumeButtonSoundEnabled() &&
			!pcmSpeaker.VoiceAudible(playbackTail) &&
			!pcmSpeaker.MusicAudible(playbackTail) {
			playVolumeCue(pcmSpeaker, s.VolumeLevel())
		}
	})
	buttonController.SetMuteCallback(func() {
		s.MuteToggle()
	})

	ctx := context.Background()

	dataClient := client.NewDataClient(deviceID, micSub, pcmSpeaker, canceller)
	canceller.SetStatePath(aec.DefaultStatePath) // saved echo path: loaded on the hardware reference
	aec.RemoveLegacyState()
	applyAecConfig(canceller, dataClient) // arm from env defaults before any config push

	controlClient := client.NewControlClient(
		deviceID,
		func(leds []led.Led, listening *bool) {
			// A raw frame from the controller supersedes any running
			// device-local animation — stop it so its next tick can't
			// paint over this frame.
			s.StopAnim()
			s.SetLEDs(leds, listening)
		},
	)

	// Device-rendered ring animations (led_anim) — the animation engine
	// runs on the device's own ticker, immune to controller/WiFi jitter.
	controlClient.OnLEDAnim(func(raw json.RawMessage) {
		var spec server.AnimSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			log.Printf("[cmd] bad led_anim spec: %v", err)
			return
		}
		// While the Echo's own satellite runs, idle is its to paint: the
		// controller's "off" (sent on every connect, em_controller after the
		// config push) is idle, which is the LED Ring light when it is on.
		// Bench, 00LQ, 2026-11-18: that "off" left the ring dark after
		// every restart.
		if spec.Pattern == "off" && incubatorMgr != nil && incubatorMgr.Running() {
			s.StopAnim()
			incubatorMgr.Repaint()
			return
		}
		s.StartAnim(spec)
	})

	// BLE proxy scanner: passive scan over /dev/stpbt, each batch to the
	// Echo's own ESPHome device, Home Assistant's Bluetooth proxy
	// (cmd/bleproxy.go). Toggled by Home Assistant's switch there
	// (bleProxyEnabled, applyBleConfig). The controller no longer takes
	// adverts or bridges connections (EchoHome).
	bleScanner := bluetooth.NewScanner(esphomeAdverts)
	applyBleConfig(bleScanner)
	bleRefs.scanner = bleScanner

	// The BLE scan costs this device's WiFi dearly (see Scanner.Yield), so it
	// stops whenever the link carries something that cannot wait: a Voice
	// Incubator call (the user's words going up), a reply coming down, or a
	// shell session (the console, OTA and asset pushes).
	// Polled rather than signalled at each edge: the answer is recomputed from
	// live state every tick, so no missed "done" can leave the proxy silent.
	// Music gets bursts rather than a yield (bluetooth.MusicDuty).
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		duty := bluetooth.NewMusicDuty()
		bleScanner.SetMusicDuty(duty)
		for now := range t.C {
			// Synced music (Sendspin) is music too: it streams for hours
			// and has its own buffer, so it gets the same bursts.
			ssOn, ssLead := sendspinMusic()
			music := duty.Yield(now, pcmSpeaker.MusicArriving() || ssOn,
				max(pcmSpeaker.MusicLead(), ssLead))
			sendspinPoll(pcmSpeaker)
			bleScanner.Yield(music ||
				incubatorInCall() ||
				pcmSpeaker.VoiceArriving() ||
				controlClient.ShellActive())
		}
	}()

	// A 7 s hold of the action button asks to pair, unless the Echo is on
	// the encrypted link (client/pairing.go, #841). The white flash says the
	// hold registered: on a connected device nothing else changes on the
	// ring until an admin approves.
	pairHold := client.NewPairHold(controlClient.PairingNeeded, func() {
		s.Flash(150, 150, 150, 400*time.Millisecond)
		controlClient.StartPairing()
	})

	// Button events — forward to controller via control plane. dispatchButton
	// is the usual path; with multi-tap events on, the action button's
	// events are counted first and come back through it (cmd/multitap.go).
	var dispatchButton func(event pkgbuttons.ButtonClickEvent)
	setMultiTapDispatch(func(e pkgbuttons.ButtonClickEvent) { dispatchButton(e) })
	_, err = buttonController.SubscribeToButton(func(event pkgbuttons.ButtonClickEvent) {
		log.Printf("Button event: clickType=%d down=%v", event.ClickType, event.Down)
		// Ahead of the link-down gate: a device that cannot connect is the one
		// that most needs to ask. The release ending a pairing hold is not
		// forwarded, so it does not also reach HA as a long press.
		if event.ClickType == pkgbuttons.DotClick && pairHold.Event(event.Down) {
			log.Println("[cmd] action button release ended a pairing hold — not forwarded")
			return
		}
		if multiTapTake(event) {
			return
		}
		dispatchButton(event)
	})
	dispatchButton = func(event pkgbuttons.ButtonClickEvent) {
		// The Echo's own ESPHome server (when on) has the hold, with or
		// without a controller.
		if event.ClickType == pkgbuttons.DotClick && !event.Down {
			esphomeButton(int(event.HeldMs))
		}
		// Inert without a controller session: the dot cannot start a turn
		// with nothing to send it to, and the ring flash CancelVolumeDisplay
		// produces would acknowledge a press that achieves nothing. Dropped
		// here rather than at the binding — the binding is the portable
		// hardware layer and knows nothing about sessions.
		if s.LinkDown() {
			log.Println("[cmd] action button ignored — no controller session")
			return
		}
		// The satellite takes the tap, with or without a controller (never
		// through it: the controller has no voice since EchoHome). A hold
		// is Home Assistant's event above.
		if event.ClickType == pkgbuttons.DotClick && !event.Down {
			s.CancelVolumeDisplay() // a press outranks the volume arc
			incubatorLocalPress(int(event.HeldMs))
		}
	}
	if err != nil {
		log.Fatalf("Button subscription failed: %v", err)
	}

	// Disconnected — orange pulse
	//
	// pulseKind is what the ring is CURRENTLY showing, and restarting a pulse
	// that is already running is the bug it exists to prevent. OnDisconnected
	// fires once per reconnect-loop iteration, not once per disconnection —
	// so every retry used to cancel the goroutine mid-cycle and start a new
	// one from phase zero, which is mid-brightness and rising. The ring ran
	// roughly two smooth cycles and then hard-cut back to the middle, at an
	// interval that is not a multiple of the pulse period, so the jump landed
	// somewhere different each time. Reported as "like a poorly repeating
	// gif" and it is exactly that: a loop being restarted, not a loop.
	var (
		pulseCancel context.CancelFunc
		pulseKind   string
	)
	// A standalone satellite with its app connected (cmd/incubator.go) is an
	// Echo of use with no controller: while it serves, the link rings stand
	// aside and the buttons are not gated (the action button then goes to
	// the satellite, below); they come back the moment it stops serving.
	// linkMu orders the link callbacks and the satellite's changes.
	var (
		linkMu   sync.Mutex
		lostKind string // the link-lost ring to show, "" while connected
		lostRun  func(context.Context, *server.Server)
	)
	showLinkLost := func() { // linkMu held
		if controllerUp.Load() || lostKind == "" {
			return
		}
		if incubatorServing() {
			if pulseCancel != nil {
				pulseCancel()
				pulseCancel = nil
				pulseKind = ""
				s.SetLinkDown(false)
				if s.IsMuted() {
					s.RestoreMuteRing() // the pulse painted over it
				}
				incubatorMgr.Repaint() // over the pulse's last frame
			}
			s.SetLinkDown(false)
			return
		}
		if pulseKind == lostKind {
			return
		}
		if pulseCancel != nil {
			pulseCancel()
		}
		pulseCtx, cancel := context.WithCancel(ctx)
		pulseCancel = cancel
		pulseKind = lostKind
		// Set BEFORE the pulse starts, or its first frames are swallowed by
		// the mute suppression on a muted device.
		s.SetLinkDown(true)
		go lostRun(pulseCtx, s)
	}
	incubatorServingChanged = func() {
		linkMu.Lock()
		showLinkLost()
		linkMu.Unlock()
	}
	// Watch the ambient light sensor for step changes (a lamp switching on)
	// and report them immediately; the steady-state value rides the ~30s
	// stats tick. No-ops on a device without the sensor.
	//
	// Started here rather than earlier because it captures controlClient,
	// which does not exist until above — and the amd64 build cannot catch
	// that, since this package is excluded from it by build constraints
	// (mic/speaker are ARM-only). Only compile.sh compiles this file.
	go als.Watch(ctx, func(lux int) {
		controlClient.SendAmbientLight(lux)
	})

	// Headphone jack. BOTH directions need work from us, and so does the
	// state the device booted into — accdet acts only on a transition, and
	// Init leaves the internal amp on regardless of what is plugged in.
	// SetJackRouting owns the whole mapping (issue #80 for the removal half,
	// measured against a stock Dot 2026-09-03 for the rest).
	go jack.Watch(ctx, func(inserted bool) {
		pcmSpeaker.SetJackRouting(inserted)
	})
	// Android's audio HAL rewrites the codec on every mediaserver restart —
	// roughly once a minute with a plug inserted — so applying the routing on
	// the jack edge alone holds for about a minute and then the jack goes
	// quiet again. Measured 2026-09-03. Nothing can stop mediaserver (the
	// framework crash-loops without it), so the routing is reconciled instead.
	go pcmSpeaker.WatchJackRouting(ctx)

	controlClient.OnDisconnected(func() {
		linkMu.Lock()
		defer linkMu.Unlock()
		controllerUp.Store(false)
		if lostKind == "orange" {
			return // already handled: restarting the pulse is what breaks its cycle
		}
		// Stop any device-local animation: the controller that owned it is
		// gone, and the pulse would otherwise fight its ticker; but not a
		// standalone satellite's, which carries on. StopAnim only bumps the
		// animator generation; the pulse paints through SetLEDs.
		if !incubatorServing() {
			s.StopAnim()
		}
		// The ring belongs to the link state from here, and the action and
		// volume buttons go inert, unless a standalone satellite serves.
		incubatorLinkDown()
		lostKind, lostRun = "orange", pulseOrange
		showLinkLost()
	})

	// Pending approval — slow white pulse
	controlClient.OnPending(func() {
		linkMu.Lock()
		defer linkMu.Unlock()
		controllerUp.Store(false)
		if lostKind == "white" {
			return
		}
		// Pending approval is the same condition one step earlier: there is
		// nothing above this device, so the white pulse owns the ring and the
		// buttons do nothing (a standalone satellite serving aside).
		incubatorLinkDown()
		lostKind, lostRun = "white", pulseWhite
		showLinkLost()
	})

	// Refused — a controller answered and would not accept this device's
	// credentials. Orange like disconnected, since it is a link problem, but
	// alternating odd and even LEDs, so it reads differently: this one the
	// owner can fix, by holding the action button 7 s to pair.
	controlClient.OnRefused(func() {
		linkMu.Lock()
		defer linkMu.Unlock()
		controllerUp.Store(false)
		if !incubatorServing() {
			s.StopAnim()
		}
		if lostKind == "refused" {
			return
		}
		incubatorLinkDown()
		lostKind, lostRun = "refused", pulseRefused
		showLinkLost()
	})

	// Connected — stop pulse, report current mute state, restore ring or hand
	// back to direction arc depending on mute state.
	controlClient.OnConnected(func() {
		// A part opened by its old number, or not found at all, is reported
		// once per process: it is how a kernel that names something
		// differently is learned about from the field (#541).
		boardReport.Do(func() {
			if layout.Board == nil {
				controlClient.SendLog("warn", "[board] not identified — using biscuit's layout")
			}
			for _, p := range layout.Problems {
				controlClient.SendLog("warn", "[board] "+p)
			}
			for _, p := range pcmSpeaker.Problems() {
				controlClient.SendLog("warn", "[board] "+p)
			}
		})
		linkMu.Lock()
		controllerUp.Store(true)
		lostKind = ""
		if pulseCancel != nil {
			pulseCancel()
			pulseCancel = nil
		}
		pulseKind = ""
		linkMu.Unlock()
		// Session restored: the ring goes back to the controller, the mute
		// ring reasserts below if it applies, and the buttons work again.
		s.SetLinkDown(false)
		// Always report mute state on (re)connect — the controller may have
		// restarted and lost its record of our state. Volume is only
		// reported once the device holds an authoritative level (seeded
		// from config or set locally): the controller persists every
		// volume_state into startupVolume, so reporting the boot-default
		// level here is what used to clobber the saved volume on reboot.
		// On a fresh boot the config push seeds the volume, and Set()'s
		// change callback sends the report instead.
		muted := s.IsMuted()
		controlClient.SendMuteState(muted)
		// What the microphone does, for the dashboard's privacy line: a
		// restarted controller has no record of it.
		reportListenState(controlClient)
		esphomeReportMDNS() // the controller announces it too, across VLANs
		if s.VolumeSeeded() {
			controlClient.SendVolumeState(s.VolumeLevel())
		}
		s.StopAnim() // fresh controller session owns the ring from here
		if muted {
			// Orange pulse overwrote the red ring — restore it.
			s.RestoreMuteRing()
		} else {
			s.SetLEDs(allLEDs(0, 0, 0), nil)
			s.LEDModeDirection()
		}
		// A satellite keeps its state: its ring (a spinner, the timers, the
		// red monitoring pulse, or idle, which is the LED Ring light when it
		// is on) is painted again AFTER the clear above, or the clear wipes
		// it (bench, 00LQ, 2026-11-18: dark after every restart).
		if incubatorMgr != nil {
			incubatorMgr.Repaint()
		}
		// Send an immediate stats snapshot so the dashboard populates on
		// (re)connect rather than waiting up to 30s for the first tick.
		go func() {
			st := collectStats()
			st.Ble = bleScanner.Stats()
			st.AecRef = canceller.RefSource()
			st.Sendspin = sendspinStatus()
			controlClient.SendStats(st)
		}()
		// Deliver any unacknowledged WiFi change outcome (including the
		// "restarted before commit" result RecoverIfPending leaves
		// behind). Not cleared here — the controller's wifi_commit ack
		// does that (wifi.Commit), so a result lost in transit re-sends.
		if r := wifi.PendingResult(); r != nil {
			controlClient.SendWifiResult(r.OK, r.SSID, r.Error)
		}
	})

	// Config applied — apply hardware changes to the mixer, AEC params to
	// the canceller. AEC/BLE read the merged post-Apply snapshot rather than
	// the (partial) message so unmentioned fields keep their values.
	// applyConfig is a config push's effects: from the controller, or at boot
	// from the one kept (config.LastPath) when no controller is there yet.
	applyConfig := func(msg config.ConfigMessage) {
		reconcileOwnedConfig() // first: the Echo's own settings stand over the push
		applyHardwareConfig(msg)
		s.SetRemoteVolumeArc(config.Get().RemoteVolumeArcEnabled())
		incubatorSaveSoon() // the ring's scene, for a standalone satellite
		s.SetMuteRing(config.Get().MuteRingOn())
		pcmSpeaker.SetResponseGainDB(config.Get().ResponseGainDB())
		// The merged config, not the partial message, for the reason given
		// above. Active is re-read from the ack on every push: a reconnect
		// can land on a controller that does not hand the chain over.
		pcmSpeaker.SetOutputChain(config.Get().OutputChain())
		// startupVolume is the controller's persisted record of this
		// device's volume (updated on every volume_state report) — restore
		// it through the Server, not a raw tinymix write: SeedVolume keeps
		// the recorded level in sync and only honours the first push per
		// run, so a reconnect's config can't stomp a live volume change.
		if msg.StartupVolume > 0 {
			s.SeedVolume(msg.StartupVolume)
		}
		applyAecConfig(canceller, dataClient)
		applyBleConfig(bleScanner)
		esphomeBLEFollow()
		esphomeFollowName()
		applySendspinConfig(pcmSpeaker, controlClient, s, deviceID)
		applyDHCPHostname()
	}
	controlClient.OnConfigApplied(func(msg config.ConfigMessage) {
		sendspinPushed(msg.SendspinEnabled) // the dashboard's switch, before the Echo's settings go back over the push
		applyConfig(msg)
	})

	// Identify (#860): the dashboard asks which Echo this is, and the device
	// takes the whole ring for its own fixed time.
	controlClient.OnIdentify(s.Identify)

	controlClient.OnSendspinToken(sendspinToken)
	controlClient.OnESPHomeKey(esphomeKeyReply)

	// The Voice Incubator satellite, when the controller asks for it.
	setupDuck(pcmSpeaker)
	setupIncubator(controlClient, dataClient, pcmSpeaker, s)
	setupESPHome(controlClient, s, pcmSpeaker)

	// Speaker flush — barge-in: cut buffered TTS the moment the controller
	// hears the wake word during playback.
	controlClient.OnSpeakerFlush(func() {
		pcmSpeaker.Flush()
	})

	// Music flush — the user genuinely stopped or paused. A voice turn ducks
	// instead and must never send this.
	controlClient.OnMusicFlush(func() {
		pcmSpeaker.FlushMusic()
	})

	// A stream's end is the media player's or the satellite's (its ring goes
	// dark, and its duck with it). Nothing goes to the controller, which no
	// longer runs turns (EchoHome).
	pcmSpeaker.OnStreamStats(func(speaker.StreamStats) {
		if !mediaStreamEnded() {
			incubatorStreamEnded()
		}
	})

	// WiFi change — the executor owns the whole switch/rollback sequence
	// (internal/wifi); the reconnect gate polls IsConnected. The outcome
	// is sent as wifi_result with at-least-once delivery: retried on a
	// ticker (and by the OnConnected drain above) until the controller's
	// wifi_commit ack clears it. IsConnected can report true against a
	// half-open TCP connection the interface bounce killed, so a single
	// send is not enough — the very first hardware success vanished that
	// way while the WS looked connected the whole time.
	controlClient.OnWifiChange(func(ssid []byte, psk string) {
		go func() {
			wifi.Change(ssid, psk, controlClient.IsConnected)
			for i := 0; i < 30; i++ { // ~5 min, then give up (dashboard TTL is 4)
				r := wifi.PendingResult()
				if r == nil {
					return
				}
				if controlClient.IsConnected() {
					controlClient.SendWifiResult(r.OK, r.SSID, r.Error)
				}
				time.Sleep(10 * time.Second)
			}
		}()
	})
	controlClient.OnWifiCommit(wifi.Commit)
	controlClient.OnWifiScan(func() {
		go func() {
			nets, err := wifi.Scan()
			if err != nil {
				controlClient.SendWifiScanResult(nil, err.Error())
				return
			}
			controlClient.SendWifiScanResult(nets, "")
		}()
	})

	// Mute state change: reported to the controller and to Home Assistant,
	// and the satellite's microphone closed (incubatorMute). Mute is the
	// Echo's own; nothing remote can lift it.
	s.SetMuteChangeCallback(func(muted bool) {
		controlClient.SendMuteState(muted)
		esphomeMuted(muted)
		// The satellite closes its microphone at mute, and opens it again
		// after (data_call.go): no other stream runs.
		incubatorMute(muted)
	})

	// Volume is applied in software by the speaker; the DAC stays at unity.
	// The hardware echo reference is the bytes written to ALSA, so it is
	// post-volume by construction and the canceller needs no scalar.
	if pcmSpeaker != nil {
		s.SetVolumeApply(pcmSpeaker.SetVolume)
	}
	// Volume change — notify controller so HA entity and dashboard reflect it.
	// Fires on every Set() call: physical button press or future volume_set command.
	s.SetVolumeChangeCallback(func(level int) {
		controlClient.SendVolumeState(level)
		sendspinVolumeChanged(level)
		mediaVolumeChanged(level)
		keepVolumeSoon(level) // the kept config's startupVolume
		incubatorSaveSoon()   // a standalone Echo keeps its own volume
	})

	// Volume set from controller (HA MediaPlayerCommandRequest forwarded down).
	// Calls Set() which applies the volume, updates LEDs, and fires the change
	// callback above — so SendVolumeState fires automatically, closing the loop.
	controlClient.OnVolumeSet(func(level int) {
		s.SetVolume(level)
	})

	// A standalone satellite starts from what it saved, before any
	// controller is found: here, once the volume reaches the speaker
	// (SetVolumeApply above), or its saved level would be silence.
	// The config last given, before any controller: an Echo that runs on its
	// own boots as it was set up, not with the firmware's defaults. With no
	// controller yet, the Echo is what plays the audio, so its own output
	// chain runs until a controller's ack says otherwise.
	sendspinSetup(pcmSpeaker, controlClient, s, deviceID)
	if last, ok := config.LoadLast(config.LastPath); ok {
		config.Get().Apply(last)
		applyConfig(last)
		pcmSpeaker.SetOutputChainActive(true)
		log.Printf("[config] the kept config applied")
	}
	startStandaloneIncubator(s)
	applyOwnedAtBoot()
	go watchMusicRing()

	// Heap-profile dump on SIGUSR1 — the ~1MB/h leak hunt (2026-07-17).
	// The device accepts no inbound connections, so instead of an HTTP pprof
	// endpoint the profile is written to /tmp and pulled over the shell
	// proxy: `kill -USR1 $(pidof server)` then base64 the file out. A GC
	// runs first so the profile reflects live objects, not garbage awaiting
	// collection. Fixed filenames (2 slots, alternating) so repeated dumps
	// for before/after diffing can't fill /tmp.
	usrCh := make(chan os.Signal, 1)
	signal.Notify(usrCh, syscall.SIGUSR1)
	go func() {
		slot := 0
		for range usrCh {
			runtime.GC()
			path := fmt.Sprintf("/tmp/heap-%d.pprof", slot)
			f, err := os.Create(path)
			if err != nil {
				log.Printf("[pprof] create %s: %v", path, err)
				continue
			}
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Printf("[pprof] write %s: %v", path, err)
			} else {
				log.Printf("[pprof] heap profile written to %s", path)
			}
			f.Close()
			slot = 1 - slot
		}
	}()

	log.Println("Ready")
	time.Sleep(2 * time.Second)

	go func() {
		if err := controlClient.Run(ctx, dataClient); err != nil && err != context.Canceled {
			log.Printf("Control client stopped: %v", err)
		}
	}()

	// Periodic stats reporter — every 30s. SendStats silently drops when
	// the device is not connected, so this goroutine runs unconditionally.
	// Every 10th tick (~5min) a [mem] line goes to the local log: the
	// process RSS is growing ~1.2MB/h (measured 2026-07-16) and the Go
	// runtime's own accounting is what distinguishes heap growth (leak —
	// HeapAlloc climbs), fragmentation/retained-but-free memory (HeapAlloc
	// flat, HeapSys/RSS climb), and goroutine leaks (goroutines climb).
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		tick := 0
		// Drained only here, on this one goroutine, so LinkLoss needs no lock;
		// the on-connect snapshot above simply leaves the fields unset.
		var upLoss client.LinkLoss
		for range ticker.C {
			st := collectStats()
			st.Ble = bleScanner.Stats()
			st.AecRef = canceller.RefSource()
			var snaps []client.TCPSnap
			if sn, ok := controlClient.TCPSnapshot(); ok {
				snaps = append(snaps, sn)
			}
			if sn, ok := dataClient.TCPSnapshot(); ok {
				snaps = append(snaps, sn)
			}
			st.TcpUpRetrans, st.TcpUpSegs = upLoss.Drain(snaps...)
			st.Sendspin = sendspinStatus()
			controlClient.SendStats(st)
			if tick%10 == 0 {
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				memLine := fmt.Sprintf("[mem] goroutines=%d heap_alloc=%dKB heap_sys=%dKB heap_idle=%dKB released=%dKB stack=%dKB rss=%dKB num_gc=%d pause_total=%dms",
					runtime.NumGoroutine(),
					ms.HeapAlloc/1024, ms.HeapSys/1024, ms.HeapIdle/1024,
					ms.HeapReleased/1024, ms.StackSys/1024, selfRSSKb(),
					ms.NumGC, ms.PauseTotalNs/1e6)
				log.Print(memLine)
				// Forward to the controller's device_logs too — the local
				// /tmp/server.log is RAM-backed and dies with every reboot,
				// and the 2026-07 leak hunt needed these lines pulled over
				// the shell proxy by hand. One message per ~5min; SendLog
				// silently drops while disconnected, same as SendStats.
				controlClient.SendLog("info", memLine)
			}
			tick++
		}
	}()

	// Graceful shutdown on SIGTERM/SIGINT — both the OTA restart
	// (`kill $PPID` from the deploy shell) and start_server.sh's trap send
	// SIGTERM, so this runs on every normal stop. The speaker Close mutes
	// and disables the amp before the PCM stream tears down: without it,
	// every stop/restart/OTA clicked (amp cut mid-stream) and the amp was
	// left driving an idle DAC while the server was down (audible hiss
	// between OTA slots). Nothing else needs orderly teardown — mic/LED/
	// WS state all reset cleanly on the next start.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Printf("Received %v — shutting down (muting output, amp off)", sig)
	bleScanner.SetEnabled(false) // scan off + /dev/stpbt closed so the chip idles
	// "restart", not "shutdown": this is an OTA or a supervisor restart far
	// more often than a power-off, and restart asks the server to redial.
	if c := sendspinPlayer(); c != nil {
		c.Stop("restart")
	}
	pcmSpeaker.Close()
	os.Exit(0)
}

// ─── Hardware stats collection ────────────────────────────────────────────────

// emmcForStats is the eMMC wear for the stats tick, read at most every six
// hours. An untyped nil when unreadable, so the field is null rather than a
// typed nil the controller would have to tell apart.
func emmcForStats() interface{} {
	if e := platform.EmmcCached(6 * time.Hour); e != nil {
		return e
	}
	return nil
}

func collectStats() client.DeviceStats {
	cpuPct := cpuPercent()
	memUsed, memTotal := memStats()
	stoUsed, stoTotal := storageStats()
	rssi := wifiRSSI()
	tx, rx, txErr, txDrop, rxCrc := netDeltas()
	speed, freq, bssid := linkInfo()
	cpuC, maxC, coreLimit := thermals()
	return client.DeviceStats{
		AmbientLux:       als.Lux(),
		CPUTempC:         cpuC,
		MaxTempC:         maxC,
		CoresOnline:      coresOnline(),
		CoresTotal:       coresTotal(),
		ThermalCoreLimit: coreLimit,
		CPUPct:           cpuPct,
		MemUsedMb:        memUsed,
		MemTotalMb:       memTotal,
		StorageUsedMb:    stoUsed,
		StorageTotalMb:   stoTotal,
		WifiRssi:         rssi,
		WifiSsid:         wifi.CurrentSSID(),
		LinkSpeedMbps:    speed,
		WifiFreqMhz:      freq,
		WifiBssid:        bssid,
		TxBytes:          tx,
		RxBytes:          rx,
		TxErrors:         txErr,
		TxDropped:        txDrop,
		RxCrcErrors:      rxCrc,
		Emmc:             emmcForStats(),
	}
}

// ─── Network telemetry ────────────────────────────────────────────────────────

// netCounters holds the previous sysfs read so stats can be reported as
// per-interval deltas. Only collectStats touches it (single stats goroutine).
var netCounters struct {
	tx, rx, txErr, txDrop, rxCrc uint64
	primed                       bool
}

// netDeltas returns tx/rx bytes and error counts accumulated since the
// previous call, read from /sys/class/net/wlan0/statistics/. Plain file
// reads — no process spawn — so this is cheap enough for every stats tick.
// The first call primes the baseline and reports zeros.
func netDeltas() (tx, rx, txErr, txDrop, rxCrc uint64) {
	read := func(name string) uint64 {
		b, err := os.ReadFile("/sys/class/net/wlan0/statistics/" + name)
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return v
	}
	ctx, crx := read("tx_bytes"), read("rx_bytes")
	cErr, cDrop, cCrc := read("tx_errors"), read("tx_dropped"), read("rx_crc_errors")

	// delta guards against counter resets (interface bounce) by clamping
	// a negative difference to 0 rather than reporting a huge number.
	delta := func(cur, prev uint64) uint64 {
		if cur < prev {
			return 0
		}
		return cur - prev
	}
	if netCounters.primed {
		tx = delta(ctx, netCounters.tx)
		rx = delta(crx, netCounters.rx)
		txErr = delta(cErr, netCounters.txErr)
		txDrop = delta(cDrop, netCounters.txDrop)
		rxCrc = delta(cCrc, netCounters.rxCrc)
	}
	netCounters.tx, netCounters.rx = ctx, crx
	netCounters.txErr, netCounters.txDrop, netCounters.rxCrc = cErr, cDrop, cCrc
	netCounters.primed = true
	return
}

// linkInfoCache holds the last wpa_cli result and when it was taken.
var linkInfoCache struct {
	speed, freq int
	bssid       string
	at          time.Time
}

// linkInfoInterval — how often the wpa_cli subprocess is actually run.
// Unlike everything else in collectStats this costs a process spawn, and
// PHY rate / band / AP change on the scale of minutes, not seconds. Cached
// values are reused between refreshes so every stats message still carries
// the fields.
const linkInfoInterval = 2 * time.Minute

// linkInfo returns negotiated PHY rate (Mbps), frequency (MHz) and BSSID.
//
// Requires the -p control-socket path: plain `wpa_cli -i wlan0` answers
// UNKNOWN COMMAND on FireOS because the default socket dir doesn't exist.
// Returns zero values if wpa_supplicant isn't reachable — the fields are
// omitempty, so the controller sees them absent rather than wrong.
func linkInfo() (speed, freq int, bssid string) {
	if time.Since(linkInfoCache.at) < linkInfoInterval {
		return linkInfoCache.speed, linkInfoCache.freq, linkInfoCache.bssid
	}
	linkInfoCache.at = time.Now()

	out, err := exec.Command("wpa_cli", "-p", "/data/misc/wifi/sockets",
		"-i", "wlan0", "signal_poll").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			n, convErr := strconv.Atoi(v)
			if convErr != nil {
				continue
			}
			switch k {
			case "LINKSPEED":
				linkInfoCache.speed = n
			case "FREQUENCY":
				linkInfoCache.freq = n
			}
		}
	}
	if out, err := exec.Command("wpa_cli", "-p", "/data/misc/wifi/sockets",
		"-i", "wlan0", "status").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "bssid="); ok {
				linkInfoCache.bssid = v
				break
			}
		}
	}
	return linkInfoCache.speed, linkInfoCache.freq, linkInfoCache.bssid
}

// cpuPercent samples /proc/stat twice over 500ms and returns utilisation %.
func cpuPercent() float64 {
	type snap struct{ total, idle uint64 }

	read := func() (snap, bool) {
		f, err := os.Open("/proc/stat")
		if err != nil {
			return snap{}, false
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			fields := strings.Fields(line)[1:] // skip "cpu"
			var vals [8]uint64
			for i := 0; i < len(fields) && i < 8; i++ {
				vals[i], _ = strconv.ParseUint(fields[i], 10, 64)
			}
			// user nice system idle iowait irq softirq steal
			idle := vals[3] + vals[4] // idle + iowait
			total := vals[0] + vals[1] + vals[2] + vals[3] +
				vals[4] + vals[5] + vals[6] + vals[7]
			return snap{total, idle}, true
		}
		return snap{}, false
	}

	s1, ok1 := read()
	time.Sleep(500 * time.Millisecond)
	s2, ok2 := read()
	if !ok1 || !ok2 {
		return 0
	}
	dTotal := float64(s2.total - s1.total)
	if dTotal <= 0 {
		return 0
	}
	dIdle := float64(s2.idle - s1.idle)
	pct := (1 - dIdle/dTotal) * 100
	// Round to one decimal place
	return math.Round(pct*10) / 10
}

// memStats reads /proc/meminfo and returns (used MB, total MB).
func memStats() (usedMb, totalMb int) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var totalKb, availKb uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKb = val
		case "MemAvailable:":
			availKb = val
		}
	}
	if totalKb == 0 {
		return 0, 0
	}
	usedKb := totalKb - availKb
	return int(usedKb / 1024), int(totalKb / 1024)
}

// storageStats returns (used MB, total MB) for /data via statfs.
func storageStats() (usedMb, totalMb int) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/data", &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	free := st.Bfree * bsize
	used := total - free
	const mb = 1024 * 1024
	return int(used / mb), int(total / mb)
}

// selfRSSKb reads the process's resident set size from /proc/self/status —
// the OS's ground truth, against which the Go runtime numbers in the [mem]
// log line are compared. 0 if unreadable.
func selfRSSKb() int {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			kb, _ := strconv.Atoi(fields[1])
			return kb
		}
	}
	return 0
}

// wifiRSSI reads /proc/net/wireless and returns the signal level in dBm,
// or nil if the interface is not available.
//
// Some kernels encode the level field as a positive offset (0–255) rather
// than signed dBm; values > 0 are adjusted by subtracting 256 to recover
// the actual dBm reading (e.g. 206 → -50 dBm).
func wifiRSSI() *int {
	f, err := os.Open("/proc/net/wireless")
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // skip two header lines
		}
		fields := strings.Fields(sc.Text())
		// fields: [iface status link level noise ...]
		if len(fields) < 4 {
			continue
		}
		// level is fields[3], may have a trailing "."
		rssiStr := strings.TrimRight(fields[3], ".")
		rssi, err := strconv.Atoi(rssiStr)
		if err != nil {
			continue
		}
		// Correct offset encoding used by some kernels
		if rssi > 0 {
			rssi -= 256
		}
		// Sanity check — valid RSSI is roughly -30 to -100 dBm
		if rssi < -120 || rssi > 0 {
			continue
		}
		return &rssi
	}
	return nil
}

// ─── Hardware config ──────────────────────────────────────────────────────────

// applyHardwareConfig sets the mixer controls for fields that map to hardware.
// Called whenever the controller pushes a config message.
func applyHardwareConfig(msg config.ConfigMessage) {
	// Non-nil rather than non-zero: 0 is the bottom of each control's own
	// range and a legitimate setting. Under the old guard the dashboard
	// offered it, the config stored it, and the mic stayed where it was —
	// a control that appeared to work. Absent is still absent, so firmware
	// meeting a controller that omits the key behaves as it always did.
	if msg.AdcDigitalGain != nil {
		g := strconv.Itoa(*msg.AdcDigitalGain)
		for _, adc := range []string{"A", "B", "C", "D"} {
			mixer.Set("ADC_"+adc+" Digital Volume Control", g)
		}
	}
	if msg.AdcMicpga != nil {
		g := strconv.Itoa(*msg.AdcMicpga)
		for _, adc := range []string{"A", "B", "C", "D"} {
			mixer.Set("ADC_"+adc+" MICPGA Volume Ctrl", g)
		}
	}
}

// applyAecConfig pushes the current effective AEC config into the canceller.
// SetParams no-ops when nothing changed, so calling it on every config push
// is free; when delay/tail change it rebuilds the echo state (adaptive
// filter state is meaningless across a timing change anyway).
func applyAecConfig(canceller *aec.Canceller, dataClient *client.DataClient) {
	snap := config.Get().Snapshot()
	enabled := snap.AecEnabled != nil && *snap.AecEnabled
	delayMs := 250
	if snap.AecDelayMs != nil {
		delayMs = *snap.AecDelayMs
	}
	canceller.SetParams(enabled, delayMs, snap.AecTailMs)
	// The reference SOURCE lives on the data client, not the canceller: it
	// is the mic goroutine that extracts ch8 and decides per period whether
	// to hand it over, so that goroutine has to own the switch.
	dataClient.SetAecRefSource(snap.AecRefSource)
}

// benchSpeaker is handed the speaker at startup; set only in bench builds
// (play_bench.go).
var benchSpeaker func(*speaker.PcmSpeaker)

// playVolumeCue previews the newly selected device volume. PlayCue is mixed
// after software volume (so wake sounds can stay absolute), therefore these
// samples carry the device-volume gain themselves, under the law in force.
func playVolumeCue(spk *speaker.PcmSpeaker, level int) {
	if spk == nil {
		return
	}
	spk.PlayCue(cue.VolumeCue(speakerRate, spk.CueGain(level)))
}

// speakerRate mirrors the speaker binding's rate, declared here so this file
// still builds on a host, where the //go:build server binding does not.
const speakerRate = 48000

// controllerUp: a controller session is up (set by the link callbacks).
// LinkDown is the gate for the buttons and the ring, and a standalone
// satellite serving can lift it while this stays false.
var controllerUp atomic.Bool

// incubatorServingChanged re-decides the link-lost ring when a standalone
// satellite starts or stops serving (set in main).
var incubatorServingChanged = func() {}

// applyBleConfig starts/stops the BLE proxy scanner from the current
// effective config. SetEnabled is idempotent, so calling it on every config
// push is free.
func applyBleConfig(scanner *bluetooth.Scanner) {
	snap := config.Get().Snapshot()
	scanner.SetEnabled(snap.BleProxyEnabled != nil && *snap.BleProxyEnabled)
	sendspinProxy(scanner.Enabled())
}

func allLEDs(r, g, b uint8) []led.Led {
	leds := make([]led.Led, 12)
	for i := range leds {
		leds[i] = led.Led{ID: i, R: r, G: g, B: b}
	}
	return leds
}

// ─── LED animations ───────────────────────────────────────────────────────────

// pulsePhase returns the current point in a cycle of `period` that began at
// `start`, as a fraction in [0,1).
//
// Elapsed time, not a step counter: a counter advances one step per tick
// whether or not the tick was on time, so a delayed tick stretches that cycle
// rather than skipping ahead within it — the ring slows down under load and
// never catches up. animator.go's runPulse already works this way.
func pulsePhase(start time.Time, period time.Duration) float64 {
	return math.Mod(float64(time.Since(start))/float64(period), 1.0)
}

// pulseOrange — sine-wave orange pulse while disconnected from server.
//
// Started ONCE per disconnection, not once per reconnect attempt — see
// pulseKind at the OnDisconnected call site. Cancelling and relaunching this
// resets the phase to mid-brightness, which is visible as a stutter.
func pulseOrange(ctx context.Context, s *server.Server) {
	const (
		minBr  = 0.05
		maxBr  = 0.6
		period = 2000 * time.Millisecond
		stepMs = 50
	)
	ticker := time.NewTicker(stepMs * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t := pulsePhase(start, period)
			br := minBr + (maxBr-minBr)*(0.5+0.5*math.Sin(2*math.Pi*t))
			s.SetLEDs(allLEDs(uint8(255*br), uint8(40*br), 0), nil)
		}
	}
}

// pulseRefused — orange, odd and even LEDs crossfading against each other,
// while a controller refuses this device's credentials. Same colour as
// pulseOrange (a link problem), different shape (one the owner can fix).
func pulseRefused(ctx context.Context, s *server.Server) {
	const (
		minBr  = 0.03
		maxBr  = 0.6
		period = 1200 * time.Millisecond
		stepMs = 50
	)
	ticker := time.NewTicker(stepMs * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a := 0.5 + 0.5*math.Sin(2*math.Pi*pulsePhase(start, period))
			leds := allLEDs(0, 0, 0)
			for i := range leds {
				w := a
				if i%2 == 1 {
					w = 1 - a
				}
				br := minBr + (maxBr-minBr)*w
				leds[i].R, leds[i].G = uint8(255*br), uint8(40*br)
			}
			s.SetLEDs(leds, nil)
		}
	}
}

// pulseWhite — slow white pulse while pending controller approval.
// Slower and dimmer than orange to be visually distinct.
func pulseWhite(ctx context.Context, s *server.Server) {
	const (
		minBr  = 0.02
		maxBr  = 0.35
		period = 3000 * time.Millisecond // slower than orange
		stepMs = 50
	)
	ticker := time.NewTicker(stepMs * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t := pulsePhase(start, period)
			br := minBr + (maxBr-minBr)*(0.5+0.5*math.Sin(2*math.Pi*t))
			v := uint8(255 * br)
			s.SetLEDs(allLEDs(v, v, v), nil)
		}
	}
}

// ─── Thermals and core hotplug ────────────────────────────────────────────────
//
// Two facts about this SoC make these worth reporting, and they are related.
//
// The MT8163 is a QUAD-core Cortex-A53, but MediaTek's hotplug strategy parks
// cores that are not needed: /sys/devices/system/cpu/online is usually just
// "0". A second core comes online only after utilisation holds above
// /proc/hps/up_threshold (80%) for up_times (2) samples. So a device sitting
// at 54% is not near a ceiling — it is comfortably inside one core's budget
// with three more parked.
//
// That directly undermines cpuPct, which is derived from the aggregate
// /proc/stat line and is therefore a share of ONLINE capacity: the same
// absolute work halves its reported percentage the moment a second core
// appears. Reporting coresOnline alongside it is what makes the number
// interpretable rather than merely available.
//
// Thermals matter for the opposite reason — to show there is nothing to worry
// about, or to show when there is. thermalCoreLimit is the sharpest indicator
// this SoC offers: it is how many cores the thermal governor will currently
// permit, so anything below 4 means throttling has begun, which shows up as
// capacity loss long before a temperature reading looks alarming.

// thermalZones maps a zone type ("mtktscpu") to its temp file. Resolved once —
// the names are stable for the life of the boot, and rescanning 11 sysfs
// directories every 30s to learn nothing would be silly.
var (
	thermalOnce   sync.Once
	thermalByType map[string]string
)

func resolveThermalZones() map[string]string {
	thermalOnce.Do(func() {
		thermalByType = map[string]string{}
		dirs, err := filepath.Glob("/sys/class/thermal/thermal_zone*")
		if err != nil {
			return
		}
		for _, d := range dirs {
			b, err := os.ReadFile(filepath.Join(d, "type"))
			if err != nil {
				continue
			}
			thermalByType[strings.TrimSpace(string(b))] = filepath.Join(d, "temp")
		}
		log.Printf("[thermal] %d zones: %s", len(thermalByType), strings.Join(zoneTypes(), " "))
	})
	return thermalByType
}

func zoneTypes() []string {
	out := make([]string, 0, len(thermalByType))
	for t := range thermalByType {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// readMilliC reads a sysfs temperature (millidegrees C) as degrees.
func readMilliC(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	// Sanity bound: a plausible reading is roughly -20..150C. Some MTK zones
	// report a sentinel (0, or a huge value) when their sensor is not wired,
	// and averaging that into a trend would quietly ruin it — the same reason
	// the RF counters are deliberately not surfaced.
	c := float64(n) / 1000.0
	if c < -20 || c > 150 {
		return 0, false
	}
	return c, true
}

// thermals returns the CPU zone temperature, the hottest zone of any kind, and
// how many cores the thermal governor currently permits.
//
// mtktscpu is the SoC/CPU zone. The hottest-of-all figure is reported too
// because the PMIC and board sensors can run warmer than the CPU, and a device
// in trouble will not necessarily show it on the zone you thought to watch.
func thermals() (cpuC *float64, maxC *float64, coreLimit int) {
	zones := resolveThermalZones()
	if c, ok := readMilliC(zones["mtktscpu"]); ok {
		cpuC = &c
	}
	var hottest float64
	var any bool
	for _, p := range zones {
		if c, ok := readMilliC(p); ok && (!any || c > hottest) {
			hottest, any = c, true
		}
	}
	if any {
		maxC = &hottest
	}
	// /proc/hps/num_limit_thermal — cores the thermal governor allows. Absent
	// on a kernel without MTK HPS, reported as 0 = unknown rather than 0 cores.
	if b, err := os.ReadFile("/proc/hps/num_limit_thermal"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			coreLimit = n
		}
	}
	return cpuC, maxC, coreLimit
}

// coresOnline counts online CPUs from /sys/devices/system/cpu/online, whose
// format is a range list ("0", "0-3", "0,2-3").
func coresOnline() int {
	b, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return 0
	}
	n := 0
	for _, part := range strings.Split(strings.TrimSpace(string(b)), ",") {
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		if !found {
			if err1 == nil {
				n++
			}
			continue
		}
		z, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 == nil && err2 == nil && z >= a {
			n += z - a + 1
		}
	}
	return n
}

// hpsCoreFloor is the minimum number of CPU cores kept online.
//
// The MT8163 has four Cortex-A53 cores and MediaTek's hotplug strategy parks
// all but one, bringing a second up only after utilisation holds above
// /proc/hps/up_threshold (80%) for up_times (2) samples. That is a sensible
// default for an idle appliance and a poor one for this workload: the mic
// pipeline has a hard 160ms deadline (the ALSA ring's whole depth) and now
// shares a core with wake word inference that runs in ~31ms bursts. Time-
// slicing those on one core works — measured, zero stalls — but it works with
// no margin for a coincidence, and it depends on hotplug reacting in time to a
// burst that has already started.
//
// A floor of 2 lets the two actually run in parallel, and leaves up_threshold
// to scale to 3 and 4 exactly as before. The cost is one A53 core out of idle,
// which on a mains-powered device sitting at 33C is not a real cost: measured
// +0.3C at the PMIC and no change at the CPU zone.
//
// Set via num_base_perf_serv, which is HPS's core-count FLOOR (the num_limit_*
// files are its ceilings, all 4 here). Deliberately NOT done by writing
// cpu1/online directly: HPS would re-park it within down_times samples, and
// fighting the governor is how you get a setting that appears to work and
// silently stops.
const hpsCoreFloor = 2

// platformInitMarker is how start_server.sh tells a binary that has the
// platform-init mode from one that would ignore the argument and start a
// second server. It greps the binary for this string.
const platformInitMarker = "EM_PLATFORM_INIT_V1"

// platformInit applies the detected board's kernel tuning and exits. emOS only:
// on FireOS the vendor's thermal_manager owns this. An unknown board is not a
// failure — it keeps the kernel defaults, which are the stricter setting.
func platformInit() int {
	fmt.Printf("platform-init (%s) base=%s", platformInitMarker, platform.Base())
	b := board.Detect("")
	fmt.Printf(" board=%s", board.IDOf(b))
	if platform.Base() != platform.EmOS {
		fmt.Println(" — not emOS, nothing to do")
		return 0
	}
	if b == nil || b.Tuning == nil {
		fmt.Println(" — no profile, kernel defaults kept")
		return 0
	}
	lines, err := board.Apply("", b.Tuning)
	for _, l := range lines {
		fmt.Printf(" | %s", l)
	}
	if err != nil {
		fmt.Printf(" — FAILED: %v\n", err)
		return 1
	}
	fmt.Println(" — ok")
	return 0
}

// applyCoreFloor raises the hotplug floor, best-effort.
//
// procfs, so it does not survive a reboot — which is why it lives here, in the
// binary, rather than in a provisioning script: it travels with the firmware
// and re-applies on every start. Absent on a kernel without MTK HPS, in which
// case there is nothing to do and nothing to warn about.
func applyCoreFloor() {
	const path = "/proc/hps/num_base_perf_serv"
	before, err := os.ReadFile(path)
	if err != nil {
		return // not an MTK HPS kernel
	}
	if strings.TrimSpace(string(before)) == strconv.Itoa(hpsCoreFloor) {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(hpsCoreFloor)), 0o644); err != nil {
		log.Printf("[cpu] could not raise core floor to %d: %v", hpsCoreFloor, err)
		return
	}
	log.Printf("[cpu] core floor %s -> %d (online=%d, hotplug still scales above up_threshold)",
		strings.TrimSpace(string(before)), hpsCoreFloor, coresOnline())
}

// coresTotal is how many cores the SoC has, online or parked. Reported so a
// "1 of 4 online" reads as a power state rather than a one-core device — which
// is how the MT8163's hotplug behaviour gets misread.
func coresTotal() int {
	b, err := os.ReadFile("/sys/devices/system/cpu/present")
	if err != nil {
		return 0
	}
	n := 0
	for _, part := range strings.Split(strings.TrimSpace(string(b)), ",") {
		lo, hi, found := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		if !found {
			if err1 == nil {
				n++
			}
			continue
		}
		z, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 == nil && err2 == nil && z >= a {
			n += z - a + 1
		}
	}
	return n
}

// reportListenState tells the controller what this Echo does with its
// microphone, for the dashboard's privacy line (docs/listening.md, the
// controller's em_listen.resolve): "local" -- nothing ever goes to the
// controller; the Voice Incubator app hears it only in a call, after the
// Echo's own wake word or a press, or while it listens live, which
// incubator_state reports (monitored) -- or "degraded" with no microphone
// it can read. EchoHome: the old Home Assistant voice mode, its wake
// stream and its sessions are gone.
func reportListenState(cc *client.ControlClient) {
	if why := client.MicUnavailable(); why != "" {
		cc.SendListenState("degraded", "no microphone it can read: "+why)
		return
	}
	cc.SendListenState("local", "")
}
