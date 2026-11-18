package mediaplayer

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The WAV Home Assistant's ffmpeg proxy sends, parsed incrementally: a port
// of controller/em_wav.py (and its tests), as the controller already asks HA
// for the same thing. HA transcodes any source to the format the media
// player declares, so with WAV at the speaker's own format (48 kHz mono
// S16LE) the data chunk is the PCM the speaker plays: no decoder.
//
// A streamed WAV cannot know its length: the RIFF and data sizes are
// placeholders (ffmpeg writes 0xFFFFFFFF on a pipe) and are ignored; the
// data chunk runs to the end of the stream. Chunks before it (LIST, fact)
// are skipped, with RIFF's pad byte on odd sizes.

// Format is a WAV's fmt chunk.
type Format struct {
	AudioFormat uint16 // 1 PCM; 0xFFFE WAVE_FORMAT_EXTENSIBLE
	Channels    uint16
	SampleRate  uint32
	Bits        uint16
}

const (
	wavPCM        = 1
	wavExtensible = 0xFFFE
	// maxHeader bounds a header that never reaches its data chunk; HA's
	// proxy writes a few dozen bytes of metadata at most.
	maxHeader = 64 * 1024
)

// ErrNotWav: the stream is not a WAV this can read.
var ErrNotWav = errors.New("mediaplayer: not a WAV stream")

// WavStream takes bytes; once the header is parsed Format is set and Push
// returns PCM from the data chunk on.
type WavStream struct {
	buf    []byte
	Format *Format
	inData bool
}

// Ready reports whether the header is parsed.
func (w *WavStream) Ready() bool { return w.inData }

// Push feeds bytes and returns the PCM in them (none until the header is
// through).
func (w *WavStream) Push(b []byte) ([]byte, error) {
	if w.inData {
		return b, nil
	}
	w.buf = append(w.buf, b...)
	pcm, done, err := w.parse()
	if err != nil {
		return nil, err
	}
	if !done && len(w.buf) > maxHeader {
		return nil, fmt.Errorf("%w: no data chunk within %d bytes", ErrNotWav, maxHeader)
	}
	return pcm, nil
}

func (w *WavStream) parse() ([]byte, bool, error) {
	b := w.buf
	if len(b) < 12 {
		return nil, false, nil
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, false, fmt.Errorf("%w: not RIFF/WAVE", ErrNotWav)
	}
	pos := 12
	for {
		if len(b) < pos+8 {
			return nil, false, nil
		}
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4:]))
		body := pos + 8
		if id == "data" {
			if w.Format == nil {
				return nil, false, fmt.Errorf("%w: data chunk before fmt", ErrNotWav)
			}
			w.inData = true
			pcm := append([]byte(nil), b[body:]...)
			w.buf = nil
			return pcm, true, nil
		}
		end := body + size + size&1
		if size < 0 || end < body || len(b) < end {
			return nil, false, nil
		}
		if id == "fmt " {
			if size < 16 {
				return nil, false, fmt.Errorf("%w: short fmt chunk", ErrNotWav)
			}
			f := b[body:]
			w.Format = &Format{AudioFormat: binary.LittleEndian.Uint16(f), Channels: binary.LittleEndian.Uint16(f[2:]),
				SampleRate: binary.LittleEndian.Uint32(f[4:]), Bits: binary.LittleEndian.Uint16(f[14:])}
		}
		pos = end
	}
}

// IsWirePCM reports whether the data chunk is what the speaker plays: mono
// S16LE at 48 kHz. Anything else is refused: the Echo has no decoder, and
// other bytes played as S16 are full-scale noise.
func IsWirePCM(f *Format) bool {
	return f != nil && (f.AudioFormat == wavPCM || f.AudioFormat == wavExtensible) &&
		f.Channels == 1 && f.SampleRate == 48000 && f.Bits == 16
}
