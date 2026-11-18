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
	Effect     string  `json:"effect,omitempty"` // "" or "None": the plain colour
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
	effect := st.Effect
	if effect == "" {
		effect = "None"
	}
	return &api.LightStateResponse{State: st.On, Brightness: st.Brightness, ColorMode: api.ColorMode_COLOR_MODE_RGB,
		ColorBrightness: 1, Red: st.R, Green: st.G, Blue: st.B, Effect: effect}
}

// ledRingEntities builds the light and the action.
func ledRingEntities() []esphome.Entity {
	loadLEDRing()
	l := &esphome.Light{Desc: &api.ListEntitiesLightResponse{ObjectId: "led_ring", Key: esphomeLEDRingKey,
		Name: "LED Ring", Icon: "mdi:circle-outline",
		SupportedColorModes: []api.ColorMode{api.ColorMode_COLOR_MODE_RGB},
		Effects:             ledRingEffectNames}}
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
	if c.HasEffect {
		st.Effect = ""
		if _, ok := ledRingEffects[c.Effect]; ok {
			st.Effect = c.Effect
		}
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
	colour := [3]uint8{ch(st.R), ch(st.G), ch(st.B)}
	if effect, ok := ledRingEffects[st.Effect]; ok {
		return effect(colour, st.Brightness), true
	}
	return server.AnimSpec{Pattern: "solid", Colors: [][3]uint8{colour}}, true
}

// The LED Ring's effects, as an ESPHome light lists them ("None", the plain
// colour, first), drawn by the ring's own animator: in the light's colour
// and brightness, the rainbow in its own colours at the light's brightness.
var ledRingEffectNames = []string{"None", "Pulse", "Slow Pulse", "Spin", "Rainbow"}

var ledRingEffects = map[string]func(c [3]uint8, brightness float32) server.AnimSpec{
	"Pulse": func(c [3]uint8, _ float32) server.AnimSpec {
		return server.AnimSpec{Pattern: "pulse", Colors: [][3]uint8{c}, PeriodMs: 2000}
	},
	"Slow Pulse": func(c [3]uint8, _ float32) server.AnimSpec {
		return server.AnimSpec{Pattern: "pulse", Colors: [][3]uint8{c}, PeriodMs: 5000}
	},
	"Spin": func(c [3]uint8, _ float32) server.AnimSpec {
		trail := [3]uint8{c[0] / 4, c[1] / 4, c[2] / 4}
		return server.AnimSpec{Pattern: "spin", Colors: [][3]uint8{c, trail}, PeriodMs: 90}
	},
	"Rainbow": func(_ [3]uint8, brightness float32) server.AnimSpec {
		return server.AnimSpec{Pattern: "rotate", Colors: rainbow(brightness), PeriodMs: 150}
	},
}

// rainbow is twelve hues around the ring at full saturation, scaled.
func rainbow(brightness float32) [][3]uint8 {
	out := make([][3]uint8, 12)
	for i := range out {
		h := float64(i) / 12 * 6
		x := 1 - math.Abs(math.Mod(h, 2)-1)
		var r, g, b float64
		switch int(h) {
		case 0:
			r, g = 1, x
		case 1:
			r, g = x, 1
		case 2:
			g, b = 1, x
		case 3:
			g, b = x, 1
		case 4:
			r, b = x, 1
		default:
			r, b = 1, x
		}
		k := float64(clamp01(brightness)) * 255
		out[i] = [3]uint8{uint8(math.Round(r * k)), uint8(math.Round(g * k)), uint8(math.Round(b * k))}
	}
	return out
}
