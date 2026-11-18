package mediaplayer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Ported from controller/tests/test_wav.py: the format's edges rather than
// the one header ffmpeg happens to write.

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func fmtChunk(af, ch uint16, rate uint32, bits uint16, extra []byte) []byte {
	body := binary.LittleEndian.AppendUint16(nil, af)
	body = binary.LittleEndian.AppendUint16(body, ch)
	body = binary.LittleEndian.AppendUint32(body, rate)
	body = binary.LittleEndian.AppendUint32(body, rate*uint32(ch)*uint32(bits)/8)
	body = binary.LittleEndian.AppendUint16(body, ch*bits/8)
	body = binary.LittleEndian.AppendUint16(body, bits)
	body = append(body, extra...)
	return chunk("fmt ", body)
}

func chunk(id string, body []byte) []byte {
	out := append([]byte(id), le32(uint32(len(body)))...)
	out = append(out, body...)
	if len(body)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

func wav(data []byte, dataSize uint32, chunks ...[]byte) []byte {
	out := append([]byte("RIFF"), le32(0xFFFFFFFF)...)
	out = append(out, "WAVE"...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	out = append(out, "data"...)
	out = append(out, le32(dataSize)...)
	return append(out, data...)
}

var wireFmt = func() []byte { return fmtChunk(1, 1, 48000, 16, nil) }

// The first 78 bytes ffmpeg writes streaming 48 kHz mono S16 to a pipe.
var ffmpegPipe = []byte("RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\xbb\x00\x00" +
	"\x00w\x01\x00\x02\x00\x10\x00LIST\x1a\x00\x00\x00INFOISFT\x0e\x00\x00\x00" +
	"Lavf58.76.100\x00data\xff\xff\xff\xff")

func TestFFmpegsStreamedHeader(t *testing.T) {
	var s WavStream
	pcm, err := s.Push(append(append([]byte{}, ffmpegPipe...), 0xe3, 0x00, 0xdf, 0x01))
	if err != nil || !bytes.Equal(pcm, []byte{0xe3, 0x00, 0xdf, 0x01}) {
		t.Fatalf("%x %v", pcm, err)
	}
	if *s.Format != (Format{1, 1, 48000, 16}) || !IsWirePCM(s.Format) {
		t.Fatalf("%+v", s.Format)
	}
	if pcm, _ := s.Push([]byte{0xb7, 0x02}); !bytes.Equal(pcm, []byte{0xb7, 0x02}) {
		t.Fatal("after the header, bytes pass straight through")
	}
}

func TestOneByteAtATime(t *testing.T) {
	var s WavStream
	blob := wav([]byte("ABCD"), 0xFFFFFFFF, wireFmt(), chunk("LIST", []byte("INFOx")))
	var out []byte
	for i := range blob {
		pcm, err := s.Push(blob[i : i+1])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, pcm...)
	}
	if string(out) != "ABCD" {
		t.Fatalf("%q", out)
	}
}

func TestAnOddSizedChunkIsPadded(t *testing.T) {
	var s WavStream
	pcm, err := s.Push(wav([]byte("PCM!"), 0xFFFFFFFF, wireFmt(), chunk("junk", []byte("abc"))))
	if err != nil || string(pcm) != "PCM!" {
		t.Fatalf("%q %v", pcm, err)
	}
}

func TestFmtVariants(t *testing.T) {
	ext := append([]byte{0x16, 0x00, 0x10, 0x00, 0x04, 0x00, 0x00, 0x00},
		[]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}...)
	for _, c := range []struct {
		extra []byte
		af    uint16
	}{{nil, 1}, {[]byte{0, 0}, 1}, {ext, 0xFFFE}} {
		var s WavStream
		if _, err := s.Push(wav([]byte{0, 0}, 0xFFFFFFFF, fmtChunk(c.af, 1, 48000, 16, c.extra))); err != nil {
			t.Fatal(err)
		}
		if s.Format.AudioFormat != c.af || !IsWirePCM(s.Format) {
			t.Fatalf("%+v", s.Format)
		}
	}
}

func TestAnythingElseIsNotWirePCM(t *testing.T) {
	for _, f := range []*Format{{1, 2, 48000, 16}, {1, 1, 22050, 16}, {1, 1, 48000, 24}, {3, 1, 48000, 32}, nil} {
		if IsWirePCM(f) {
			t.Fatalf("%+v taken", f)
		}
	}
}

func TestRealSizesAreAcceptedToo(t *testing.T) {
	var s WavStream
	if pcm, err := s.Push(wav([]byte{0, 1}, 2, wireFmt())); err != nil || !bytes.Equal(pcm, []byte{0, 1}) {
		t.Fatalf("%x %v", pcm, err)
	}
}

func TestNotAWavIsRefused(t *testing.T) {
	for _, blob := range [][]byte{append([]byte("fLaC\x00\x00\x00\x22"), make([]byte, 8)...), []byte("RIFF\x00\x00\x00\x00AVI LIST")} {
		var s WavStream
		if _, err := s.Push(blob); !errors.Is(err, ErrNotWav) {
			t.Fatalf("%q: %v", blob, err)
		}
	}
}

func TestDataBeforeFmtIsRefused(t *testing.T) {
	var s WavStream
	if _, err := s.Push([]byte("RIFF\xff\xff\xff\xffWAVEdata\xff\xff\xff\xff\x00\x00")); !errors.Is(err, ErrNotWav) {
		t.Fatal(err)
	}
}

func TestAHeaderThatNeverReachesDataIsBounded(t *testing.T) {
	var s WavStream
	head := append([]byte("RIFF\xff\xff\xff\xffWAVE"), wireFmt()...)
	head = append(head, "junk"...)
	head = append(head, le32(1_000_000)...)
	if _, err := s.Push(head); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(make([]byte, maxHeader+1)); !errors.Is(err, ErrNotWav) {
		t.Fatal(err)
	}
}

func TestShortInputWaits(t *testing.T) {
	var s WavStream
	if pcm, err := s.Push([]byte("RIFF")); err != nil || len(pcm) != 0 || s.Format != nil {
		t.Fatal()
	}
}
