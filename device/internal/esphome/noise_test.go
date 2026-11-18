package esphome

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"google.golang.org/protobuf/proto"
)

// A client written from the transport's description (aioesphomeapi's
// _frame_helper/noise.py), with flynn/noise as the initiator.
type noiseClient struct {
	t          *testing.T
	c          net.Conn
	r          *bufio.Reader
	send, recv *noise.CipherState
}

var testKey = bytes.Repeat([]byte{0x5a}, KeySize)

func encryptedServer(t *testing.T) (*Server, string) {
	t.Helper()
	lux, muted, btn := testEntities()
	s := New(DeviceInfo{Name: "echohome-test", MAC: "02:EC:AB:CD:00:01"}, btn, lux, muted)
	if err := s.SetKey(testKey); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(s.Close)
	return s, ln.Addr().String()
}

func (c *noiseClient) frame() []byte {
	c.t.Helper()
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := readNoiseFrame(c.r)
	if err != nil {
		c.t.Fatal(err)
	}
	return b
}

// dialNoise runs the handshake with key; it returns the client, or the
// server's rejection reason.
func dialNoise(t *testing.T, addr string, key []byte) (*noiseClient, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	nc := &noiseClient{t: t, c: c, r: bufio.NewReader(c)}
	c.Write(appendNoiseFrame(nil, nil)) // the hello: empty, as HA's
	hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: noiseSuite, Pattern: noise.HandshakeNN,
		Initiator: true, Prologue: []byte("NoiseAPIInit\x00\x00"), PresharedKey: key})
	if err != nil {
		t.Fatal(err)
	}
	sh := nc.frame()
	if want := "\x01echohome-test\x0002ecabcd0001\x00"; string(sh) != want {
		t.Fatalf("server hello %q, want %q", sh, want)
	}
	m1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Write(appendNoiseFrame(nil, append([]byte{0}, m1...)))
	m2 := nc.frame()
	if m2[0] != 0 {
		return nil, string(m2[1:])
	}
	if _, nc.send, nc.recv, err = hs.ReadMessage(nil, m2[1:]); err != nil {
		t.Fatal(err)
	}
	return nc, ""
}

func (c *noiseClient) sendMsg(m proto.Message) {
	c.t.Helper()
	typ, b, _ := Encode(m)
	pt := binary.BigEndian.AppendUint16(nil, uint16(typ))
	pt = binary.BigEndian.AppendUint16(pt, uint16(len(b)))
	ct, err := c.send.Encrypt(nil, nil, append(pt, b...))
	if err != nil {
		c.t.Fatal(err)
	}
	c.c.Write(appendNoiseFrame(nil, ct))
}

func (c *noiseClient) recvMsg() proto.Message {
	c.t.Helper()
	pt, err := c.recv.Decrypt(nil, nil, c.frame())
	if err != nil {
		c.t.Fatal(err)
	}
	m, _, err := Decode(uint32(binary.BigEndian.Uint16(pt)), pt[4:4+binary.BigEndian.Uint16(pt[2:])])
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

func TestTheEncryptedHandshakeAndMessages(t *testing.T) {
	_, addr := encryptedServer(t)
	c, reason := dialNoise(t, addr, testKey)
	if c == nil {
		t.Fatalf("refused: %s", reason)
	}
	c.sendMsg(&api.HelloRequest{ClientInfo: "test"})
	if h, ok := c.recvMsg().(*api.HelloResponse); !ok || h.Name != "echohome-test" {
		t.Fatalf("hello %v", h)
	}
	c.sendMsg(&api.ListEntitiesRequest{})
	n := 0
	for {
		if _, done := c.recvMsg().(*api.ListEntitiesDoneResponse); done {
			break
		}
		n++
	}
	if n != 3 {
		t.Fatalf("%d entities", n)
	}
}

// The reasons are ESPHome's: HA asks for the key again on the first, and
// learns from the second that a key is needed.
func TestAWrongKeyIsAMACFailure(t *testing.T) {
	_, addr := encryptedServer(t)
	if c, reason := dialNoise(t, addr, bytes.Repeat([]byte{1}, KeySize)); c != nil || reason != "Handshake MAC failure" {
		t.Fatalf("a wrong key: %v %q", c, reason)
	}
}

func TestAPlaintextClientIsTold(t *testing.T) {
	_, addr := encryptedServer(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	typ, b, _ := Encode(&api.HelloRequest{})
	c.Write(AppendFrame(nil, typ, b))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := readNoiseFrame(bufio.NewReader(c))
	if err != nil || string(f) != "\x01Bad indicator byte" {
		t.Fatalf("%q %v", f, err)
	}
}

func TestAKeyIs32Bytes(t *testing.T) {
	if err := New(DeviceInfo{}).SetKey(make([]byte, 31)); err == nil {
		t.Fatal("a 31-byte key was taken")
	}
}
