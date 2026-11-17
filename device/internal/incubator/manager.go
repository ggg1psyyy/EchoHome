package incubator

import (
	"log"
	"sync"
)

// Settings is what the controller's incubator_satellite message asks for.
type Settings struct {
	Enabled bool
	Name    string
	MAC     string
	Port    int
}

// Firewall opens and closes an inbound TCP port (internal/firewall on the
// Echo; a stand-in in tests).
type Firewall interface {
	Open(port int) error
	Close(port int)
}

// Manager runs the satellite as the controller's messages and the control
// link say: started on a message that enables it, restarted when the
// identity or port changes, kept as it is (app and all) on a repeated
// message, stopped on one that disables it and on link loss. Every change of
// state goes to Send, which the firmware forwards as incubator_state.
type Manager struct {
	Firewall Firewall
	Send     func(State)
	Model    string
	Firmware string

	mu   sync.Mutex
	s    *Server
	want Settings
}

func (m *Manager) port(st Settings) int {
	if st.Port <= 0 || st.Port > 65535 {
		return DefaultPort
	}
	return st.Port
}

// Apply acts on an incubator_satellite message.
func (m *Manager) Apply(st Settings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.Port = m.port(st)
	if m.s != nil && (!st.Enabled || st != m.want) {
		m.stopLocked("reconfigured")
	}
	if !st.Enabled {
		// Also clears a rule left by a firmware that died with it on.
		m.Firewall.Close(st.Port)
		m.Send(State{Listening: false, Port: st.Port})
		return
	}
	if m.s != nil {
		// The same again (the controller resends on every connect): keep the
		// app, and say what is true now.
		m.s.Resend()
		return
	}
	// Open before listening: the controller advertises the Echo once it hears
	// it listens, and the app may not retry a refused dial. Logged, not fatal:
	// FireOS has no filter to open.
	if err := m.Firewall.Open(st.Port); err != nil {
		log.Printf("[incubator] could not open port %d: %v", st.Port, err)
	}
	s, err := Listen(Config{Name: st.Name, MAC: st.MAC, Port: st.Port, Model: m.Model, Firmware: m.Firmware}, m.Send)
	if err != nil {
		m.Firewall.Close(st.Port)
		log.Printf("[incubator] satellite not started: %v", err)
		m.Send(State{Listening: false, Port: st.Port, Error: err.Error()})
		return
	}
	m.s, m.want = s, st
	log.Printf("[incubator] satellite listening on %d as %s", st.Port, st.Name)
}

// LinkDown stops the satellite with the controller link; the controller's
// next message, sent on connect, starts it again.
func (m *Manager) LinkDown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked("controller link down")
}

func (m *Manager) stopLocked(why string) {
	if m.s == nil {
		return
	}
	port := m.s.Port()
	m.s.Close()
	m.s = nil
	m.Firewall.Close(port)
	log.Printf("[incubator] satellite stopped: %s", why)
}

// Running reports whether the satellite is up (for tests and logs).
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s != nil
}
