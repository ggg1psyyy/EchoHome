package esphome

import (
	"strings"
	"testing"
)

// The keys ESPHome's mdns_component.cpp announces on _esphomelib; anything
// else would be a record of our own invention.
var esphomeTXTKeys = map[string]bool{
	"friendly_name": true, "version": true, "config_hash": true, "mac": true,
	"platform": true, "board": true, "network": true, "api_encryption": true,
	"api_encryption_supported": true, "api_provisioning": true, "ota_signed": true,
	"project_name": true, "project_version": true, "package_import_url": true,
}

func txtMap(t *testing.T, txt []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, r := range txt {
		// RFC 6763 §6.1: each string is at most 255 bytes; §6.4: the key is
		// what precedes the first "=", printable ASCII, and appears once.
		if len(r) > 255 {
			t.Fatalf("TXT string over 255 bytes: %q", r)
		}
		k, v, ok := strings.Cut(r, "=")
		if !ok || k == "" {
			t.Fatalf("TXT string without a key: %q", r)
		}
		for _, c := range k {
			if c < 0x20 || c > 0x7e {
				t.Fatalf("TXT key %q is not printable ASCII", k)
			}
		}
		if _, dup := m[k]; dup {
			t.Fatalf("TXT key %q twice", k)
		}
		if !esphomeTXTKeys[k] {
			t.Fatalf("TXT key %q is not one ESPHome announces", k)
		}
		m[k] = v
	}
	return m
}

// homeassistant.helpers.device_registry.format_mac on a 12-hex-digit value:
// lowercase, colon-separated pairs.
func haFormatMAC(mac string) string {
	var b strings.Builder
	for i := 0; i < len(mac); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(strings.ToLower(mac[i : i+2]))
	}
	return b.String()
}

func TestTheAnnouncementCarriesWhatHomeAssistantReads(t *testing.T) {
	s := New(DeviceInfo{Name: "echohome-11743300lq", FriendlyName: "EchoHome 00LQ",
		MAC: "02:EC:1A:2B:3C:4D", ProjectName: "EchoHome.biscuit", ProjectVersion: "v3.0.0-ea.1"})
	m := txtMap(t, s.MDNSText("biscuit"))
	// The config entry's unique id: the same as the API's MAC, so an Echo
	// added by address is the same entry when it is discovered.
	if got, want := haFormatMAC(m["mac"]), strings.ToLower(s.info.MAC); got != want {
		t.Fatalf("mac %q formats to %q, the API says %q", m["mac"], got, want)
	}
	if len(m["mac"]) != 12 {
		t.Fatalf("mac %q is not 12 hex digits", m["mac"])
	}
	if m["friendly_name"] != "EchoHome 00LQ" || m["board"] != "biscuit" || m["network"] != "wifi" {
		t.Fatalf("records: %v", m)
	}
	if m["project_name"] != "EchoHome.biscuit" || m["project_version"] != "v3.0.0-ea.1" {
		t.Fatalf("project: %v", m)
	}
	// No key store and no key: nothing about encryption is claimed.
	if _, ok := m["api_encryption"]; ok {
		t.Fatal("api_encryption without a key")
	}
	if _, ok := m["api_encryption_supported"]; ok {
		t.Fatal("api_encryption_supported without a key store")
	}
}

func TestTheEncryptionRecordFollowsTheKey(t *testing.T) {
	s := New(DeviceInfo{Name: "n", MAC: "02:EC:00:00:00:01"})
	s.SetKeyStore(func([]byte) error { return nil })
	m := txtMap(t, s.MDNSText(""))
	// Provisionable: supported, not required, so Home Assistant connects in
	// plaintext and sets the key; never zero-psk, which this server lacks.
	if m["api_encryption_supported"] != NoiseProtocol {
		t.Fatalf("unprovisioned: %v", m)
	}
	if _, ok := m["api_encryption"]; ok {
		t.Fatal("unprovisioned, yet api_encryption: Home Assistant would ask for a key nobody has")
	}
	if _, ok := m["api_provisioning"]; ok {
		t.Fatal("api_provisioning promises a zero-PSK handshake this server does not do")
	}
	key := make([]byte, KeySize)
	key[0] = 1
	if err := s.SetKey(key); err != nil {
		t.Fatal(err)
	}
	m = txtMap(t, s.MDNSText(""))
	if m["api_encryption"] != NoiseProtocol {
		t.Fatalf("with a key: %v", m)
	}
	if _, ok := m["api_encryption_supported"]; ok {
		t.Fatal("both encryption records")
	}
	if _, ok := m["board"]; ok {
		t.Fatal("an empty board announced")
	}
}
