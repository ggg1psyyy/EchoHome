// Command esphome_interop serves internal/esphome with sample entities on
// 127.0.0.1, for check.py to drive with aioesphomeapi (Home Assistant's own
// ESPHome client): the proof that the Echo's server speaks what HA speaks.
//
//	go run ./tools/esphome_interop 16053 [base64 key | provision]   (from device/)
//
// then check.py (no key, or that key), or provision.py (provision: no key,
// set by the client as Home Assistant sets it).
package main

import (
	"encoding/base64"
	"log"
	"net"
	"os"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
)

func main() {
	port := "16053"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	lux := &esphome.Sensor{Desc: &api.ListEntitiesSensorResponse{ObjectId: "ambient_light", Key: 3, Name: "Ambient Light",
		UnitOfMeasurement: "lx", AccuracyDecimals: 0, DeviceClass: "illuminance",
		StateClass: api.SensorStateClass_STATE_CLASS_MEASUREMENT}}
	muted := &esphome.BinarySensor{Desc: &api.ListEntitiesBinarySensorResponse{ObjectId: "mic_muted", Key: 4,
		Name: "Microphone Muted", Icon: "mdi:microphone-off"}}
	btn := &esphome.Event{Desc: &api.ListEntitiesEventResponse{ObjectId: "action_button", Key: 2, Name: "Action Button",
		DeviceClass: "button", EventTypes: []string{"long", "single", "double", "triple"}}}
	s := esphome.New(esphome.DeviceInfo{Name: "echohome-interop", FriendlyName: "EchoHome interop",
		MAC: "02:EC:00:00:00:99", Manufacturer: "EchoHome", Model: "biscuit",
		ProjectName: "EchoHome.biscuit", ProjectVersion: "interop"}, btn, lux, muted)
	s.SetTimeSink(func(t time.Time, tz string) {
		log.Printf("time from the client: %s (%s), %v off", t.UTC().Format(time.RFC3339), tz, time.Until(t).Round(time.Second))
	})
	if len(os.Args) > 2 && os.Args[2] == "provision" {
		s.SetKeyStore(func(psk []byte) error {
			log.Printf("kept a key of %d bytes", len(psk))
			return nil
		})
	} else if len(os.Args) > 2 {
		k, err := base64.StdEncoding.DecodeString(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		if err := s.SetKey(k); err != nil {
			log.Fatal(err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		// Once a client has subscribed: a reading, a mute, a press.
		for s.Clients() == 0 {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(700 * time.Millisecond)
		lux.Set(123)
		muted.Set(true)
		s.Fire(2, "long")
	}()
	log.Printf("serving on %s", ln.Addr())
	log.Fatal(s.Serve(ln))
}
