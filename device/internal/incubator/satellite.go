package incubator

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Config is what a satellite says of itself. Name and MAC are the identity
// the app keys it on, so the controller hands over the ones the app already
// knows (the Echo's ESPHome node name and stored MAC); a new pair would make
// the Echo a new satellite to the app, losing its settings and HA link.
type Config struct {
	Name     string
	MAC      string
	Port     int
	Model    string
	Firmware string
	// Device is the Echo's microphone and speaker for calls. Nil is
	// presence only: no calls, and the app's frames are read and dropped.
	Device Device
}

// State is what the satellite reports to the controller, which advertises
// the Echo on the app's network only while Listening.
type State struct {
	Listening    bool `json:"listening"`
	Port         int  `json:"port"`
	AppConnected bool `json:"appConnected"`
	// InCall: the microphone is going to the app now. listen_state cannot
	// say so (it is "local" throughout), so this does.
	InCall bool   `json:"inCall,omitempty"`
	Error  string `json:"error,omitempty"`
}

// keepAlive matters: an app host that loses power sends no FIN, and the
// satellite serves one app at a time, so a dead one must be noticed in
// seconds, not the kernel's default hours: probes after 10 s idle, every 5 s,
// three missed and the connection is dropped.
var keepAlive = net.KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}

// Server is a satellite's listener: one app at a time, HELLO first. With a
// Device it takes calls started by the button (Press) and plays the app's
// audio; it announces no wake word, so the app pushes no models.
type Server struct {
	cfg     Config
	ln      net.Listener
	onState func(State)

	mu     sync.Mutex // guards app, closed and state, and orders reports
	app    net.Conn
	sess   *session
	closed bool
	state  State
	wg     sync.WaitGroup
}

// Listen starts the satellite on cfg.Port (0: any free port, for tests).
// onState is called on every change, in order, with the server's lock held:
// it must not call back into the Server.
func Listen(cfg Config, onState func(State)) (*Server, error) {
	if cfg.Name == "" || cfg.MAC == "" {
		return nil, errors.New("incubator: a satellite needs a name and a MAC")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, ln: ln, onState: onState}
	s.cfg.Port = ln.Addr().(*net.TCPAddr).Port
	s.mu.Lock()
	s.setLocked(State{Listening: true, Port: s.cfg.Port})
	s.mu.Unlock()
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Port is where it listens.
func (s *Server) Port() int { return s.cfg.Port }

// State is what it last reported.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Resend reports the current state again, under the same lock as every
// change, so it cannot overtake one (a repeated controller message).
func (s *Server) Resend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(s.state)
}

// setLocked records and reports a state. Called with mu held, so reports
// leave in the order the changes happened: a closing connection's
// "disconnected" can never arrive after the next one's "connected".
func (s *Server) setLocked(st State) {
	s.state = st
	if s.onState != nil {
		s.onState(st)
	}
}

// Close stops listening and drops the app, if one is connected.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	app := s.app
	s.mu.Unlock()
	s.ln.Close()
	if app != nil {
		app.Close()
	}
	s.wg.Wait()
	s.mu.Lock()
	s.setLocked(State{Listening: false, Port: s.cfg.Port})
	s.mu.Unlock()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of descriptors, say: still listening, so try again shortly
			// rather than go deaf while reporting that it listens.
			log.Printf("[incubator] accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		busy := s.app != nil || s.closed
		if !busy {
			s.app = c
		}
		s.mu.Unlock()
		if busy {
			// One app at a time: a second connection is refused.
			c.Close()
			continue
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetKeepAliveConfig(keepAlive)
		}
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *Server) hello() HelloInfo {
	h := NewHello(s.cfg.Name, s.cfg.MAC)
	h.Model, h.Firmware = s.cfg.Model, s.cfg.Firmware
	return h
}

// Press is the action button, passed on by the controller as a press (not
// a hold, not an event). Without an app connected it does nothing.
func (s *Server) Press() {
	if sess := s.session(); sess != nil {
		sess.press()
	}
}

// Cancel ends a call the Echo must end itself (mute).
func (s *Server) Cancel() {
	if sess := s.session(); sess != nil {
		sess.cancel()
	}
}

// Drained is the speaker's report that the stream the satellite last ended
// has played out.
func (s *Server) Drained() {
	if sess := s.session(); sess != nil {
		sess.playbackDrained()
	}
}

// InCall says whether the microphone is going to the app.
func (s *Server) InCall() bool {
	sess := s.session()
	return sess != nil && sess.listening()
}

func (s *Server) session() *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess
}

// callChanged reports the call state; never called with a session's lock.
// Read under the server's lock (server, then session: never the other way
// round), so a late report cannot overwrite a newer one with a stale value.
func (s *Server) callChanged(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := sess.listening()
	if s.sess == sess && !s.closed && s.state.InCall != in {
		st := s.state
		st.InCall = in
		s.setLocked(st)
	}
}

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	var sess *session
	defer func() {
		if sess != nil {
			sess.close()
		}
		c.Close()
		s.mu.Lock()
		s.app, s.sess = nil, nil
		if !s.closed {
			s.setLocked(State{Listening: true, Port: s.cfg.Port})
		}
		s.mu.Unlock()
	}()
	if err := WriteJSON(c, Hello, s.hello()); err != nil {
		return
	}
	log.Printf("[incubator] app connected from %s", c.RemoteAddr())
	if s.cfg.Device != nil {
		sess = newSession(c, s.cfg.Device, nil)
		sess.report = func() { s.callChanged(sess) }
	}
	s.mu.Lock()
	if !s.closed {
		s.sess = sess
		s.setLocked(State{Listening: true, Port: s.cfg.Port, AppConnected: true})
	}
	s.mu.Unlock()
	r := NewReader(c)
	for {
		m, p, err := r.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("[incubator] app connection: %v", err)
			}
			log.Printf("[incubator] app disconnected")
			return
		}
		if !m.FromApp() {
			// Not a message an app sends: logged and ignored, as the spec says
			// of an unknown type.
			log.Printf("[incubator] ignored message %#x", byte(m))
			continue
		}
		if sess != nil {
			if err := sess.handle(m, p); err != nil {
				log.Printf("[incubator] %#x from the app: %v", byte(m), err)
				return
			}
		}
	}
}
