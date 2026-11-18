package owned

import "testing"

func TestHomeAssistantUndoesItsOwnMuteButNeverOneByHand(t *testing.T) {
	var m MuteSource
	m.Boot(false)
	// HA mutes: allowed, and HA may unmute it.
	if toggle, ok := m.Command(true, false); !toggle || !ok {
		t.Fatal("HA could not mute")
	}
	m.Changed(true)
	if toggle, ok := m.Command(false, true); !toggle || !ok {
		t.Fatal("HA could not undo its own mute")
	}
	m.Changed(false)
	// By hand: HA's unmute is refused.
	m.Changed(true)
	if toggle, ok := m.Command(false, true); toggle || ok {
		t.Fatal("HA unmuted a mute made by hand")
	}
	// The hand unmutes; HA's mute of an unmuted Echo works again.
	m.Changed(false)
	if toggle, ok := m.Command(true, false); !toggle || !ok {
		t.Fatal("after the hand")
	}
}

func TestAMuteNobodyHereMadeIsByHand(t *testing.T) {
	var m MuteSource
	m.Boot(true) // muted at boot
	if _, ok := m.Command(false, true); ok {
		t.Fatal("a mute found at boot was unmuted from HA")
	}
	// HA's unmute was turned back by a hardware privacy switch (muted to
	// match): no HA command made that mute.
	m.Boot(false)
	m.Command(true, false)
	m.Changed(true)        // HA's
	m.Command(false, true) // HA unmutes...
	m.Changed(false)       //
	m.Changed(true)        // ...and the hardware mutes to match
	if _, ok := m.Command(false, true); ok {
		t.Fatal("the hardware's mute was treated as HA's")
	}
}

func TestCommandsThatChangeNothingDoNothing(t *testing.T) {
	var m MuteSource
	if toggle, ok := m.Command(false, false); toggle || !ok {
		t.Fatal()
	}
	m.Changed(true) // by hand
	if toggle, ok := m.Command(true, true); toggle || !ok {
		t.Fatal("muting a muted Echo")
	}
}
