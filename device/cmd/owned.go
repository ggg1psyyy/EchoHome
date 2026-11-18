package main

import (
	"log"

	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/owned"
	"github.com/wilbowes/EchoMuse/internal/server"
)

// The settings Home Assistant can change on the Echo itself, as switches on
// its own ESPHome device: the red ring while muted (muteRing), the volume
// arc for remote changes (remoteVolumeArc) and the Voice Incubator app's
// live listening (incubatorMonitor). Kept on the Echo (internal/owned), so a
// change outlives a reboot with no controller, reported to the controller
// (settings_changed), which stores it as this Echo's own setting and pushes
// it back; until that push carries it, the Echo's value stands against the
// controller's (owned.Store.Reconcile). The mute is a switch too; it is the
// Echo's already (state.json), so it needs none of this.

const (
	ownedMuteRing        = "muteRing"
	ownedRemoteVolumeArc = "remoteVolumeArc"
	ownedMonitor         = "incubatorMonitor"
)

// The entity keys of the Echo's own settings: 20 and up, clear of the
// controller's (em_esphome.py, 1-6, reserved there).
const (
	esphomeMuteKey            = 20
	esphomeMuteRingKey        = 21
	esphomeRemoteVolumeArcKey = 22
	esphomeLiveListeningKey   = 23
)

var (
	ownedSettings *owned.Store
	ownedCC       *client.ControlClient
	ownedSrv      *server.Server
	ownedSwitch   = map[string]*esphome.Switch{}
	muteSwitch    *esphome.Switch
)

// setupOwned loads the store and builds the switches; their values are put
// in force by applyOwnedAtBoot.
func setupOwned(cc *client.ControlClient, s *server.Server, muteRingCapable bool) []esphome.Entity {
	ownedSettings = owned.Load(owned.DefaultPath)
	ownedCC, ownedSrv = cc, s
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
	return out
}

// muteSource: a mute made by hand only the hand undoes (owned.MuteSource).
var muteSource owned.MuteSource

// muteEntity is the mute as a switch, as a Voice PE has it.
func muteEntity(s *server.Server) *esphome.Switch {
	muteSwitch = &esphome.Switch{Desc: &api.ListEntitiesSwitchResponse{ObjectId: "mute", Key: esphomeMuteKey,
		Name: "Mute", Icon: "mdi:microphone-off"}}
	muteSwitch.OnCommand = func(on bool) {
		toggle, ok := muteSource.Command(on, s.IsMuted())
		if !ok {
			log.Printf("[esphome] unmute from Home Assistant refused: muted by hand, only the button unmutes")
			muteSwitch.Set(true)
			return
		}
		if toggle {
			s.MuteToggle() // the mute callback reports the result
		}
	}
	muteSource.Boot(s.IsMuted())
	muteSwitch.Set(s.IsMuted())
	return muteSwitch
}

// applyOwnedAtBoot puts the kept values in force, after the standalone
// satellite has started from its own saved copy (the Echo's are newer).
func applyOwnedAtBoot() {
	if ownedSettings == nil {
		return
	}
	for _, key := range []string{ownedMuteRing, ownedRemoteVolumeArc, ownedMonitor} {
		if v, ok := ownedSettings.Value(key); ok {
			applyOwned(key, v)
		}
	}
}

// setOwned is Home Assistant's change: kept, in force, reported.
func setOwned(key string, v bool) {
	if err := ownedSettings.Set(key, v); err != nil {
		// Not kept is not done: HA's switch stays where it was.
		log.Printf("[settings] %s could not be kept: %v", key, err)
		return
	}
	log.Printf("[settings] %s set to %v from Home Assistant", key, v)
	applyOwned(key, v)
	reportOwned()
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

// reconcileOwnedConfig runs first on every config push: a pending change
// stands against an older pushed value; otherwise the push is the value.
func reconcileOwnedConfig() {
	if ownedSettings == nil {
		return
	}
	if v := ownedSettings.Reconcile(ownedMuteRing, config.Get().MuteRingOn()); v != config.Get().MuteRingOn() {
		config.Get().Apply(config.ConfigMessage{MuteRing: &v})
	}
	if v := ownedSettings.Reconcile(ownedRemoteVolumeArc, config.Get().RemoteVolumeArcEnabled()); v != config.Get().RemoteVolumeArcEnabled() {
		config.Get().Apply(config.ConfigMessage{RemoteVolumeArc: &v})
	}
	for key, now := range map[string]bool{ownedMuteRing: config.Get().MuteRingOn(),
		ownedRemoteVolumeArc: config.Get().RemoteVolumeArcEnabled()} {
		if w := ownedSwitch[key]; w != nil {
			w.Set(now)
		}
	}
	reportOwned()
}

// reconcileOwnedMonitor does the same for the incubator_satellite message.
func reconcileOwnedMonitor(pushed bool) bool {
	if ownedSettings == nil {
		return pushed
	}
	v := ownedSettings.Reconcile(ownedMonitor, pushed)
	if w := ownedSwitch[ownedMonitor]; w != nil {
		w.Set(v)
	}
	return v
}

// reportOwned sends what the controller has yet to acknowledge.
func reportOwned() {
	if ownedCC == nil || ownedSettings == nil {
		return
	}
	if p := ownedSettings.Pending(); len(p) > 0 {
		ownedCC.SendSettingsChanged(p)
	}
}

// esphomeMuteSwitch follows the mute, and records who made it.
func esphomeMuteSwitch(muted bool) {
	muteSource.Changed(muted)
	if muteSwitch != nil {
		muteSwitch.Set(muted)
	}
}
