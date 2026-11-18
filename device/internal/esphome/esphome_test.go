package esphome

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"google.golang.org/protobuf/proto"
)

func TestFramesRoundTripAtTheVaruintEdges(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 16383, 16384, MaxPayload} {
		payload := bytes.Repeat([]byte{7}, n)
		for _, typ := range []uint32{1, 127, 128, 300} {
			f := AppendFrame(nil, typ, payload)
			gotT, gotP, err := ReadFrame(bufio.NewReader(bytes.NewReader(f)))
			if err != nil || gotT != typ || !bytes.Equal(gotP, payload) {
				t.Fatalf("len %d type %d: %d %d %v", n, typ, gotT, len(gotP), err)
			}
		}
	}
	// 300 is two bytes: 0xac 0x02, little-endian groups of seven.
	if f := AppendFrame(nil, 300, nil); !bytes.Equal(f, []byte{0, 0, 0xac, 0x02}) {
		t.Fatalf("frame %x", f)
	}
}

func TestBadFramesAreRefused(t *testing.T) {
	cases := map[string]struct {
		b    []byte
		want error
	}{
		"noise preamble":   {[]byte{1, 0, 1}, ErrNoise},
		"other preamble":   {[]byte{2, 0, 1}, errPreamble},
		"varuint too long": {[]byte{0, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, errVaruint},
		"too large":        {AppendFrame(nil, 1, nil)[:1:1], nil},
		"truncated":        {[]byte{0, 5, 1, 1, 2}, io.ErrUnexpectedEOF},
	}
	big := append([]byte{0}, appendVaruint(nil, MaxPayload+1)...)
	cases["too large"] = struct {
		b    []byte
		want error
	}{append(big, 1), errTooLarge}
	for name, c := range cases {
		_, _, err := ReadFrame(bufio.NewReader(bytes.NewReader(c.b)))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func TestEveryMessageWithAnIDCodesBothWays(t *testing.T) {
	if len(api.MessageIDs) < 100 {
		t.Fatalf("only %d message ids: the generator lost the (id) option", len(api.MessageIDs))
	}
	seen := map[uint32]string{}
	for name, id := range api.MessageIDs {
		if other, dup := seen[id]; dup {
			t.Fatalf("id %d is both %s and %s", id, other, name)
		}
		seen[id] = name
	}
	for _, m := range []proto.Message{&api.HelloRequest{ClientInfo: "x"}, &api.SensorStateResponse{Key: 3, State: 1.5}} {
		typ, b, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		got, known, err := Decode(typ, b)
		if err != nil || !known || !proto.Equal(got, m) {
			t.Fatalf("%v: %v %v %v", m, got, known, err)
		}
	}
	if _, known, _ := Decode(60000, nil); known {
		t.Fatal("an undefined type decoded")
	}
}

// client is a minimal API client for the tests.
type client struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dialServer(t *testing.T, s *Server) *client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(s.Close)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &client{t, c, bufio.NewReader(c)}
}

func (c *client) send(m proto.Message) {
	c.t.Helper()
	typ, b, err := Encode(m)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.c.Write(AppendFrame(nil, typ, b)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() proto.Message {
	c.t.Helper()
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, p, err := ReadFrame(c.r)
	if err != nil {
		c.t.Fatal(err)
	}
	m, _, err := Decode(typ, p)
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

func testEntities() (*Sensor, *BinarySensor, *Event) {
	lux := &Sensor{Desc: &api.ListEntitiesSensorResponse{ObjectId: "ambient_light", Key: 3, Name: "Ambient Light",
		UnitOfMeasurement: "lx", DeviceClass: "illuminance", StateClass: api.SensorStateClass_STATE_CLASS_MEASUREMENT}}
	muted := &BinarySensor{Desc: &api.ListEntitiesBinarySensorResponse{ObjectId: "mic_muted", Key: 4, Name: "Microphone Muted"}}
	btn := &Event{Desc: &api.ListEntitiesEventResponse{ObjectId: "action_button", Key: 2, Name: "Action Button",
		DeviceClass: "button", EventTypes: []string{"long", "single", "double", "triple"}}}
	return lux, muted, btn
}

func TestTheHandshakeAndTheEntities(t *testing.T) {
	lux, muted, btn := testEntities()
	s := New(DeviceInfo{Name: "echohome-test", FriendlyName: "Test", MAC: "02:EC:00:00:00:01",
		Manufacturer: "EchoHome", Model: "biscuit", ProjectName: "EchoHome.biscuit", ProjectVersion: "dev"}, btn, lux, muted)
	c := dialServer(t, s)
	c.send(&api.HelloRequest{ClientInfo: "test", ApiVersionMajor: 1, ApiVersionMinor: 10})
	if h, ok := c.recv().(*api.HelloResponse); !ok || h.ApiVersionMajor != 1 || h.Name != "echohome-test" {
		t.Fatalf("hello %v", h)
	}
	c.send(&api.DeviceInfoRequest{})
	if d, ok := c.recv().(*api.DeviceInfoResponse); !ok || d.MacAddress != "02:EC:00:00:00:01" || d.ProjectName != "EchoHome.biscuit" {
		t.Fatalf("device info %v", d)
	}
	c.send(&api.ListEntitiesRequest{})
	var names []string
	for {
		m := c.recv()
		if _, done := m.(*api.ListEntitiesDoneResponse); done {
			break
		}
		names = append(names, string(m.ProtoReflect().Descriptor().Name()))
	}
	if len(names) != 3 || names[0] != "ListEntitiesEventResponse" {
		t.Fatalf("entities %v", names)
	}
	// States: missing until measured, never 0.
	c.send(&api.SubscribeStatesRequest{})
	if st, ok := c.recv().(*api.SensorStateResponse); !ok || !st.MissingState {
		t.Fatalf("an unmeasured sensor: %v", st)
	}
	c.recv() // the binary sensor's
	lux.Set(42)
	if st, ok := c.recv().(*api.SensorStateResponse); !ok || st.State != 42 || st.MissingState {
		t.Fatalf("pushed %v", st)
	}
	lux.Set(42) // unchanged: nothing
	muted.Set(true)
	if st, ok := c.recv().(*api.BinarySensorStateResponse); !ok || !st.State {
		t.Fatalf("pushed %v", st)
	}
	s.Fire(2, "long")
	if ev, ok := c.recv().(*api.EventResponse); !ok || ev.EventType != "long" || ev.Key != 2 {
		t.Fatalf("event %v", ev)
	}
	c.send(&api.PingRequest{})
	if _, ok := c.recv().(*api.PingResponse); !ok {
		t.Fatal("no pong")
	}
	c.send(&api.DisconnectRequest{})
	if _, ok := c.recv().(*api.DisconnectResponse); !ok {
		t.Fatal("no disconnect response")
	}
}

// A client whose queue is full is dropped at once: sending to it never
// waits, so it cannot hold up the state of the others.
func TestASlowClientIsDroppedNotWaitedFor(t *testing.T) {
	s := New(DeviceInfo{Name: "n"})
	a, b := net.Pipe()
	defer b.Close()
	cn := &conn{s: s, c: a, out: make(chan packet, 2), done: make(chan struct{})}
	s.conns[cn] = struct{}{}
	// No writer runs: the queue fills and stays full.
	start := time.Now()
	for i := 0; i < 3; i++ {
		cn.send(packet{typ: 7})
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("a full queue made the sender wait")
	}
	select {
	case <-cn.done:
	default:
		t.Fatal("a client with a full queue was kept")
	}
	if s.Clients() != 0 {
		t.Fatal("dropped, and still counted")
	}
}

// A quiet client is pinged, and one that answers is kept past readIdle; one
// that does not answer is dropped.
func TestAQuietClientIsPingedAndKeptIfItAnswers(t *testing.T) {
	ki, kt, ri := keepaliveIdle, keepaliveTick, readIdle
	keepaliveIdle, keepaliveTick, readIdle = 150*time.Millisecond, 20*time.Millisecond, 400*time.Millisecond
	// Restored after the server is closed (cleanups run last-first).
	t.Cleanup(func() { keepaliveIdle, keepaliveTick, readIdle = ki, kt, ri })
	s := New(DeviceInfo{Name: "n"})
	c := dialServer(t, s)
	c.send(&api.HelloRequest{})
	c.recv()
	start := time.Now()
	for pings := 0; time.Since(start) < time.Second; pings++ {
		if _, ok := c.recv().(*api.PingRequest); !ok {
			t.Fatal("not a ping")
		}
		c.send(&api.PingResponse{})
	}
	if s.Clients() != 1 {
		t.Fatal("a client that answers its pings was dropped")
	}
	// Silent now: pinged once more, then dropped.
	if _, ok := c.recv().(*api.PingRequest); !ok {
		t.Fatal("not a ping")
	}
	c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ReadFrame(c.r); err == nil {
		t.Fatal("a second frame to a silent client")
	}
	if s.Clients() != 0 {
		t.Fatal("a silent client was kept")
	}
}
