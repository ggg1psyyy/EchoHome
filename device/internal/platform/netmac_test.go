package platform

import (
	"os"
	"path/filepath"
	"testing"
)

type iface struct {
	name, mac, assign string
	wireless          bool
}

func fakeNet(t *testing.T, route string, ifs ...iface) string {
	t.Helper()
	root := t.TempDir()
	for _, i := range ifs {
		d := filepath.Join(root, "sys", "class", "net", i.name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "address"), []byte(i.mac+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "addr_assign_type"), []byte(i.assign+"\n"), 0o644)
		if i.wireless {
			os.MkdirAll(filepath.Join(d, "wireless"), 0o755)
		}
	}
	if route != "" {
		os.MkdirAll(filepath.Join(root, "proc", "net"), 0o755)
		os.WriteFile(filepath.Join(root, "proc", "net", "route"), []byte(route), 0o644)
	}
	return root
}

const routeHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

// 00LQ's interfaces as its kernel lists them (2026-11-19), the addresses
// replaced: wlan0 is the chip's own; ifb0/ifb1 are random and not wireless.
func TestTheEchosWiFiChipIsFoundByWhatItIs(t *testing.T) {
	root := fakeNet(t, routeHeader+"wlan0\t00000000\t0114A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
		iface{"ifb0", "f6:3a:11:0a:0b:0c", "1", false},
		iface{"ifb1", "9e:22:01:0d:0e:0f", "1", false},
		iface{"lo", "00:00:00:00:00:00", "0", false},
		iface{"wlan0", "fc:a1:83:12:34:56", "0", true},
	)
	if got := WiFiMAC(root); got != "FC:A1:83:12:34:56" {
		t.Fatalf("got %q", got)
	}
}

func TestOnlyAPermanentGloballyUniqueUnicastAddressCounts(t *testing.T) {
	for _, c := range []struct {
		mac, assign, want string
	}{
		{"fc:a1:83:12:34:56", "0", "FC:A1:83:12:34:56"},
		{"FC-A1-83-12-34-56", "0", "FC:A1:83:12:34:56"}, // IEEE 802 hyphen form
		{"fe:a1:83:12:34:56", "0", ""},                  // locally administered (bit 1)
		{"fd:a1:83:12:34:56", "0", ""},                  // group address (bit 0)
		{"02:ec:00:00:00:01", "0", ""},                  // the old made-up form
		{"00:00:00:00:00:00", "0", ""},
		{"fc:a1:83:12:34:56", "1", ""},       // random (NET_ADDR_RANDOM)
		{"fc:a1:83:12:34:56", "3", ""},       // set by userspace
		{"fc:a1:83:12:34:56", "", ""},        // unknown: not trusted
		{"fc:a1:83:12:34", "0", ""},          // not 48 bits
		{"00:00:00:00:fe:80:00:00", "0", ""}, // EUI-64
		{"not a mac", "0", ""},
	} {
		root := fakeNet(t, "", iface{"wlan0", c.mac, c.assign, true})
		if got := WiFiMAC(root); got != c.want {
			t.Errorf("%q assign %q: got %q, want %q", c.mac, c.assign, got, c.want)
		}
	}
}

func TestAWiredInterfaceIsNotTheWiFiMAC(t *testing.T) {
	root := fakeNet(t, "", iface{"eth0", "00:11:22:33:44:55", "0", false})
	if got := WiFiMAC(root); got != "" {
		t.Fatalf("got %q from a wired interface", got)
	}
}

func TestTheDefaultRouteWinsThenTheNameOrder(t *testing.T) {
	a := iface{"wlan0", "00:11:22:33:44:01", "0", true}
	b := iface{"wlan1", "00:11:22:33:44:02", "0", true}
	root := fakeNet(t, routeHeader+"wlan1\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n", a, b)
	if got := WiFiMAC(root); got != "00:11:22:33:44:02" {
		t.Fatalf("default route: got %q", got)
	}
	root = fakeNet(t, routeHeader+"wlan1\t0014A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n", b, a)
	if got := WiFiMAC(root); got != "00:11:22:33:44:01" {
		t.Fatalf("no default route: got %q", got)
	}
}

func TestNoSysfsMeansNoMAC(t *testing.T) {
	if got := WiFiMAC(t.TempDir()); got != "" {
		t.Fatalf("got %q", got)
	}
}
