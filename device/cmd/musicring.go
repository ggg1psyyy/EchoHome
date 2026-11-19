package main

import (
	"encoding/json"
	"log"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/mediaplayer"
	"github.com/wilbowes/EchoMuse/internal/server"
)

// The ring while music plays: Home Assistant's "Ring while music plays"
// select on the Echo's own device, kept on the Echo (ownedMusicRing, an
// index into musicRingModes). While music plays, Home Assistant's or a
// Sendspin group's, the idle ring shows it instead of the LED Ring light;
// the satellite's states, the volume arc and the mute ring stay above it, as
// they are above the light. Night mode dims and warms it as every idle ring.

const (
	ownedMusicRing      = "musicRing" // an int: ownedSettings' Ints
	esphomeMusicRingKey = 32
)

// musicRingModes are the select's options. Append-only: the Echo keeps the
// index.
var musicRingModes = []string{"Off", "Level meter", "Colour drift"}

const (
	musicRingOff = iota
	musicRingMeter
	musicRingDrift
)

var musicRingSelect *esphome.Select

func musicRingEntity() *esphome.Select {
	musicRingSelect = &esphome.Select{Desc: &api.ListEntitiesSelectResponse{ObjectId: "music_ring", Key: esphomeMusicRingKey,
		Name: "Ring while music plays", Icon: "mdi:music-circle-outline", Options: musicRingModes,
		EntityCategory: api.EntityCategory_ENTITY_CATEGORY_CONFIG}}
	musicRingSelect.OnCommand = func(option string) {
		for i, o := range musicRingModes {
			if o == option {
				if err := ownedSettings.SetLocalInt(ownedMusicRing, i); err != nil {
					log.Printf("[settings] %s could not be kept: %v", ownedMusicRing, err)
					return
				}
				log.Printf("[settings] %s set to %q from Home Assistant", ownedMusicRing, o)
				musicRingSelect.Set(o)
				musicRingRepaint()
				return
			}
		}
	}
	musicRingSelect.Set(musicRingModes[musicRingMode()])
	return musicRingSelect
}

// musicRingMode is the mode in force: Off unless one was chosen.
func musicRingMode() int {
	if ownedSettings != nil {
		if v, ok := ownedSettings.IntValue(ownedMusicRing); ok && v >= 0 && v < len(musicRingModes) {
			return v
		}
	}
	return musicRingOff
}

// musicPlaying is whether music plays here now: Home Assistant's media
// player (not paused, not an announcement alone) or a Sendspin stream.
func musicPlaying() bool {
	if mp := mediaPlayer; mp != nil && mp.State() == mediaplayer.Playing {
		return true
	}
	c := sendspinPlayer()
	return c != nil && c.Active()
}

// musicRingSpec is the idle ring while music plays, or false: with no music,
// or the mode Off. specs are the scene's (incubatorRing): the meter is the
// scene's reply meter, following the music; with none, the LED Ring light's
// colour, or white.
func musicRingSpec(specs map[string]json.RawMessage) (server.AnimSpec, bool) {
	mode := musicRingMode()
	if mode == musicRingOff || !musicPlaying() {
		return server.AnimSpec{}, false
	}
	if mode == musicRingDrift {
		return server.AnimSpec{Pattern: "rotate", Colors: rainbow(0.4), PeriodMs: 400}, true
	}
	var spec server.AnimSpec
	if raw := specs["meter"]; len(raw) == 0 || json.Unmarshal(raw, &spec) != nil || spec.Pattern != "meter" {
		colour := [3]uint8{160, 160, 160}
		if idle, ok := ledRingIdle(); ok && len(idle.Colors) > 0 {
			colour = idle.Colors[0]
		}
		spec = server.AnimSpec{Pattern: "meter", Colors: [][3]uint8{colour}}
	}
	spec.Source, spec.TTLSec = "music", 0
	return spec, true
}

// musicRingRepaint paints the idle ring again (the satellite's, which knows
// whether the ring is idle).
func musicRingRepaint() {
	if incubatorMgr != nil {
		incubatorMgr.Repaint()
	}
}

// watchMusicRing repaints when music starts or stops while a mode is chosen.
// Polled: music starts on three paths (Home Assistant, Sendspin, a resume)
// and a quarter second is soon enough for a ring.
func watchMusicRing() {
	was := false
	for range time.Tick(250 * time.Millisecond) {
		now := musicRingMode() != musicRingOff && musicPlaying()
		if now != was {
			was = now
			musicRingRepaint()
		}
	}
}
