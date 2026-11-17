package incubator

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeFirewall struct {
	mu  sync.Mutex
	log []string
}

func (f *fakeFirewall) Open(port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "open")
	return nil
}

func (f *fakeFirewall) Close(port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "close")
}

func (f *fakeFirewall) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// freePort is a port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func manager(t *testing.T) (*Manager, *fakeFirewall, *states) {
	fw, st := &fakeFirewall{}, &states{}
	m := &Manager{Firewall: fw, Send: func(s State) {
		// The firewall must be open by the time "listening" first goes out.
		if s.Listening && !st.sawListening() {
			if c := fw.calls(); len(c) == 0 || c[len(c)-1] != "open" {
				t.Error("reported listening before the port was opened")
			}
		}
		st.add(s)
	}, Model: "echomuse-biscuit", Firmware: "v9"}
	t.Cleanup(m.LinkDown)
	return m, fw, st
}

func TestApplyOpensThePortThenListens(t *testing.T) {
	m, fw, st := manager(t)
	p := freePort(t)
	m.Apply(Settings{Enabled: true, Name: "n", MAC: "02:ec:00:00:00:01", Port: p})
	if !m.Running() || !st.last().Listening || st.last().Port != p {
		t.Fatalf("not listening: %+v", st.last())
	}
	if c := fw.calls(); len(c) != 1 || c[0] != "open" {
		t.Fatalf("firewall %v", c)
	}
	c, r := dial(t, p)
	defer c.Close()
	if h := readHello(t, r); h["model"] != "echomuse-biscuit" || h["firmware"] != "v9" || h["mac"] != "02:ec:00:00:00:01" {
		t.Errorf("hello %v", h)
	}
}

func TestTheSameMessageAgainKeepsTheAppAndSaysItIsConnected(t *testing.T) {
	m, fw, st := manager(t)
	set := Settings{Enabled: true, Name: "n", MAC: "m", Port: freePort(t)}
	m.Apply(set)
	c, r := dial(t, set.Port)
	defer c.Close()
	readHello(t, r)
	waitFor(t, "connected", func() bool { return st.last().AppConnected })
	m.Apply(set) // the controller resends on every connect
	if !st.last().AppConnected {
		t.Fatalf("a repeated message reported the app gone: %+v", st.last())
	}
	if err := WriteJSON(c, Event, EventInfo{Type: "listening"}); err != nil {
		t.Fatalf("the app was dropped: %v", err)
	}
	if c := fw.calls(); len(c) != 1 {
		t.Errorf("firewall %v: a repeat must not reopen", c)
	}
}

func TestANewIdentityRestartsTheSatellite(t *testing.T) {
	m, _, _ := manager(t)
	p := freePort(t)
	m.Apply(Settings{Enabled: true, Name: "old", MAC: "m", Port: p})
	c, r := dial(t, p)
	defer c.Close()
	readHello(t, r)
	m.Apply(Settings{Enabled: true, Name: "new", MAC: "m", Port: p})
	if _, _, err := r.Read(); err == nil {
		t.Fatal("the app under the old name was kept")
	}
	c2, r2 := dial(t, p)
	defer c2.Close()
	if h := readHello(t, r2); h["name"] != "new" {
		t.Errorf("name %v", h["name"])
	}
}

func TestDisableAndLinkDownStopItAndCloseThePort(t *testing.T) {
	for _, how := range []string{"disable", "link down"} {
		t.Run(how, func(t *testing.T) {
			m, fw, st := manager(t)
			p := freePort(t)
			m.Apply(Settings{Enabled: true, Name: "n", MAC: "m", Port: p})
			c, r := dial(t, p)
			defer c.Close()
			readHello(t, r)
			if how == "disable" {
				m.Apply(Settings{Enabled: false, Port: p})
			} else {
				m.LinkDown()
			}
			if m.Running() || st.last().Listening {
				t.Fatalf("still running: %+v", st.last())
			}
			if _, _, err := r.Read(); err == nil {
				t.Fatal("the app is still connected")
			}
			if c := fw.calls(); c[len(c)-1] != "close" {
				t.Errorf("firewall %v", c)
			}
		})
	}
}

func TestAPortInUseIsReportedAndTheRuleRemoved(t *testing.T) {
	m, fw, st := manager(t)
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	m.Apply(Settings{Enabled: true, Name: "n", MAC: "m", Port: ln.Addr().(*net.TCPAddr).Port})
	if m.Running() || st.last().Listening || st.last().Error == "" {
		t.Fatalf("%+v", st.last())
	}
	if c := fw.calls(); len(c) != 2 || c[1] != "close" {
		t.Errorf("firewall %v", c)
	}
}

func TestAnOutOfRangePortIsTheDefault(t *testing.T) {
	m := &Manager{}
	for _, p := range []int{0, -1, 65536} {
		if got := m.port(Settings{Port: p}); got != DefaultPort {
			t.Errorf("%d -> %d", p, got)
		}
	}
	if errors.Is(nil, nil) && m.port(Settings{Port: 6056}) != 6056 {
		t.Error("a valid port was changed")
	}
}

// The Echo's switch for listening live: a flip either way restarts the
// satellite, so the app reconnects to a HELLO that says the new truth, and
// a stream going when it is turned off stops with the old connection.
func TestTheMonitorSwitchRestartsTheSatelliteBothWays(t *testing.T) {
	m, _, _ := manager(t)
	m.Device = &fakeEcho{}
	p := freePort(t)
	hello := func() (net.Conn, *Reader, map[string]any) {
		c, r := dial(t, p)
		t.Cleanup(func() { c.Close() })
		return c, r, readHello(t, r)
	}
	m.Apply(Settings{Enabled: true, Name: "n", MAC: "m", Port: p})
	_, r, h := hello()
	if _, ok := h["monitor"]; ok {
		t.Fatal("offered with the switch off")
	}
	m.Apply(Settings{Enabled: true, Name: "n", MAC: "m", Port: p, Monitor: true})
	if _, _, err := r.Read(); err == nil {
		t.Fatal("the app was kept across the switch")
	}
	c, r, h := hello()
	if h["monitor"] != true {
		t.Fatal("not offered with the switch on")
	}
	waitFor(t, "the session", func() bool { return m.server().session() != nil })
	WriteJSON(c, Monitor, MonitorInfo{Active: true})
	waitFor(t, "monitoring", func() bool { return m.server().session().monitoring() })
	m.Apply(Settings{Enabled: true, Name: "n", MAC: "m", Port: p}) // off mid-stream
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := r.Read(); err != nil {
			break // the stream went with the connection
		}
	}
	_, _, h = hello()
	if _, ok := h["monitor"]; ok {
		t.Fatal("still offered after the switch went off")
	}
}
