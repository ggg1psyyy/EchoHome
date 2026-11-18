package server

import (
	"testing"

	"github.com/wilbowes/EchoMuse/internal/softmute"
)

// Home Assistant's software mute: muted for every check, the microphones
// zeroed at the source; never on under the button's mute (refused); any
// press of the button clears it, and the button's own mute is its alone.
func TestTheSoftwareMuteAndTheButton(t *testing.T) {
	ring := &recordingRing{}
	s := testServer(ring)
	var got []bool
	s.mute.SetOnMuteChange(func(m bool) { got = append(got, m) })
	defer softmute.Set(false)

	if !s.SetSoftMute(true) || !s.IsMuted() || s.HardMuted() || !softmute.On() {
		t.Fatalf("soft on: muted %v hard %v zeroing %v", s.IsMuted(), s.HardMuted(), softmute.On())
	}
	if !lastIsRed(ring) {
		t.Fatalf("soft on, the ring is %+v", ring.last())
	}
	if !s.SetSoftMute(false) || s.IsMuted() || softmute.On() || !lastIsDark(ring) {
		t.Fatal("soft off did not unmute")
	}
	// Under the button's mute, turning it on is refused.
	s.mute.Toggle() // hard on
	if s.SetSoftMute(true) || s.SoftMuted() {
		t.Fatal("a software mute was turned on under the button's")
	}
	s.mute.Toggle() // hard off
	if s.IsMuted() {
		t.Fatal("the button's unmute left a mute")
	}
	// A press while software-muted: the button's mute, the soft one cleared.
	s.SetSoftMute(true)
	s.mute.Toggle()
	if !s.HardMuted() || s.SoftMuted() || softmute.On() {
		t.Fatalf("press under a soft mute: hard %v soft %v", s.HardMuted(), s.SoftMuted())
	}
	s.mute.Toggle()
	if s.IsMuted() {
		t.Fatal("the button's unmute left the Echo muted")
	}
	want := []bool{true, false, true, false, true, true, false}
	if len(got) != len(want) {
		t.Fatalf("callbacks %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("callbacks %v, want %v", got, want)
		}
	}
}

// The ring is the soft mute's only sign (the button's light is the hard
// mute's), so "no red ring while muted" never hides it.
func TestTheRingAlwaysShowsASoftMute(t *testing.T) {
	old := hasMuteButtonLED
	hasMuteButtonLED = func() bool { return true }
	defer func() { hasMuteButtonLED = old }()
	defer softmute.Set(false)
	ring := &recordingRing{}
	s := testServer(ring)
	s.SetMuteRing(false)
	s.SetSoftMute(true)
	if !lastIsRed(ring) {
		t.Fatalf("a soft mute with the ring setting off showed %+v", ring.last())
	}
}

// Kept across a reboot, apart from the hard mute.
func TestTheSoftMuteIsKept(t *testing.T) {
	p := t.TempDir() + "/state.json"
	saveDeviceState(p, deviceState{SoftMuted: true})
	st, ok := loadDeviceState(p)
	if !ok || st.Muted || !st.SoftMuted {
		t.Fatalf("%+v %v", st, ok)
	}
}
