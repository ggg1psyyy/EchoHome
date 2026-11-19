package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/owned"
	"github.com/wilbowes/EchoMuse/internal/sendspin"
	"github.com/wilbowes/EchoMuse/internal/server"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// The settings Home Assistant changes on the Echo itself, as switches on its
// own ESPHome device: the red ring while muted (muteRing), the volume arc for
// remote changes (remoteVolumeArc), the Voice Incubator app's live listening
// (incubatorMonitor), the Bluetooth proxy (bleProxyEnabled) and the Sendspin
// player (sendspinEnabled; its pairing stays the setup tool's). The Echo is
// their only owner since EchoHome: kept here (internal/owned), never
// reported, and a controller push is taken only for one the Echo has no
// value for yet (an Echo upgraded with no kept value keeps what its old
// controller last pushed). The mute is a switch too: Home Assistant's soft
// mute, the Echo's already (state.json), so it needs none of this.

const (
	ownedMuteRing        = "muteRing"
	ownedRemoteVolumeArc = "remoteVolumeArc"
	ownedMonitor         = "incubatorMonitor"
	ownedBLEProxy        = "bleProxyEnabled"
	ownedSendspin        = "sendspinEnabled"
	// ownedSendspinPushed is the controller's last sendspinEnabled: a push
	// that differs from it is a change made on the dashboard (sendspinPushed).
	ownedSendspinPushed = "sendspinEnabledPushed"
	ownedWakeMic        = "wakeMic" // a number: ownedSettings' Ints
)

// The entity keys of the Echo's own settings: 20 and up, clear of the
// controller's (em_esphome.py, 1-6, reserved there).
const (
	esphomeMuteKey            = 20
	esphomeMuteRingKey        = 21
	esphomeRemoteVolumeArcKey = 22
	esphomeLiveListeningKey   = 23
	esphomeBLEProxyKey        = 27
	esphomeWakeMicKey         = 28
	esphomeSendspinKey        = 29
	// 30 and 31 were the Sendspin unpaired switch and token sensor, on a
	// development build only (2026-11-19): not reused.
)

var (
	ownedSettings *owned.Store
	ownedSrv      *server.Server
	ownedSwitch   = map[string]*esphome.Switch{}
	muteSwitch    *esphome.Switch
	wakeMicSelect *esphome.Select
)

// setupOwned loads the store and builds the switches; their values are put
// in force by applyOwnedAtBoot.
func setupOwned(s *server.Server, muteRingCapable bool) []esphome.Entity {
	ownedSettings = owned.Load(owned.DefaultPath)
	ownedSrv = s
	var out []esphome.Entity
	cfgSwitch := func(key string, k uint32, objectID, name, icon string, now bool) {
		w := &esphome.Switch{Desc: &api.ListEntitiesSwitchResponse{ObjectId: objectID, Key: k, Name: name, Icon: icon,
			EntityCategory: api.EntityCategory_ENTITY_CATEGORY_CONFIG}}
		w.OnCommand = func(on bool) { setOwned(key, on) }
		if v, ok := ownedSettings.Value(key); ok {
			now = v
		}
		w.Set(now)
		ownedSwitch[key] = w
		out = append(out, w)
	}
	if muteRingCapable {
		cfgSwitch(ownedMuteRing, esphomeMuteRingKey, "mute_ring", "Red ring while muted", "mdi:circle-outline",
			config.Get().MuteRingOn())
	}
	cfgSwitch(ownedRemoteVolumeArc, esphomeRemoteVolumeArcKey, "remote_volume_arc", "Volume arc for remote changes",
		"mdi:volume-high", config.Get().RemoteVolumeArcEnabled())
	cfgSwitch(ownedMonitor, esphomeLiveListeningKey, "live_listening", "Voice Incubator live listening",
		"mdi:ear-hearing", false)
	cfgSwitch(ownedBLEProxy, esphomeBLEProxyKey, "bluetooth_proxy", "Bluetooth proxy",
		"mdi:bluetooth", bleProxyOn())
	cfgSwitch(ownedSendspin, esphomeSendspinKey, "sendspin", "Sendspin player",
		"mdi:speaker-wireless", sendspinOn())
	if client.MicUnavailable() == "" {
		out = append(out, wakeMicEntity())
	}
	out = append(out, musicRingEntity())
	return out
}

// The wake word microphone (wakeMic): the one microphone the Echo listens on,
// for its wake word and its calls alike (the beamformer never locks since
// EchoHome). 0 is the board's default, its centre mic, or its first edge mic
// where it has none; 1..N pick an edge mic. An escape hatch for a dead centre
// mic (#705; 0SE7's), kept on the Echo like the switches above.
func wakeMicOptions() []string {
	mics := board.CurrentLayout().Mics
	first := "Centre"
	if mics.Centre < 0 {
		first = "Default (MK1)"
	}
	opts := []string{first}
	for i := range mics.Perimeter {
		opts = append(opts, fmt.Sprintf("MK%d", i+1))
	}
	return opts
}

func wakeMicEntity() *esphome.Select {
	opts := wakeMicOptions()
	wakeMicSelect = &esphome.Select{Desc: &api.ListEntitiesSelectResponse{ObjectId: "wake_word_mic", Key: esphomeWakeMicKey,
		Name: "Wake word microphone", Icon: "mdi:microphone", Options: opts,
		EntityCategory: api.EntityCategory_ENTITY_CATEGORY_CONFIG}}
	wakeMicSelect.OnCommand = func(option string) {
		for i, o := range opts {
			if o == option {
				setWakeMic(i)
				return
			}
		}
	}
	now := wakeMicNow()
	if v, ok := ownedSettings.IntValue(ownedWakeMic); ok {
		now = v
	}
	showWakeMic(now)
	return wakeMicSelect
}

// wakeMicNow is the value in force.
func wakeMicNow() int {
	if v := config.Get().Snapshot().WakeMic; v != nil {
		return *v
	}
	return 0
}

// showWakeMic puts a value on the select, the default for one it has no
// option for.
func showWakeMic(v int) {
	if wakeMicSelect == nil {
		return
	}
	opts := wakeMicSelect.Desc.Options
	if v < 0 || v >= len(opts) {
		v = 0
	}
	wakeMicSelect.Set(opts[v])
}

// setWakeMic is Home Assistant's choice: kept, in force from the next
// microphone batch (streamMic reads it every period), and shown.
func setWakeMic(v int) {
	if err := ownedSettings.SetLocalInt(ownedWakeMic, v); err != nil {
		log.Printf("[settings] wakeMic could not be kept: %v", err)
		return
	}
	log.Printf("[settings] wakeMic set to %d from Home Assistant", v)
	config.Get().Apply(config.ConfigMessage{WakeMic: &v})
	showWakeMic(v)
}

// muteEntity is Home Assistant's "Software Mute" (internal/softmute): the
// microphones silenced in software, the button's hardware mute apart. It
// shows only its own state; it cannot be turned on while the button's mute
// is on, and any press of the button clears it (chosen 2026-11-18).
func muteEntity(s *server.Server) *esphome.Switch {
	muteSwitch = &esphome.Switch{Desc: &api.ListEntitiesSwitchResponse{ObjectId: "mute", Key: esphomeMuteKey,
		Name: "Software Mute", Icon: "mdi:microphone-off"}}
	muteSwitch.OnCommand = func(on bool) {
		s.SetSoftMute(on)
		muteSwitch.Set(s.SoftMuted()) // a refusal leaves it off
	}
	muteSwitch.Set(s.SoftMuted())
	softMuted = s.SoftMuted
	return muteSwitch
}

// softMuted reads the software mute for the mute callback.
var softMuted func() bool

// applyOwnedAtBoot puts the kept values in force, after the standalone
// satellite has started from its own saved copy (the Echo's are newer).
func applyOwnedAtBoot() {
	if ownedSettings == nil {
		return
	}
	for _, key := range []string{ownedMuteRing, ownedRemoteVolumeArc, ownedMonitor, ownedBLEProxy,
		ownedSendspin} {
		if v, ok := ownedSettings.Value(key); ok {
			applyOwned(key, v)
		}
	}
	if v, ok := ownedSettings.IntValue(ownedWakeMic); ok {
		config.Get().Apply(config.ConfigMessage{WakeMic: &v})
		showWakeMic(v)
	}
}

// setOwned is Home Assistant's change: kept and in force.
func setOwned(key string, v bool) {
	if key == ownedSendspin && v && !sendspinPlayable() {
		// Pairing is the setup tool's: until it has paired the Echo (or
		// allowed unpaired servers) a player would only wait for nobody.
		log.Printf("[settings] Sendspin player not turned on from Home Assistant: not paired with a server (pair it in the setup tool)")
		if w := ownedSwitch[key]; w != nil {
			w.Set(sendspinOn()) // HA's switch is not optimistic: no state change, and it shows off again
		}
		return
	}
	if err := ownedSettings.SetLocal(key, v); err != nil {
		// Not kept is not done: HA's switch stays where it was.
		log.Printf("[settings] %s could not be kept: %v", key, err)
		return
	}
	log.Printf("[settings] %s set to %v from Home Assistant", key, v)
	applyOwned(key, v)
}

// applyOwned puts a value in force and shows it on the switch.
func applyOwned(key string, v bool) {
	switch key {
	case ownedMuteRing:
		config.Get().Apply(config.ConfigMessage{MuteRing: &v})
		ownedSrv.SetMuteRing(v)
		incubatorSaveSoon()
	case ownedRemoteVolumeArc:
		config.Get().Apply(config.ConfigMessage{RemoteVolumeArc: &v})
		ownedSrv.SetRemoteVolumeArc(v)
	case ownedBLEProxy:
		config.Get().Apply(config.ConfigMessage{BleProxyEnabled: &v})
		if bleRefs.scanner != nil {
			applyBleConfig(bleRefs.scanner)
		}
		esphomeBLEFollow()
	case ownedSendspin:
		config.Get().Apply(config.ConfigMessage{SendspinEnabled: &v})
		sendspinReapply()
	case ownedMonitor:
		if incubatorMgr != nil {
			if st := incubatorMgr.Want(); st.Enabled && st.Monitor != v {
				st.Monitor = v
				incubatorMgr.Apply(st) // restarts the satellite: the app reads the new HELLO
				incubatorSaveSoon()
			}
		}
	}
	if w := ownedSwitch[key]; w != nil {
		w.Set(v)
	}
}

// reconcileOwnedConfig runs first on every config push: the Echo's own value
// stands over whatever the push carried; the push counts only for a key the
// Echo has none of yet (owned.Store.Seed).
func reconcileOwnedConfig() {
	if ownedSettings == nil {
		return
	}
	if v := ownedSettings.Seed(ownedMuteRing, config.Get().MuteRingOn()); v != config.Get().MuteRingOn() {
		config.Get().Apply(config.ConfigMessage{MuteRing: &v})
	}
	if v := ownedSettings.Seed(ownedRemoteVolumeArc, config.Get().RemoteVolumeArcEnabled()); v != config.Get().RemoteVolumeArcEnabled() {
		config.Get().Apply(config.ConfigMessage{RemoteVolumeArc: &v})
	}
	if v := ownedSettings.Seed(ownedBLEProxy, bleProxyOn()); v != bleProxyOn() {
		config.Get().Apply(config.ConfigMessage{BleProxyEnabled: &v})
	}
	if v := ownedSettings.Seed(ownedSendspin, sendspinOn()); v != sendspinOn() {
		config.Get().Apply(config.ConfigMessage{SendspinEnabled: &v})
	}

	// The controller still pushes wakeMic for firmware from before EchoHome:
	// here it only seeds an Echo that has no choice of its own yet, so a mic
	// picked on the dashboard before the upgrade carries over (0SE7).
	if now := wakeMicNow(); ownedSettings.SeedInt(ownedWakeMic, now) != now {
		v, _ := ownedSettings.IntValue(ownedWakeMic)
		config.Get().Apply(config.ConfigMessage{WakeMic: &v})
	}
	showWakeMic(wakeMicNow())
	for key, now := range map[string]bool{ownedMuteRing: config.Get().MuteRingOn(),
		ownedRemoteVolumeArc: config.Get().RemoteVolumeArcEnabled(), ownedBLEProxy: bleProxyOn(),
		ownedSendspin: sendspinOn()} {
		if w := ownedSwitch[key]; w != nil {
			w.Set(now)
		}
	}
}

// reconcileOwnedMonitor does the same for the incubator_satellite message.
func reconcileOwnedMonitor(pushed bool) bool {
	if ownedSettings == nil {
		return pushed
	}
	v := ownedSettings.Seed(ownedMonitor, pushed)
	if w := ownedSwitch[ownedMonitor]; w != nil {
		w.Set(v)
	}
	return v
}

// esphomeMuteSwitch follows the software mute on any change of the mute
// (the button clears it).
func esphomeMuteSwitch(bool) {
	if muteSwitch != nil && softMuted != nil {
		muteSwitch.Set(softMuted())
	}
}

// bleProxyOn is the Bluetooth proxy's setting in force.
func bleProxyOn() bool {
	snap := config.Get().Snapshot()
	return snap.BleProxyEnabled != nil && *snap.BleProxyEnabled
}

// sendspinOn and sendspinUnpairedOn are the Sendspin player's settings in
// force.
func sendspinOn() bool {
	snap := config.Get().Snapshot()
	return snap.SendspinEnabled != nil && *snap.SendspinEnabled
}

func sendspinUnpairedOn() bool {
	snap := config.Get().Snapshot()
	return snap.SendspinUnpaired != nil && *snap.SendspinUnpaired
}

// sendspinPlayable says whether a player would have anyone to play for: a
// server paired with it (in the setup tool), or unpaired servers allowed.
func sendspinPlayable() bool {
	if c := sendspinPlayer(); c != nil && c.Status().PairedWith > 0 {
		return true
	}
	return sendspin.PairedServers(sendspinStore) > 0 || sendspinUnpairedOn()
}

// sendspinPushed takes a controller push's sendspinEnabled before the
// Echo's own settings are put back over it: the dashboard's switch and Home
// Assistant's set the same value, the last change winning. The controller
// sends its value on every connect, so only one that differs from the last
// it sent is a change; the first one this firmware sees only seeds
// (reconcileOwnedConfig). The dashboard turns the player on to pair it, so
// no pairing is needed for this one.
func sendspinPushed(p *bool) {
	if ownedSettings == nil || p == nil {
		return
	}
	last, known := ownedSettings.Value(ownedSendspinPushed)
	if known && last == *p {
		return
	}
	if err := ownedSettings.SetLocal(ownedSendspinPushed, *p); err != nil {
		log.Printf("[settings] %s could not be kept: %v", ownedSendspinPushed, err)
		return
	}
	if known {
		if err := ownedSettings.SetLocal(ownedSendspin, *p); err == nil {
			log.Printf("[settings] %s set to %v from the setup tool", ownedSendspin, *p)
		}
	}
}

// The Echo's own volume, kept in the kept config (config.LastPath) as its
// startupVolume, so a level changed with no controller comes back at the
// next boot; the kept config is what seeds it first. Debounced: the buttons
// step the level, and each step must not be a flash write.
var keepVolume struct {
	sync.Mutex
	timer *time.Timer
}

func keepVolumeSoon(level int) {
	if level <= 0 {
		return // silent is never kept as the volume (em_output_mute's rule)
	}
	config.Get().Apply(config.ConfigMessage{StartupVolume: level})
	keepVolume.Lock()
	defer keepVolume.Unlock()
	if keepVolume.timer != nil {
		keepVolume.timer.Stop()
	}
	keepVolume.timer = time.AfterFunc(2*time.Second, func() {
		if _, err := config.Get().SaveLast(config.LastPath); err != nil {
			log.Printf("[config] volume not kept: %v", err)
		}
	})
}
