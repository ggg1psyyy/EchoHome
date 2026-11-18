package esphome

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome/api"
)

// A provisionable server, as Home Assistant sets its key (esphome
// manager.py): no key, then HA's key over the all-zero setup connection.

type store struct {
	mu   sync.Mutex
	keys [][]byte
	fail bool
}

func (st *store) keep(psk []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fail {
		return net.ErrClosed
	}
	st.keys = append(st.keys, psk)
	return nil
}

func (st *store) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.keys)
}

func provisionable(t *testing.T) (*Server, *store, string) {
	t.Helper()
	lux, muted, btn := testEntities()
	s := New(DeviceInfo{Name: "echohome-test", MAC: "02:EC:AB:CD:00:01"}, btn, lux, muted)
	st := &store{}
	s.SetKeyStore(st.keep)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(s.Close)
	return s, st, ln.Addr().String()
}

var zeroKey = make([]byte, KeySize)

func plainClient(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &client{t, c, bufio.NewReader(c)}
}

func TestWithNoKeyItSaysProvisionableAndTakesPlaintext(t *testing.T) {
	_, _, addr := provisionable(t)
	c := plainClient(t, addr)
	c.send(&api.DeviceInfoRequest{})
	d, ok := c.recv().(*api.DeviceInfoResponse)
	if !ok || !d.ApiEncryptionSupported || !d.ApiEncryptionProvisionable {
		t.Fatalf("device info %v", d)
	}
}

func TestHomeAssistantSetsTheKeyOverTheZeroKey(t *testing.T) {
	s, st, addr := provisionable(t)
	c, reason := dialNoise(t, addr, zeroKey)
	if c == nil {
		t.Fatalf("the setup connection was refused: %s", reason)
	}
	c.sendMsg(&api.NoiseEncryptionSetKeyRequest{Key: []byte(base64.StdEncoding.EncodeToString(testKey))})
	if r, ok := c.recvMsg().(*api.NoiseEncryptionSetKeyResponse); !ok || !r.Success {
		t.Fatalf("set key: %v", r)
	}
	if st.count() != 1 || !bytes.Equal(st.keys[0], testKey) {
		t.Fatalf("kept %v", st.keys)
	}
	// Every client is told to go, so none from before the key survives.
	if _, ok := c.recvMsg().(*api.DisconnectRequest); !ok {
		t.Fatal("no disconnect request")
	}
	if !s.HasKey() {
		t.Fatal("the key is not in use")
	}
	// From now: the zero key and plaintext are refused, the key works.
	if c, reason := dialNoise(t, addr, zeroKey); c != nil || reason != "Handshake MAC failure" {
		t.Fatalf("the zero key after setup: %v %q", c, reason)
	}
	p := plainClient(t, addr)
	p.send(&api.HelloRequest{})
	p.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if f, err := readNoiseFrame(p.r); err != nil || string(f) != "\x01Bad indicator byte" {
		t.Fatalf("plaintext after setup: %q %v", f, err)
	}
	k, reason := dialNoise(t, addr, testKey)
	if k == nil {
		t.Fatalf("the new key refused: %s", reason)
	}
	k.sendMsg(&api.DeviceInfoRequest{})
	if d, ok := k.recvMsg().(*api.DeviceInfoResponse); !ok || d.ApiEncryptionProvisionable || !d.ApiEncryptionSupported {
		t.Fatalf("device info with a key %v", d)
	}
	// HA clears it (an entry removed): empty key, and plaintext is back.
	k.sendMsg(&api.NoiseEncryptionSetKeyRequest{})
	if r, ok := k.recvMsg().(*api.NoiseEncryptionSetKeyResponse); !ok || !r.Success {
		t.Fatalf("clear: %v", r)
	}
	if st.count() != 2 || st.keys[1] != nil {
		t.Fatalf("kept %v", st.keys)
	}
	for deadline := time.Now().Add(2 * time.Second); s.HasKey() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if s.HasKey() {
		t.Fatal("still a key after clearing")
	}
}

func TestBadKeysAreRefused(t *testing.T) {
	_, st, addr := provisionable(t)
	c, reason := dialNoise(t, addr, zeroKey)
	if c == nil {
		t.Fatal(reason)
	}
	for _, k := range []string{
		base64.StdEncoding.EncodeToString(zeroKey),      // the setup key is no key
		base64.StdEncoding.EncodeToString(testKey[:31]), // 31 bytes
		"not base64!",
	} {
		c.sendMsg(&api.NoiseEncryptionSetKeyRequest{Key: []byte(k)})
		if r, ok := c.recvMsg().(*api.NoiseEncryptionSetKeyResponse); !ok || r.Success {
			t.Fatalf("%q taken", k)
		}
	}
	if st.count() != 0 {
		t.Fatal("a refused key was kept")
	}
}

func TestAKeyOfferedInPlaintextOrNotKeptIsRefused(t *testing.T) {
	s, st, addr := provisionable(t)
	p := plainClient(t, addr)
	p.send(&api.NoiseEncryptionSetKeyRequest{Key: []byte(base64.StdEncoding.EncodeToString(testKey))})
	if r, ok := p.recv().(*api.NoiseEncryptionSetKeyResponse); !ok || r.Success {
		t.Fatal("a key taken in plaintext")
	}
	st.fail = true
	c, _ := dialNoise(t, addr, zeroKey)
	c.sendMsg(&api.NoiseEncryptionSetKeyRequest{Key: []byte(base64.StdEncoding.EncodeToString(testKey))})
	if r, ok := c.recvMsg().(*api.NoiseEncryptionSetKeyResponse); !ok || r.Success {
		t.Fatal("a key that could not be kept was reported set")
	}
	if s.HasKey() {
		t.Fatal("a key that could not be kept is in use")
	}
}

// A server whose key is fixed (no store) takes none.
func TestAFixedKeyIsNotChanged(t *testing.T) {
	_, addr := encryptedServer(t)
	c, _ := dialNoise(t, addr, testKey)
	c.sendMsg(&api.DeviceInfoRequest{})
	if d, ok := c.recvMsg().(*api.DeviceInfoResponse); !ok || d.ApiEncryptionProvisionable {
		t.Fatalf("%v", d)
	}
	c.sendMsg(&api.NoiseEncryptionSetKeyRequest{Key: []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, KeySize)))})
	if r, ok := c.recvMsg().(*api.NoiseEncryptionSetKeyResponse); !ok || r.Success {
		t.Fatal("a fixed key was changed")
	}
}
