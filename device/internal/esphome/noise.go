package esphome

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/flynn/noise"
)

// ESPHome's encrypted transport, the responder's half, as the controller's
// esphome/frame_protocol.py and noise.py speak it (and ESPHome's firmware):
//
//	every frame: [0x01] [length, 2 bytes big-endian] [data]
//	client → hello (its data is bound into the handshake: the prologue is
//	         "NoiseAPIInit" + its length + it)
//	server → [0x01] name NUL mac NUL       chosen protocol, who we are
//	client → [0x00] + Noise message 1      -> psk, e
//	server → [0x00] + Noise message 2      <- e, ee
//	then each way: encrypt( type(2) length(2) payload ), big-endian
//
// Noise_NNpsk0_25519_ChaChaPoly_SHA256. A failure is answered [0x01] +
// reason before the close, in ESPHome's own words: Home Assistant keys on
// them ("Handshake MAC failure" makes it ask for the key again rather than
// report a dead device; "Bad indicator byte" tells a plaintext client that a
// key is needed).

const (
	// KeySize is the pre-shared key's: 32 bytes (base64 in Home Assistant).
	KeySize = 32
	// maxNoiseFrame is the 2-byte length's limit.
	maxNoiseFrame = 0xffff
	// maxNoisePayload is what fits one encrypted frame: the 4-byte inner
	// header and the 16-byte tag come out of it.
	maxNoisePayload = maxNoiseFrame - 4 - 16
)

var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

var errHandshake = errors.New("esphome: encryption handshake failed")

func readNoiseFrame(r *bufio.Reader) ([]byte, error) {
	var h [3]byte
	if _, err := io.ReadFull(r, h[:1]); err != nil {
		return nil, err
	}
	if h[0] != noisePreamble {
		return nil, errPreamble
	}
	if _, err := io.ReadFull(r, h[1:]); err != nil {
		return nil, eofTruncated(err)
	}
	b := make([]byte, binary.BigEndian.Uint16(h[1:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, eofTruncated(err)
	}
	return b, nil
}

func appendNoiseFrame(b, data []byte) []byte {
	b = append(b, noisePreamble, byte(len(data)>>8), byte(len(data)))
	return append(b, data...)
}

// noiseMAC is the MAC as the server hello carries it: twelve lowercase hex
// digits.
func noiseMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(mac, ":", ""))
}

// noiseTransport is one connection's two cipher states once the handshake
// is done. send is used by the writer alone and recv by the reader alone.
type noiseTransport struct {
	send, recv *noise.CipherState
}

// handshake runs the responder's half on a new connection. write sends raw
// bytes (it is called before the connection's writer runs). It returns the
// transport, or an error once the client has been told why.
func handshake(r *bufio.Reader, write func([]byte) error, psk []byte, name, mac string) (*noiseTransport, error) {
	reject := func(reason string, cause error) (*noiseTransport, error) {
		write(appendNoiseFrame(nil, append([]byte{0x01}, reason...)))
		return nil, fmt.Errorf("%w: %s (%v)", errHandshake, reason, cause)
	}
	first, err := r.Peek(1)
	if err != nil {
		return nil, err
	}
	if first[0] != noisePreamble {
		return reject("Bad indicator byte", errors.New("a plaintext client, and this server has a key"))
	}
	hello, err := readNoiseFrame(r)
	if err != nil {
		return nil, err
	}
	prologue := append([]byte("NoiseAPIInit"), byte(len(hello)>>8), byte(len(hello)))
	prologue = append(prologue, hello...)
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeNN,
		Initiator:             false,
		Prologue:              prologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
	})
	if err != nil {
		return nil, err
	}
	sh := append([]byte{0x01}, name...)
	sh = append(sh, 0)
	sh = append(append(sh, noiseMAC(mac)...), 0)
	if err := write(appendNoiseFrame(nil, sh)); err != nil {
		return nil, err
	}
	m1, err := readNoiseFrame(r)
	if err != nil {
		return nil, err
	}
	if len(m1) == 0 {
		return reject("Bad handshake packet len", nil)
	}
	if m1[0] != 0x00 {
		return reject("Bad handshake error byte", nil)
	}
	if _, _, _, err := hs.ReadMessage(nil, m1[1:]); err != nil {
		return reject("Handshake MAC failure", err)
	}
	m2, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return reject("Handshake MAC failure", err)
	}
	if err := write(appendNoiseFrame(nil, append([]byte{0x00}, m2...))); err != nil {
		return nil, err
	}
	// cs1 is the initiator's sending direction: the responder receives on it.
	return &noiseTransport{send: cs2, recv: cs1}, nil
}

// readMessage reads and decrypts one message.
func (t *noiseTransport) readMessage(r *bufio.Reader) (uint32, []byte, error) {
	ct, err := readNoiseFrame(r)
	if err != nil {
		return 0, nil, err
	}
	pt, err := t.recv.Decrypt(nil, nil, ct)
	if err != nil {
		return 0, nil, fmt.Errorf("esphome: encrypted frame rejected: %w", err)
	}
	if len(pt) < 4 {
		return 0, nil, errors.New("esphome: encrypted frame shorter than its header")
	}
	typ := binary.BigEndian.Uint16(pt)
	n := int(binary.BigEndian.Uint16(pt[2:]))
	if n > len(pt)-4 {
		return 0, nil, errors.New("esphome: encrypted frame shorter than its stated length")
	}
	return uint32(typ), pt[4 : 4+n], nil
}

// appendMessage encrypts one message onto b.
func (t *noiseTransport) appendMessage(b []byte, typ uint32, payload []byte) ([]byte, error) {
	if len(payload) > maxNoisePayload || typ > 0xffff {
		return b, fmt.Errorf("esphome: message %d of %d bytes does not fit a frame", typ, len(payload))
	}
	pt := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(pt, uint16(typ))
	binary.BigEndian.PutUint16(pt[2:], uint16(len(payload)))
	ct, err := t.send.Encrypt(nil, nil, append(pt, payload...))
	if err != nil {
		return b, err
	}
	return appendNoiseFrame(b, ct), nil
}
