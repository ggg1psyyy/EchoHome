package esphome

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"google.golang.org/protobuf/proto"
)

// The API version the server speaks: the controller's
// (esphome/satellite_server.py API_VERSION_MAJOR, _MINOR).
const (
	APIVersionMajor = 1
	APIVersionMinor = 10
)

// DefaultPort is ESPHome's.
const DefaultPort = 6053

var (
	// readIdle: Home Assistant pings every 20 s or so; a client silent this
	// long is gone (a host that lost power sends no FIN).
	readIdle = 90 * time.Second
	// writeDeadline bounds one write to a client.
	writeDeadline = 10 * time.Second
)

// outQueue is how many frames may wait for a client. A client that lets it
// fill is too slow to follow the Echo and is dropped: it must never hold
// up the state of the others.
const outQueue = 128

// DeviceInfo is what the server says of the Echo (DeviceInfoResponse).
type DeviceInfo struct {
	// Name is the node name (unique on the network), FriendlyName what HA
	// shows.
	Name, FriendlyName string
	// MAC is what HA keys the device on.
	MAC                         string
	Manufacturer, Model         string
	ProjectName, ProjectVersion string
}

// Server serves Home Assistant (and any other API client) the Echo's
// entities. Safe for concurrent use.
type Server struct {
	info     DeviceInfo
	entities []Entity
	byKey    map[uint32]Entity

	mu     sync.Mutex
	ln     net.Listener
	conns  map[*conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New is a server for these entities.
func New(info DeviceInfo, entities ...Entity) *Server {
	s := &Server{info: info, entities: entities, byKey: map[uint32]Entity{}, conns: map[*conn]struct{}{}}
	for _, e := range entities {
		s.byKey[e.Key()] = e
		e.bind(s.publish)
	}
	return s
}

// Serve accepts clients on ln until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("[esphome] accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		cn := &conn{s: s, c: c, out: make(chan []byte, outQueue), done: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			continue
		}
		s.conns[cn] = struct{}{}
		s.wg.Add(2)
		s.mu.Unlock()
		go cn.write()
		go cn.read()
	}
}

// Close stops listening and drops every client.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ln := s.ln
	all := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, c := range all {
		c.close()
	}
	s.wg.Wait()
}

// Clients is how many are connected.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// publish sends an entity's new state to every client subscribed to states.
func (s *Server) publish(e Entity) {
	if m := e.State(); m != nil {
		s.broadcast(m)
	}
}

// Fire sends an event entity's event (EventResponse) to subscribed clients.
func (s *Server) Fire(key uint32, eventType string) {
	if _, ok := s.byKey[key].(*Event); !ok {
		return
	}
	s.broadcast(&api.EventResponse{Key: key, EventType: eventType})
}

func (s *Server) broadcast(m proto.Message) {
	frame, err := frameOf(m)
	if err != nil {
		log.Printf("[esphome] %v", err)
		return
	}
	s.mu.Lock()
	all := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		if c.subscribed.Load() {
			all = append(all, c)
		}
	}
	s.mu.Unlock()
	for _, c := range all {
		c.send(frame)
	}
}

func frameOf(m proto.Message) ([]byte, error) {
	typ, b, err := Encode(m)
	if err != nil {
		return nil, err
	}
	return AppendFrame(nil, typ, b), nil
}

// conn is one client: a reader that answers it, and a writer fed through a
// bounded queue.
type conn struct {
	s          *Server
	c          net.Conn
	out        chan []byte
	done       chan struct{}
	once       sync.Once
	subscribed atomic.Bool
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.c.Close()
		c.s.mu.Lock()
		delete(c.s.conns, c)
		c.s.mu.Unlock()
	})
}

// send queues a frame, or drops the client if its queue is full.
func (c *conn) send(frame []byte) {
	select {
	case <-c.done:
	case c.out <- frame:
	default:
		log.Printf("[esphome] %s too slow to follow: dropped", c.c.RemoteAddr())
		c.close()
	}
}

func (c *conn) reply(m proto.Message) {
	frame, err := frameOf(m)
	if err != nil {
		log.Printf("[esphome] %v", err)
		return
	}
	c.send(frame)
}

func (c *conn) write() {
	defer c.s.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case f := <-c.out:
			c.c.SetWriteDeadline(time.Now().Add(writeDeadline))
			if _, err := c.c.Write(f); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *conn) read() {
	defer c.s.wg.Done()
	defer c.close()
	r := bufio.NewReader(c.c)
	for {
		c.c.SetReadDeadline(time.Now().Add(readIdle))
		typ, payload, err := ReadFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("[esphome] %s: %v", c.c.RemoteAddr(), err)
			}
			return
		}
		m, known, err := Decode(typ, payload)
		if err != nil {
			log.Printf("[esphome] %s: message %d: %v", c.c.RemoteAddr(), typ, err)
			return
		}
		if !known {
			continue // a type this API does not define: ignored, as ESPHome does
		}
		if !c.handle(m) {
			return
		}
	}
}

// handle answers one message; false ends the connection.
func (c *conn) handle(m proto.Message) bool {
	s := c.s
	switch m := m.(type) {
	case *api.HelloRequest:
		log.Printf("[esphome] %s: hello from %q (API %d.%d)", c.c.RemoteAddr(), m.ClientInfo, m.ApiVersionMajor, m.ApiVersionMinor)
		c.reply(&api.HelloResponse{ApiVersionMajor: APIVersionMajor, ApiVersionMinor: APIVersionMinor,
			ServerInfo: s.info.Name, Name: s.info.Name})
	case *api.AuthenticationRequest:
		// No password: acknowledged, as the controller does.
		c.reply(&api.AuthenticationResponse{})
	case *api.DeviceInfoRequest:
		i := s.info
		c.reply(&api.DeviceInfoResponse{Name: i.Name, FriendlyName: i.FriendlyName, MacAddress: i.MAC,
			Manufacturer: i.Manufacturer, Model: i.Model,
			// A dot is required: HA splits project_name on it unconditionally
			// (em_esphome.py, "session handoff finding #1").
			ProjectName: i.ProjectName, ProjectVersion: i.ProjectVersion})
	case *api.ListEntitiesRequest:
		for _, e := range s.entities {
			c.reply(e.Info())
		}
		c.reply(&api.ListEntitiesDoneResponse{})
	case *api.SubscribeStatesRequest:
		c.subscribed.Store(true)
		for _, e := range s.entities {
			if st := e.State(); st != nil {
				c.reply(st)
			}
		}
	case *api.PingRequest:
		c.reply(&api.PingResponse{})
	case *api.DisconnectRequest:
		c.reply(&api.DisconnectResponse{})
		// Let the reply go before the close.
		time.AfterFunc(100*time.Millisecond, c.close)
		return true
	case *api.DisconnectResponse:
		return false
	}
	return true
}
