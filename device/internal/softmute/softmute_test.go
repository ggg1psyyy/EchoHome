package softmute

import (
	"bytes"
	"testing"
)

// biscuit: 9 channels of S24_3LE, 7 microphones then two others (ch8 the
// loopback); donut: 4 channels of S32_LE, 2 microphones.
func TestOnlyTheMicrophonesAreSilenced(t *testing.T) {
	for _, c := range []struct{ channels, width, mics int }{{9, 3, 7}, {4, 4, 2}, {2, 2, 2}} {
		bpf := c.channels * c.width
		buf := bytes.Repeat([]byte{0x55}, 3*bpf)
		Zero(buf, 3, bpf, c.channels, c.mics)
		for f := 0; f < 3; f++ {
			frame := buf[f*bpf : (f+1)*bpf]
			mics, rest := frame[:c.mics*c.width], frame[c.mics*c.width:]
			if !bytes.Equal(mics, make([]byte, len(mics))) {
				t.Fatalf("%v frame %d: a microphone not silenced: %x", c, f, mics)
			}
			if !bytes.Equal(rest, bytes.Repeat([]byte{0x55}, len(rest))) {
				t.Fatalf("%v frame %d: the loopback was touched: %x", c, f, rest)
			}
		}
	}
}

func TestALayoutItCannotReadIsSilencedWhole(t *testing.T) {
	for _, c := range []struct{ bpf, channels, mics int }{{27, 0, 7}, {27, 9, 0}, {26, 9, 7}} {
		buf := bytes.Repeat([]byte{1}, 54)
		Zero(buf, 2, c.bpf, c.channels, c.mics)
		if !bytes.Equal(buf, make([]byte, 54)) {
			t.Fatalf("%v: %x", c, buf)
		}
	}
	// A short last frame is silenced too.
	buf := bytes.Repeat([]byte{1}, 40)
	Zero(buf, 2, 27, 9, 7)
	if !bytes.Equal(buf[27:], make([]byte, 13)) {
		t.Fatalf("%x", buf[27:])
	}
}

func TestSetAndOn(t *testing.T) {
	Set(true)
	if !On() {
		t.Fatal()
	}
	Set(false)
	if On() {
		t.Fatal()
	}
}
