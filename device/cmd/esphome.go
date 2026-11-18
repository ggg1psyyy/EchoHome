package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wilbowes/EchoMuse/internal/atomicfile"
	"github.com/wilbowes/EchoMuse/internal/bindings/als"
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/config"
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
// controller's entry. The entities' keys and object ids are the
// controller's (em_esphome.py, test-pinned), but HA keys its entities on
// the device's MAC too: for them to carry over, the day the Echo takes over
// it must also adopt the controller's MAC (em_db.get_esphome_mac).
//
// The switch is kept on the Echo (esphomeOnPath), so the server starts at
// boot with no controller.
//
// Always encrypted (ESPHome's Noise transport), as a Voice PE is: the key is
// made on the Echo the first time it is needed and kept there
// (esphomeKeyPath), never changed (Home Assistant stores it: a new key would
// lock its entry out), and handed out only on the controller's
// esphome_key_request, for the dashboard to show an admin. Never logged.
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

// esphomeOnPath, present, means the server is on: read at boot, so it
// serves with no controller; written only when the switch changes.
const esphomeOnPath = "/data/local/etc/echomuse/esphome.on"

// esphomeKeyPath is the server's key, base64 as Home Assistant takes it.
const esphomeKeyPath = "/data/local/etc/echomuse/esphome.key"

var esphomeKeyMu sync.Mutex

// esphomeKey is the server's key, made and kept on first use. An error
// when it cannot be kept: a key that changed at the next boot would lock
// Home Assistant out, so the server does not start on one.
func esphomeKey() ([]byte, string, error) {
	esphomeKeyMu.Lock()
	defer esphomeKeyMu.Unlock()
	if b, err := os.ReadFile(esphomeKeyPath); err == nil {
		s := strings.TrimSpace(string(b))
		if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == esphome.KeySize {
			return k, s, nil
		}
		// Unreadable: kept aside rather than overwritten, and a new one made.
		atomicfile.Write(esphomeKeyPath+".bad", b, 0o600)
		log.Printf("[esphome] the saved key is unreadable: kept as %s.bad, a new one made", esphomeKeyPath)
	}
	k := make([]byte, esphome.KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, "", err
	}
	s := base64.StdEncoding.EncodeToString(k)
	if err := atomicfile.Write(esphomeKeyPath, []byte(s+"\n"), 0o600); err != nil {
		return nil, "", err
	}
	log.Printf("[esphome] encryption key made")
	return k, s, nil
}

// esphomeKeyReply answers esphome_key_request.
func esphomeKeyReply() map[string]any {
	_, s, err := esphomeKey()
	if err != nil {
		log.Printf("[esphome] no key: %v", err)
		return nil
	}
	return map[string]any{"key": s}
}

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
	if _, err := os.Stat(esphomeOnPath); err == nil {
		// Seeded into the config too: a controller that does not know the
		// key leaves it as it is, rather than turning the server off.
		on := true
		config.Get().Apply(config.ConfigMessage{ESPHomeServer: &on})
		applyESPHome(true)
	}
}

// keepESPHomeOn records the switch for the next boot.
func keepESPHomeOn(on bool) {
	_, err := os.Stat(esphomeOnPath)
	switch had := err == nil; {
	case on && !had:
		err = atomicfile.Write(esphomeOnPath, []byte("1\n"), 0o600)
	case !on && had:
		err = os.Remove(esphomeOnPath)
	default:
		return
	}
	if err != nil {
		log.Printf("[esphome] could not keep the switch: %v", err)
	}
}

// applyESPHome starts or stops the server, as the config says.
func applyESPHome(on bool) {
	e := esphomeOnEcho
	if e == nil {
		return
	}
	keepESPHomeOn(on)
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case on && e.srv == nil:
		key, _, err := esphomeKey()
		if err != nil {
			log.Printf("[esphome] not started, no key could be kept: %v", err)
			return
		}
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
		e.srv.SetKey(key)
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
