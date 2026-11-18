package esphome

import (
	"bufio"
	"errors"
	"fmt"
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
	psk      []byte // nil: plaintext
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

// SetKey makes the server encrypted (ESPHome's Noise transport) with this
// 32-byte pre-shared key, refusing plaintext clients. Before Serve.
func (s *Server) SetKey(psk []byte) error {
	if len(psk) != KeySize {
		return fmt.Errorf("esphome: a key is %d bytes, not %d", KeySize, len(psk))
	}
	s.psk = append([]byte(nil), psk...)
	return nil
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
		cn := &conn{s: s, c: c, out: make(chan packet, outQueue), done: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			continue
		}
		s.conns[cn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go cn.run()
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
	p, err := packetOf(m)
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
		c.send(p)
	}
}

// packet is one message, encoded but not framed: each client's writer
// frames it, encrypting it for that client.
type packet struct {
	typ     uint32
	payload []byte
}

func packetOf(m proto.Message) (packet, error) {
	typ, b, err := Encode(m)
	return packet{typ, b}, err
}

// conn is one client: a reader that answers it, and a writer fed through a
// bounded queue.
type conn struct {
	s          *Server
	c          net.Conn
	out        chan packet
	noise      *noiseTransport // nil: plaintext
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

// send queues a message, or drops the client if its queue is full.
func (c *conn) send(p packet) {
	select {
	case <-c.done:
	case c.out <- p:
	default:
		log.Printf("[esphome] %s too slow to follow: dropped", c.c.RemoteAddr())
		c.close()
	}
}

func (c *conn) reply(m proto.Message) {
	p, err := packetOf(m)
	if err != nil {
		log.Printf("[esphome] %v", err)
		return
	}
	c.send(p)
}

// writeRaw writes bytes straight to the socket: the handshake's, before the
// writer runs.
func (c *conn) writeRaw(b []byte) error {
	c.c.SetWriteDeadline(time.Now().Add(writeDeadline))
	_, err := c.c.Write(b)
	return err
}

func (c *conn) write() {
	defer c.s.wg.Done()
	var buf []byte
	for {
		select {
		case <-c.done:
			return
		case p := <-c.out:
			// What else is queued goes in the same write.
			buf = buf[:0]
			for more := true; more; {
				var err error
				buf, err = c.frame(buf, p)
				if err != nil {
					log.Printf("[esphome] %v", err)
				}
				select {
				case p = <-c.out:
				default:
					more = false
				}
			}
			if err := c.writeRaw(buf); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *conn) frame(b []byte, p packet) ([]byte, error) {
	if c.noise != nil {
		return c.noise.appendMessage(b, p.typ, p.payload)
	}
	return AppendFrame(b, p.typ, p.payload), nil
}

func (c *conn) readMessage(r *bufio.Reader) (uint32, []byte, error) {
	if c.noise != nil {
		return c.noise.readMessage(r)
	}
	return ReadFrame(r)
}

// run serves one client: the handshake if the server has a key, then the
// writer and the reader.
func (c *conn) run() {
	defer c.s.wg.Done()
	defer c.close()
	r := bufio.NewReader(c.c)
	if c.s.psk != nil {
		c.c.SetReadDeadline(time.Now().Add(readIdle))
		t, err := handshake(r, c.writeRaw, c.s.psk, c.s.info.Name, c.s.info.MAC)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("[esphome] %s: %v", c.c.RemoteAddr(), err)
			}
			return
		}
		c.noise = t
	}
	c.s.wg.Add(1)
	go c.write()
	for {
		c.c.SetReadDeadline(time.Now().Add(readIdle))
		typ, payload, err := c.readMessage(r)
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
