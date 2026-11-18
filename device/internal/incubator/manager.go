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
	// Monitor: the app may listen live (the Echo's opt-in switch). A change
	// restarts the satellite, so the app reconnects and reads the new HELLO.
	Monitor bool
	// Standalone: the satellite outlives the controller link (the Echo's
	// "keep working without the controller" switch). The firmware keeps
	// these settings on disk and starts from them at boot.
	Standalone bool
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
	// Device is the Echo's microphone and speaker for calls (nil: presence
	// only).
	Device Device
	// Announce puts the satellite on the network over mDNS, as the app finds
	// its satellites (`_voice-incubator._tcp`, TXT mac and protocol), and
	// returns what withdraws it; nil announces nothing (the controller did,
	// before incubator_mdns). Called once the satellite listens.
	Announce func(name, mac string, port int) (withdraw func(), err error)

	mu       sync.Mutex
	s        *Server
	want     Settings
	withdraw func() // the mDNS announcement, while up
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
	s, err := Listen(Config{Name: st.Name, MAC: st.MAC, Port: st.Port, Model: m.Model, Firmware: m.Firmware, Device: m.Device, Monitor: st.Monitor}, m.Send)
	if err != nil {
		m.Firewall.Close(st.Port)
		log.Printf("[incubator] satellite not started: %v", err)
		m.Send(State{Listening: false, Port: st.Port, Error: err.Error()})
		return
	}
	m.s, m.want = s, st
	log.Printf("[incubator] satellite listening on %d as %s", st.Port, st.Name)
	s.Repaint() // idle, until the app connects
	if m.Announce != nil {
		w, err := m.Announce(st.Name, st.MAC, s.Port())
		if err != nil {
			log.Printf("[incubator] mDNS announcement failed: %v", err)
		} else {
			m.withdraw = w
			log.Printf("[incubator] announced as %s._voice-incubator._tcp", st.Name)
		}
	}
}

// LinkDown stops the satellite with the controller link; the controller's
// next message, sent on connect, starts it again.
func (m *Manager) LinkDown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s != nil && m.want.Standalone {
		log.Printf("[incubator] controller link down: the satellite keeps running (standalone)")
		return
	}
	m.stopLocked("controller link down")
}

// Repaint paints the satellite's ring again.
func (m *Manager) Repaint() {
	if s := m.server(); s != nil {
		s.Repaint()
	}
}

// Want is what the satellite was last asked to run as (zero when it does
// not run).
func (m *Manager) Want() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s == nil {
		return Settings{}
	}
	return m.want
}

// Serving says whether a standalone satellite has its app connected: the
// Echo then needs no controller to be of use.
func (m *Manager) Serving() bool {
	m.mu.Lock()
	s, standalone := m.s, m.want.Standalone
	m.mu.Unlock()
	return s != nil && standalone && s.State().AppConnected
}

func (m *Manager) stopLocked(why string) {
	if m.s == nil {
		return
	}
	port := m.s.Port()
	if m.withdraw != nil {
		// Off the network first: an app finding it now would be refused.
		m.withdraw()
		m.withdraw = nil
	}
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

func (m *Manager) server() *Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

// Press passes the action button to the satellite; false when none runs,
// so the caller knows nothing acted on it.
func (m *Manager) Press() bool {
	s := m.server()
	if s == nil {
		return false
	}
	s.Press()
	return true
}

// SetMuted passes on the Echo's mute: a call ends, and the satellite's
// microphone stays closed until it is unmuted.
func (m *Manager) SetMuted(muted bool) {
	if s := m.server(); s != nil {
		s.SetMuted(muted)
	}
}

// Drained passes on the speaker's report that a satellite stream played out.
func (m *Manager) Drained() {
	if s := m.server(); s != nil {
		s.Drained()
	}
}

// WakeModels is how many wake word models the satellite's app has pushed:
// while any are loaded the Echo's own wake word stands down for them.
func (m *Manager) WakeModels() int {
	if s := m.server(); s != nil {
		return s.WakeModels()
	}
	return 0
}

// InCall says whether a call holds the microphone.
func (m *Manager) InCall() bool {
	s := m.server()
	return s != nil && s.InCall()
}
