package server

import (
	"testing"

	"github.com/wilbowes/EchoMuse/internal/softmute"
)

// Home Assistant's soft mute, as a Voice PE's: muted for every check, the
// microphones zeroed at the source, the button's mute untouched; the button,
// muted for either reason, unmutes both.
func TestTheSoftMuteMutesAndTheButtonHasTheLastWord(t *testing.T) {
	ring := &recordingRing{}
	s := testServer(ring)
	var got []bool
	s.mute.SetOnMuteChange(func(m bool) { got = append(got, m) })
	defer softmute.Set(false)

	s.SetSoftMute(true)
	if !s.IsMuted() || s.HardMuted() || !softmute.On() {
		t.Fatalf("soft on: muted %v hard %v zeroing %v", s.IsMuted(), s.HardMuted(), softmute.On())
	}
	if !lastIsRed(ring) {
		t.Fatalf("soft on, the ring is %+v", ring.last())
	}
	// HA turning it off unmutes only what HA muted.
	s.SetSoftMute(false)
	if s.IsMuted() || softmute.On() || !lastIsDark(ring) {
		t.Fatal("soft off did not unmute")
	}
	// Under a hard mute, HA's off changes nothing.
	s.mute.Toggle() // the button: hard on
	s.SetSoftMute(true)
	s.SetSoftMute(false)
	if !s.IsMuted() || !s.HardMuted() {
		t.Fatal("HA's switch undid the button's mute")
	}
	s.mute.Toggle() // the button: off
	// The button while soft-muted unmutes both.
	s.SetSoftMute(true)
	s.mute.Toggle()
	if s.IsMuted() || s.mute.SoftMuted() || softmute.On() {
		t.Fatalf("the button left a mute: muted %v soft %v", s.IsMuted(), s.mute.SoftMuted())
	}
	want := []bool{true, false, true, true, true, false, true, false}
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
