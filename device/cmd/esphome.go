package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/wilbowes/EchoMuse/internal/bindings/als"
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/firewall"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// The Echo's own ESPHome API server (internal/esphome), run while the
// config's esphomeServer says so: Home Assistant can then connect to the
// Echo itself, as it does to a Voice PE. While the controller still presents
// the Echo to HA, the Echo's server is a separate device in HA, under its
// own name ("echohome-<serial>") and MAC, so it never takes over the
// controller's entry; the entities' keys and object ids are the
// controller's (em_esphome.py, test-pinned) for the day it does.
//
// The entities are the ones the Echo has: the action button's hold (every
// board), ambient light where a sensor reads, the CPU and hottest thermal
// zone where the kernel exposes one, and the microphone mute where there is
// a microphone. Values are read where the stats tick reads them.

// The controller's entity keys (em_esphome.py), so HA's entities survive
// the Echo taking over from it.
const (
	esphomeEventKey    = 2 // EVENT_KEY
	esphomeAmbientKey  = 3 // AMBIENT_LUX_KEY
	esphomeMicMutedKey = 4 // MIC_MUTED_KEY
	esphomeCPUTempKey  = 5 // CPU_TEMP_KEY
	esphomeMaxTempKey  = 6 // MAX_TEMP_KEY
)

// esphomeButtonEventTypes are the controller's BUTTON_EVENT_TYPES.
var esphomeButtonEventTypes = []string{"long", "single", "double", "triple"}

// esphomePoll is how often the sensors are read: the stats tick's pace;
// the light sensor itself is read every 5 s (als), never faster.
const esphomePoll = 30 * time.Second

type esphomeEcho struct {
	mu      sync.Mutex
	srv     *esphome.Server
	stop    chan struct{}
	lux     *esphome.Sensor
	cpu     *esphome.Sensor
	maxT    *esphome.Sensor
	muted   *esphome.BinarySensor
	entries []esphome.Entity
	info    esphome.DeviceInfo
}

var esphomeOnEcho *esphomeEcho

// setupESPHome builds the Echo's entities and identity; nothing listens
// until applyESPHome turns it on.
func setupESPHome(muted bool) {
	serial := client.GetSerialNo()
	short := strings.ToLower(serial)
	if len(short) > 12 {
		short = short[len(short)-12:]
	}
	sum := sha256.Sum256([]byte("echohome:" + serial))
	id := board.IDOf(board.Current())
	e := &esphomeEcho{info: esphome.DeviceInfo{
		Name:         "echohome-" + short,
		FriendlyName: "EchoHome " + strings.ToUpper(short[max(0, len(short)-4):]),
		MAC:          fmt.Sprintf("02:EC:%02X:%02X:%02X:%02X", sum[0], sum[1], sum[2], sum[3]),
		Manufacturer: "EchoHome",
		Model:        id,
		// A dot is required: HA splits project_name on it.
		ProjectName:    "EchoHome." + id,
		ProjectVersion: client.Version,
	}}
	e.entries = append(e.entries, &esphome.Event{Desc: &api.ListEntitiesEventResponse{ObjectId: "action_button",
		Key: esphomeEventKey, Name: "Action Button", DeviceClass: "button", EventTypes: esphomeButtonEventTypes}})
	if als.Present() {
		e.lux = &esphome.Sensor{Desc: &api.ListEntitiesSensorResponse{ObjectId: "ambient_light", Key: esphomeAmbientKey,
			Name: "Ambient Light", UnitOfMeasurement: "lx", DeviceClass: "illuminance",
			StateClass: api.SensorStateClass_STATE_CLASS_MEASUREMENT}}
		e.entries = append(e.entries, e.lux)
	}
	if cpuC, _, _ := thermals(); cpuC != nil {
		temp := func(objectID string, key uint32, name string) *esphome.Sensor {
			return &esphome.Sensor{Desc: &api.ListEntitiesSensorResponse{ObjectId: objectID, Key: key, Name: name,
				UnitOfMeasurement: "°C", AccuracyDecimals: 1, DeviceClass: "temperature",
				StateClass:     api.SensorStateClass_STATE_CLASS_MEASUREMENT,
				EntityCategory: api.EntityCategory_ENTITY_CATEGORY_DIAGNOSTIC, DisabledByDefault: true}}
		}
		e.cpu = temp("cpu_temperature", esphomeCPUTempKey, "CPU Temperature")
		e.maxT = temp("hottest_zone_temperature", esphomeMaxTempKey, "Hottest Zone Temperature")
		e.entries = append(e.entries, e.cpu, e.maxT)
	}
	if client.MicUnavailable() == "" {
		e.muted = &esphome.BinarySensor{Desc: &api.ListEntitiesBinarySensorResponse{ObjectId: "mic_muted",
			Key: esphomeMicMutedKey, Name: "Microphone Muted", Icon: "mdi:microphone-off"}}
		e.muted.Set(muted)
		e.entries = append(e.entries, e.muted)
	}
	esphomeOnEcho = e
}

// applyESPHome starts or stops the server, as the config says.
func applyESPHome(on bool) {
	e := esphomeOnEcho
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case on && e.srv == nil:
		if err := firewall.Open(esphome.DefaultPort); err != nil {
			log.Printf("[esphome] could not open port %d: %v", esphome.DefaultPort, err)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", esphome.DefaultPort))
		if err != nil {
			firewall.Close(esphome.DefaultPort)
			log.Printf("[esphome] not started: %v", err)
			return
		}
		e.srv = esphome.New(e.info, e.entries...)
		e.stop = make(chan struct{})
		go e.srv.Serve(ln)
		go e.poll(e.stop)
		log.Printf("[esphome] serving Home Assistant on %d as %s (%s)", esphome.DefaultPort, e.info.Name, e.info.MAC)
	case !on && e.srv != nil:
		close(e.stop)
		e.srv.Close()
		e.srv = nil
		firewall.Close(esphome.DefaultPort)
		log.Printf("[esphome] stopped")
	}
}

// poll reads the sensors until stopped.
func (e *esphomeEcho) poll(stop chan struct{}) {
	t := time.NewTicker(esphomePoll)
	defer t.Stop()
	for {
		if e.lux != nil {
			if v := als.Lux(); v != nil {
				e.lux.Set(float32(*v))
			} else {
				e.lux.Clear()
			}
		}
		if e.cpu != nil {
			cpuC, maxC, _ := thermals()
			setTemp(e.cpu, cpuC)
			setTemp(e.maxT, maxC)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func setTemp(s *esphome.Sensor, v *float64) {
	if v == nil {
		s.Clear()
		return
	}
	s.Set(float32(*v))
}

// esphomeMuted reports the microphone mute.
func esphomeMuted(muted bool) {
	if e := esphomeOnEcho; e != nil && e.muted != nil {
		e.muted.Set(muted)
	}
}

// esphomeButton reports an action button release: a hold is the "long"
// event, as the controller fires it (a tap is the voice press, not an
// event, as with the controller's default).
func esphomeButton(heldMs int) {
	e := esphomeOnEcho
	if e == nil || heldMs < incubatorHoldMs {
		return
	}
	e.mu.Lock()
	srv := e.srv
	e.mu.Unlock()
	if srv != nil {
		srv.Fire(esphomeEventKey, "long")
	}
}
