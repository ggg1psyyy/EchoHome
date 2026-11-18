package esphome

import "strings"

// The native API's mDNS service, as ESPHome announces it and Home
// Assistant's ESPHome integration browses for it (its manifest's zeroconf
// "_esphomelib._tcp.local."): the instance and the host are the node name,
// the port the API's.
//
// What Home Assistant reads (homeassistant/components/esphome/config_flow.py,
// async_step_zeroconf, 2026.11.1): "mac" (required: without it the flow
// aborts with mdns_missing_mac; it is the config entry's unique id, so an
// Echo already added by address is recognised and its address updated),
// "friendly_name" (the name it offers), "api_encryption" (present: a key is
// needed). The host name, less ".local.", is the device name it expects the
// API's HelloResponse to carry, so it must be the node name.
//
// The keys and values are ESPHome's own (esphome/components/mdns/
// mdns_component.cpp). Left out: "version" and "config_hash" (an ESPHome
// build's, which this is not), "platform" (ESPHome's chip families only),
// and "api_provisioning" = "zero-psk", which promises a Noise handshake on
// an all-zero key that this server does not do: unprovisioned it is
// plaintext, and Home Assistant sets the key over that
// (api_encryption_provisionable).
const ServiceType = "_esphomelib._tcp"

// NoiseProtocol is ESPHome's value for its encryption TXT records.
const NoiseProtocol = "Noise_NNpsk0_25519_ChaChaPoly_SHA256"

// MDNSText is the service's TXT records, in ESPHome's order: "api_encryption"
// once a key is in use, "api_encryption_supported" while one can be set.
func (s *Server) MDNSText(board string) []string {
	i := s.info
	var txt []string
	if n := s.FriendlyName(); n != "" {
		txt = append(txt, "friendly_name="+n)
	}
	txt = append(txt, "mac="+strings.ToLower(strings.ReplaceAll(i.MAC, ":", "")))
	if board != "" {
		txt = append(txt, "board="+board)
	}
	txt = append(txt, "network=wifi")
	switch {
	case s.HasKey():
		txt = append(txt, "api_encryption="+NoiseProtocol)
	case s.keyStore != nil:
		txt = append(txt, "api_encryption_supported="+NoiseProtocol)
	}
	if i.ProjectName != "" {
		txt = append(txt, "project_name="+i.ProjectName, "project_version="+i.ProjectVersion)
	}
	return txt
}
