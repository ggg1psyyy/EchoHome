package main

import (
	"encoding/hex"
	"strings"

	"github.com/wilbowes/EchoMuse/internal/bluetooth"
	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
)

// The Bluetooth proxy on the Echo's own ESPHome device: the passive scan the
// Echo already runs (internal/bluetooth, its emission gate and its yields
// for voice and music) goes to Home Assistant directly, as from an ESPHome
// bluetooth_proxy, so presence (Bermuda, Private BLE Device) keeps working
// with no controller. Passive only: no connection slots. On while the
// Bluetooth proxy setting is (bleProxyEnabled, the Echo's own: its HA
// switch, cmd/owned.go).

var bleRefs struct {
	scanner *bluetooth.Scanner
}

// esphomeBLEFollow sets the Echo's server's proxy flags from the setting.
func esphomeBLEFollow() {
	e := esphomeOnEcho
	if e == nil {
		return
	}
	e.mu.Lock()
	srv := e.srv
	e.mu.Unlock()
	if srv == nil {
		return
	}
	var flags uint32
	if bleProxyOn() {
		flags = esphome.BLEPassiveScan | esphome.BLERawAdvertisements
	}
	srv.SetBluetoothProxy(flags)
}

// esphomeAdverts hands one scanned batch to the Echo's server.
func esphomeAdverts(batch []bluetooth.Advert) {
	e := esphomeOnEcho
	if e == nil {
		return
	}
	e.mu.Lock()
	srv := e.srv
	e.mu.Unlock()
	if srv == nil {
		return
	}
	out := make([]*api.BluetoothLERawAdvertisement, 0, len(batch))
	for _, a := range batch {
		addr, ok := macToUint64(a.Addr)
		if !ok {
			continue
		}
		out = append(out, &api.BluetoothLERawAdvertisement{Address: addr, Rssi: int32(a.Rssi),
			AddressType: uint32(a.AddrType), Data: a.Data})
	}
	srv.Adverts(out)
}

// macToUint64 is "AA:BB:CC:DD:EE:FF" as the API's address, as the
// controller converts it (em_ble_proxy.forward_adverts).
func macToUint64(mac string) (uint64, bool) {
	b, err := hex.DecodeString(strings.ReplaceAll(mac, ":", ""))
	if err != nil || len(b) != 6 {
		return 0, false
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v, true
}
