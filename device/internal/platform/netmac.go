package platform

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WiFiMAC is the factory MAC address of the device's WiFi interface, as
// "AA:BB:CC:DD:EE:FF", or "" when none can be read. It is what an ESPHome
// device reports, and what Home Assistant matches DHCP leases and a router
// integration's device list against (its dhcp integration's
// registered_devices, by the device registry's MAC connection).
//
// Found by what an interface IS, never by its name or number (CLAUDE.md):
// the kernel's own facts in sysfs, read under root ("" for the live system).
//
//   - wireless: /sys/class/net/<if>/wireless or phy80211 exists;
//   - permanent: addr_assign_type 0 (NET_ADDR_PERM), the address the chip
//     came with, never a random or set one;
//   - a globally unique unicast address: neither the multicast bit nor the
//     locally administered bit set in the first byte.
//
// Of those, the interface carrying the IPv4 default route wins
// (/proc/net/route), then the first by name, so the answer is the same on
// every boot.
func WiFiMAC(root string) string {
	dir := filepath.Join(root, "sys", "class", "net")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	def := defaultRouteIface(root)
	var found []string
	byIface := map[string]string{}
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(dir, name)
		if !exists(filepath.Join(p, "wireless")) && !exists(filepath.Join(p, "phy80211")) {
			continue
		}
		if readTrim(filepath.Join(p, "addr_assign_type")) != "0" {
			continue
		}
		mac := factoryMAC(readTrim(filepath.Join(p, "address")))
		if mac == "" {
			continue
		}
		found = append(found, name)
		byIface[name] = mac
	}
	if len(found) == 0 {
		return ""
	}
	if mac, ok := byIface[def]; ok {
		return mac
	}
	sort.Strings(found)
	return byIface[found[0]]
}

// factoryMAC normalises a 48-bit address and refuses one that is not a
// globally unique unicast address (IEEE 802: bit 0 of the first octet is
// group, bit 1 local).
func factoryMAC(s string) string {
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return ""
	}
	if hw[0]&0x03 != 0 {
		return ""
	}
	zero := true
	for _, b := range hw {
		if b != 0 {
			zero = false
		}
	}
	if zero {
		return ""
	}
	return strings.ToUpper(hw.String())
}

// defaultRouteIface is the interface of the IPv4 default route in
// /proc/net/route (destination 00000000), or "".
func defaultRouteIface(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "proc", "net", "route"))
	if err != nil {
		return ""
	}
	for i, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 2 {
			continue
		}
		if f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
