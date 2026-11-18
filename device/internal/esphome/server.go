package esphome

import (
	"bufio"
	"encoding/base64"
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
	// keepaliveIdle: a client silent this long is pinged, as ESPHome's own
	// firmware does. Home Assistant pings only when it has heard nothing,
	// so a device that keeps sending states would otherwise never hear from
	// it (bench, 00LQ, 2026-11-18: dropped after HA went quiet).
	keepaliveIdle = 60 * time.Second
	// keepaliveTick is how often the writer looks.
	keepaliveTick = 5 * time.Second
	// timeRefresh is how often the time is asked again: ESPHome's
	// homeassistant time's default update interval.
	timeRefresh = 15 * time.Minute
	// readIdle: a client silent this long, pinged and not answering, is gone
	// (a host that lost power sends no FIN).
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
	info DeviceInfo
	// keyStore, set, makes the server provisionable (SetKeyStore).
	keyStore func(psk []byte) error
	// timeSink, set, takes Home Assistant's time (SetTimeSink).
	timeSink func(t time.Time, timezone string)

	keyMu    sync.Mutex
	psk      []byte // nil: no key
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
// 32-byte pre-shared key, refusing plaintext and the all-zero setup key.
func (s *Server) SetKey(psk []byte) error {
	if len(psk) != KeySize {
		return fmt.Errorf("esphome: a key is %d bytes, not %d", KeySize, len(psk))
	}
	if allZero(psk) {
		return errors.New("esphome: the all-zero key is no key")
	}
	s.keyMu.Lock()
	s.psk = append([]byte(nil), psk...)
	s.keyMu.Unlock()
	return nil
}

// SetKeyStore lets Home Assistant set the key, as it does on an ESPHome
// device with none in its YAML (HA's esphome manager.py): with no key the
// server says api_encryption_provisionable, accepts plaintext and the
// all-zero setup key, and takes HA's NoiseEncryptionSetKeyRequest over an
// encrypted connection only. store keeps the key before it is used (nil
// clears it); it must be durable, as HA holds the key from then on. Before
// Serve.
func (s *Server) SetKeyStore(store func(psk []byte) error) { s.keyStore = store }

// SetTimeSink asks Home Assistant for the time, as ESPHome's homeassistant
// time platform does: a GetTimeRequest to each client that subscribes to
// states (HA does on connect), and again every timeRefresh to one that is
// subscribed. HA's client answers by itself (aioesphomeapi connection.py
// _handle_get_time_request_internal) with the time to the second and its
// timezone. Before Serve.
func (s *Server) SetTimeSink(sink func(t time.Time, timezone string)) { s.timeSink = sink }

// HasKey reports whether a key is in use.
func (s *Server) HasKey() bool {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	return s.psk != nil
}

func (s *Server) key() []byte {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	return s.psk
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// setKey answers NoiseEncryptionSetKeyRequest, as ESPHome's firmware does
// (api_connection.cpp send_noise_encryption_set_key_response_): an empty
// key clears it, one that is not 32 bytes of base64 or is all zeros is
// refused, and one that cannot be kept is refused. Over plaintext it is
// refused too (ESPHome still takes it there, deprecated, until 2027.2;
// HA uses the encrypted setup for a device that says provisionable).
// After success the key is put to use and every client is told to
// disconnect, so none from before the key survives.
func (s *Server) setKey(c *conn, b64 []byte) bool {
	if s.keyStore == nil {
		log.Printf("[esphome] %s: a key was offered; this server's key cannot be set", c.c.RemoteAddr())
		return false
	}
	if c.noise == nil {
		log.Printf("[esphome] %s: a key offered in plaintext refused", c.c.RemoteAddr())
		return false
	}
	var psk []byte
	if len(b64) > 0 {
		k, err := base64.StdEncoding.DecodeString(string(b64))
		if err != nil || len(k) != KeySize {
			log.Printf("[esphome] %s: a key of the wrong form refused", c.c.RemoteAddr())
			return false
		}
		if allZero(k) {
			log.Printf("[esphome] %s: the all-zero key refused", c.c.RemoteAddr())
			return false
		}
		psk = k
	}
	if err := s.keyStore(psk); err != nil {
		log.Printf("[esphome] the key could not be kept: %v", err)
		return false
	}
	if psk == nil {
		log.Printf("[esphome] key cleared by %s", c.c.RemoteAddr())
	} else {
		log.Printf("[esphome] key set by %s", c.c.RemoteAddr())
	}
	// After the response has gone, as ESPHome's 100 ms.
	time.AfterFunc(100*time.Millisecond, func() {
		s.keyMu.Lock()
		s.psk = psk
		s.keyMu.Unlock()
		s.mu.Lock()
		all := make([]*conn, 0, len(s.conns))
		for c := range s.conns {
			all = append(all, c)
		}
		s.mu.Unlock()
		for _, c := range all {
			c.reply(&api.DisconnectRequest{})
			time.AfterFunc(100*time.Millisecond, c.close)
		}
	})
	return true
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
		cn := &conn{s: s, c: c, out: make(chan packet, outQueue), done: make(chan struct{}), born: time.Now()}
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
	// born is when the connection was made: lastIn and lastTime are
	// monotonic offsets from it, never wall-clock instants, because the
	// wall clock is what this server may step (SetTimeSink): an hour back
	// would read as an hour to wait (bench, 00LQ, 2026-11-18).
	born     time.Time
	lastIn   atomic.Int64 // since born, of the last frame read
	lastTime atomic.Int64 // since born, of the last time asked
	pinged   atomic.Bool  // a ping is out since the last frame read
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

// since is how long ago a monotonic mark was.
func (c *conn) since(mark *atomic.Int64) time.Duration {
	return time.Since(c.born) - time.Duration(mark.Load())
}

// askTime sends GetTimeRequest.
func (c *conn) askTime() {
	c.lastTime.Store(int64(time.Since(c.born)))
	c.reply(&api.GetTimeRequest{})
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
	tick := time.NewTicker(keepaliveTick)
	defer tick.Stop()
	var buf []byte
	for {
		select {
		case <-c.done:
			return
		case <-tick.C:
			if c.s.timeSink != nil && c.subscribed.Load() &&
				c.since(&c.lastTime) >= timeRefresh {
				c.askTime()
			}
			if c.since(&c.lastIn) >= keepaliveIdle && !c.pinged.Swap(true) {
				p, _ := packetOf(&api.PingRequest{})
				var err error
				if buf, err = c.frame(buf[:0], p); err == nil {
					if err := c.writeRaw(buf); err != nil {
						c.close()
						return
					}
				}
			}
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
	c.c.SetReadDeadline(time.Now().Add(readIdle))
	psk := c.s.key()
	encrypted := psk != nil
	if !encrypted && c.s.keyStore != nil {
		// No key yet: a client that opens encrypted is setting one up, with
		// the all-zero key.
		first, err := r.Peek(1)
		if err != nil {
			return
		}
		if first[0] == noisePreamble {
			encrypted, psk = true, make([]byte, KeySize)
		}
	}
	if encrypted {
		t, err := handshake(r, c.writeRaw, psk, c.s.info.Name, c.s.info.MAC)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("[esphome] %s: %v", c.c.RemoteAddr(), err)
			}
			return
		}
		c.noise = t
	}
	c.lastIn.Store(int64(time.Since(c.born)))
	c.s.wg.Add(1)
	go c.write()
	for {
		c.c.SetReadDeadline(time.Now().Add(readIdle))
		typ, payload, err := c.readMessage(r)
		if err == nil {
			c.lastIn.Store(int64(time.Since(c.born)))
			c.pinged.Store(false)
		}
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
			ProjectName: i.ProjectName, ProjectVersion: i.ProjectVersion,
			ApiEncryptionSupported:     s.keyStore != nil || s.HasKey(),
			ApiEncryptionProvisionable: s.keyStore != nil && !s.HasKey()})
	case *api.NoiseEncryptionSetKeyRequest:
		c.reply(&api.NoiseEncryptionSetKeyResponse{Success: s.setKey(c, m.Key)})
	case *api.ListEntitiesRequest:
		for _, e := range s.entities {
			c.reply(e.Info())
		}
		c.reply(&api.ListEntitiesDoneResponse{})
	case *api.SubscribeStatesRequest:
		c.subscribed.Store(true)
		if s.timeSink != nil {
			c.askTime()
		}
		for _, e := range s.entities {
			if st := e.State(); st != nil {
				c.reply(st)
			}
		}
	case *api.SwitchCommandRequest:
		if w, ok := s.byKey[m.Key].(*Switch); ok && w.OnCommand != nil {
			w.OnCommand(m.State)
		}
	case *api.GetTimeResponse:
		if s.timeSink != nil && m.EpochSeconds != 0 {
			s.timeSink(time.Unix(int64(m.EpochSeconds), 0), m.Timezone)
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
