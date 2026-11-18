package owned

import "sync"

// MuteSource is who made the mute in force, as a Voice PE has it
// (home-assistant-voice.yaml master_mute_switch): Home Assistant's Mute
// switch mutes and unmutes, but a mute made by hand is the hardware
// switch's, and only the hand undoes it. A mute nobody here made (found at
// boot, or made to match a hardware privacy switch) counts as by hand: the
// safe reading.
type MuteSource struct {
	mu     sync.Mutex
	fromHA bool // the next mute is HA's
	byHand bool // the mute in force was made by hand
}

// Boot records the mute found at start.
func (m *MuteSource) Boot(muted bool) {
	m.mu.Lock()
	m.byHand, m.fromHA = muted, false
	m.mu.Unlock()
}

// Command is HA's switch: whether to toggle the mute, and false for refused
// (an unmute of a mute made by hand).
func (m *MuteSource) Command(on, muted bool) (toggle, allowed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !on && muted && m.byHand {
		return false, false
	}
	if on && !muted {
		m.fromHA = true
	}
	return on != muted, true
}

// Changed records a change of the mute, whoever made it: a mute is HA's
// only if HA's command made it.
func (m *MuteSource) Changed(muted bool) {
	m.mu.Lock()
	m.byHand = muted && !m.fromHA
	m.fromHA = false
	m.mu.Unlock()
}
