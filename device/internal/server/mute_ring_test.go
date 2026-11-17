package server

import (
	"testing"
	"time"
)

func lastIsRed(r *recordingRing) bool  { l := r.last(); return l.R > 0 && l.G == 0 && l.B == 0 }
func lastIsDark(r *recordingRing) bool { l := r.last(); return l.R == 0 && l.G == 0 && l.B == 0 }

// Off, the ring stays dark while muted, and the lit button alone says it;
// but only where the button lights itself, or a muted Echo would show
// nothing. A change while muted repaints at once.
func TestTheMuteRingCanBeLeftOffOnlyWhereTheButtonLights(t *testing.T) {
	for _, button := range []bool{true, false} {
		old := hasMuteButtonLED
		hasMuteButtonLED = func() bool { return button }
		ring := &recordingRing{}
		s := testServer(ring)
		s.mute.muted = true
		s.mute.showMuteLEDs()
		if !lastIsRed(ring) {
			t.Fatalf("button %v: muted with the setting on, the ring is %+v", button, ring.last())
		}
		s.SetMuteRing(false)
		if button && !lastIsDark(ring) {
			t.Fatalf("the ring stayed red with the setting off: %+v", ring.last())
		}
		if !button && !lastIsRed(ring) {
			t.Fatalf("no lit button, and the ring went dark: a muted Echo showing nothing (%+v)", ring.last())
		}
		s.SetMuteRing(true)
		if !lastIsRed(ring) {
			t.Fatalf("button %v: back on, the ring is %+v", button, ring.last())
		}
		hasMuteButtonLED = old
	}
}

func TestTheMuteRingSettingPaintsNothingWhileUnmuted(t *testing.T) {
	ring := &recordingRing{}
	s := testServer(ring)
	s.SetMuteRing(false)
	s.SetMuteRing(true)
	if n := ringFrameCount(ring); n != 0 {
		t.Fatalf("unmuted, the setting painted %d frames", n)
	}
}

// The volume arc hands the ring back to the mute ring as the mute
// controller paints it: dark with the setting off, not its own red.
func TestTheVolumeArcHandsBackToTheMuteRingAsSet(t *testing.T) {
	old := hasMuteButtonLED
	hasMuteButtonLED = func() bool { return true }
	defer func() { hasMuteButtonLED = old }()
	ring := &recordingRing{}
	s := testServer(ring)
	s.SetMuteRing(false)
	s.mute.muted = true
	s.VolumeStepUp()
	time.Sleep(volumeLEDSecs*time.Second + 300*time.Millisecond)
	if !lastIsDark(ring) {
		t.Fatalf("after the arc, muted with the ring off: %+v", ring.last())
	}
}
