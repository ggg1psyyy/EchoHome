package main

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"sync"

	"github.com/wilbowes/EchoMuse/internal/atomicfile"
	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/server"
)

// The LED Ring light and the start_call action, as a Voice PE on Voice
// Incubator firmware shows them to Home Assistant (firmware-voice-incubator
// devices/voice-pe.yaml): the light is the ring's colour and brightness
// between calls, "otherwise the LED Ring light if it is on", under every
// state the satellite shows and under the mute ring; start_call starts a
// call as the action button does (voice_incubator.start: manual).
//
// The light's state is kept on the Echo (ledRingPath), as ESPHome restores
// a light's, and painted where the satellite's ring would be dark.

const (
	esphomeLEDRingKey   = 24
	esphomeStartCallKey = 25
	ledRingPath         = "/data/local/etc/echomuse/ledring.json"
)

type ledRingState struct {
	On         bool    `json:"on"`
	Brightness float32 `json:"brightness"`
	R          float32 `json:"r"`
	G          float32 `json:"g"`
	B          float32 `json:"b"`
}

var ledRing = struct {
	sync.Mutex
	st     ledRingState
	entity *esphome.Light
}{st: ledRingState{Brightness: 1, R: 1, G: 1, B: 1}}

func loadLEDRing() {
	b, err := os.ReadFile(ledRingPath)
	if err != nil {
		return
	}
	var st ledRingState
	if json.Unmarshal(b, &st) == nil {
		ledRing.st = st
	}
}

func (st ledRingState) response() *api.LightStateResponse {
	return &api.LightStateResponse{State: st.On, Brightness: st.Brightness, ColorMode: api.ColorMode_COLOR_MODE_RGB,
		ColorBrightness: 1, Red: st.R, Green: st.G, Blue: st.B}
}

// ledRingEntities builds the light and the action.
func ledRingEntities() []esphome.Entity {
	loadLEDRing()
	l := &esphome.Light{Desc: &api.ListEntitiesLightResponse{ObjectId: "led_ring", Key: esphomeLEDRingKey,
		Name: "LED Ring", Icon: "mdi:circle-outline",
		SupportedColorModes: []api.ColorMode{api.ColorMode_COLOR_MODE_RGB}}}
	l.OnCommand = ledRingCommand
	ledRing.Lock()
	ledRing.entity = l
	st := ledRing.st
	ledRing.Unlock()
	l.Set(st.response())
	start := &esphome.Action{Name: "start_call", ID: esphomeStartCallKey, OnCall: func() {
		if incubatorMgr == nil || !incubatorMgr.Press() {
			log.Println("[esphome] start_call: no Voice Incubator satellite running")
		}
	}}
	return []esphome.Entity{l, start}
}

func ledRingCommand(c *api.LightCommandRequest) {
	ledRing.Lock()
	st := ledRing.st
	if c.HasState {
		st.On = c.State
	}
	if c.HasBrightness {
		st.Brightness = clamp01(c.Brightness)
	}
	if c.HasRgb {
		st.R, st.G, st.B = clamp01(c.Red), clamp01(c.Green), clamp01(c.Blue)
	}
	changed := st != ledRing.st
	ledRing.st = st
	e := ledRing.entity
	ledRing.Unlock()
	if changed {
		if b, err := json.Marshal(st); err == nil {
			if err := atomicfile.Write(ledRingPath, b, 0o600); err != nil {
				log.Printf("[esphome] LED Ring state not kept: %v", err)
			}
		}
		if incubatorMgr != nil {
			incubatorMgr.Repaint()
		}
	}
	if e != nil {
		e.Set(st.response())
	}
}

func clamp01(v float32) float32 {
	if math.IsNaN(float64(v)) {
		return 0
	}
	return min(max(v, 0), 1)
}

// ledRingIdle is the light as the ring's idle scene, or false when it is off.
func ledRingIdle() (server.AnimSpec, bool) {
	ledRing.Lock()
	st := ledRing.st
	ledRing.Unlock()
	if !st.On {
		return server.AnimSpec{}, false
	}
	ch := func(v float32) uint8 { return uint8(math.Round(float64(clamp01(v*st.Brightness)) * 255)) }
	return server.AnimSpec{Pattern: "solid", Colors: [][3]uint8{{ch(st.R), ch(st.G), ch(st.B)}}}, true
}
