package server

import (
	"log"
	"sync"

	internalLed "github.com/wilbowes/EchoMuse/internal/bindings/led"
	"github.com/wilbowes/EchoMuse/internal/bindings/mixer"
	"github.com/wilbowes/EchoMuse/internal/softmute"
	"github.com/wilbowes/EchoMuse/pkg/board"
	"github.com/wilbowes/EchoMuse/pkg/led"
)

// hasMuteButtonLED: whether the mute button lights itself (a variable for
// the tests, which run on no Echo).
var hasMuteButtonLED = internalLed.HasMuteButtonLED

type muteController struct {
	mu sync.Mutex
	// muted is the hard mute: the button's, the ADC muted in hardware.
	muted bool
	// soft is Home Assistant's mute (softmute): the microphones zeroed in
	// software, the hardware and the button's light untouched. The Echo is
	// muted while either is on (IsMuted).
	soft bool
	// ringOff: the red ring is not shown while muted (the mute ring
	// setting), honoured only where the mute button lights itself.
	ringOff bool
	ledCtrl func() led.Controller
	// dotMuted is set externally to block dot button events while muted
	onMuteChange func(muted bool)
	// persist, when set, is called after every Toggle() so the mute state
	// survives reboots and OTA restarts (state.json). Separate from
	// onMuteChange: that one is the controller-notification hook wired by
	// cmd, this one is internal.
	persist func()
}

func newMuteController(ledGetter func() led.Controller, onMuteChange func(muted bool)) *muteController {
	return &muteController{
		ledCtrl:      ledGetter,
		onMuteChange: onMuteChange,
	}
}

// SetOnMuteChange wires a callback invoked when mute state changes.
// B7 fix (2026-07-05 review): previously Server.SetMuteChangeCallback
// reached directly into m.mu/m.onMuteChange from outside this struct.
// Encapsulating the lock here keeps muteController responsible for its
// own synchronisation, matching every other muteController method.
func (m *muteController) SetOnMuteChange(cb func(muted bool)) {
	m.mu.Lock()
	m.onMuteChange = cb
	m.mu.Unlock()
}

// IsMuted: the microphones are silent, by the button or by Home Assistant.
// Every "may the microphone be used" check reads this.
func (m *muteController) IsMuted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.muted || m.soft
}

// HardMuted is the button's mute alone (the hardware's state).
func (m *muteController) HardMuted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.muted
}

// SoftMuted is Home Assistant's mute alone.
func (m *muteController) SoftMuted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.soft
}

// SetSoft turns Home Assistant's mute on or off. It never touches the
// button's: while that is on the Echo stays muted whatever this says.
func (m *muteController) SetSoft(on bool) {
	m.mu.Lock()
	if m.soft == on {
		m.mu.Unlock()
		return
	}
	m.soft = on
	hard := m.muted
	cb := m.onMuteChange
	persist := m.persist
	m.mu.Unlock()
	softmute.Set(on)
	log.Printf("Mute: soft mute %v (Home Assistant)", on)
	if !hard {
		if on {
			m.showMuteLEDs()
		} else {
			m.clearLEDs()
		}
	}
	if persist != nil {
		persist()
	}
	if cb != nil {
		cb(on || hard)
	}
}

// RestoreSoft re-applies a persisted soft mute at boot (the LEDs are
// painted once they exist, as for the hard one).
func (m *muteController) RestoreSoft() {
	m.mu.Lock()
	m.soft = true
	m.mu.Unlock()
	softmute.Set(true)
	log.Println("Mute: restoring persisted soft mute")
}

// muteHard turns the hardware mute on (the privacy driver's reconcile).
func (m *muteController) muteHard() {
	m.mu.Lock()
	if m.muted {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.Toggle()
}

// Toggle is the button. Muted for either reason, it unmutes both: the
// person at the Echo has the last word over Home Assistant's mute. Else it
// mutes in hardware.
func (m *muteController) Toggle() {
	m.mu.Lock()
	wasSoft := m.soft
	m.muted = !(m.muted || m.soft)
	m.soft = false
	muted := m.muted
	// Copy under the lock — SetOnMuteChange writes this field under mu from
	// the main goroutine, and button events can fire before that wiring
	// completes (SubscribeToButton starts the evdev goroutines first).
	cb := m.onMuteChange
	persist := m.persist
	m.mu.Unlock()

	if wasSoft {
		softmute.Set(false)
	}
	if muted {
		m.applyMute()
	} else {
		m.applyUnmute()
	}
	if persist != nil {
		persist()
	}

	if cb != nil {
		cb(muted)
	}
	m.reconcilePrivacySoon()
}

// setAdcMute writes the board's microphone mutes (pkg/board MicMutes), every
// one of them: only chip A was muted before 2026-07-07, which left the wake
// word's ch6 physically hot, and the mic stream-stop was what made mute
// effective. By name since 2026-09-17 (#546). It reports every failure, not
// just the first per control: this is the hardware half of the mute, and a
// silent miss here is a hot microphone.
func setAdcMute(val string) {
	ctls := board.CurrentLayout().MicMutes
	failed := 0
	for _, ctl := range ctls {
		if mixer.Set(ctl, val) != nil {
			failed++
		}
	}
	if failed > 0 {
		log.Printf("Mute: %d of %d ADC mute controls failed to set %s", failed, len(ctls), val)
	}
}

// RestoreMuted re-applies a persisted muted state at boot: flag + ADC mute
// only. The LED hardware isn't up yet when this runs (NewServer, before the
// LED-init goroutine finishes), so the red ring and button LED are painted
// by that goroutine once the controllers exist.
func (m *muteController) RestoreMuted() {
	m.mu.Lock()
	m.muted = true
	m.mu.Unlock()
	log.Println("Mute: restoring persisted muted state")
	setAdcMute("1")
}

func (m *muteController) applyMute() {
	log.Println("Mute: mic muted")
	setAdcMute("1")
	m.showMuteLEDs()
	setMuteButtonLED(true)
}

func (m *muteController) applyUnmute() {
	log.Println("Mute: mic unmuted")
	setAdcMute("0")
	m.clearLEDs()
	setMuteButtonLED(false)
}

// setMuteButtonLED drives the discrete red LED under the mic-off button —
// stock-Alexa parity: the button itself shows muted, not just the ring.
// GPIO-backed and independent of the ring driver, so it needs no repaint
// protection (ring repaints can't stomp it) and survives every LED-mode
// transition for free. Direct binding call, same precedent as setAdcMute's
// tinymix exec above.
func setMuteButtonLED(on bool) {
	if err := internalLed.SetMuteButtonLED(on); err != nil {
		log.Printf("Mute button LED: %v", err)
	}
}

func (m *muteController) showMuteLEDs() {
	lc := m.ledCtrl()
	if lc == nil {
		return
	}
	m.mu.Lock()
	// Off only under the hard mute: the soft one does not light the button,
	// so the ring is its only sign.
	off := m.ringOff && hasMuteButtonLED() && m.muted
	m.mu.Unlock()
	if off {
		// The lit mute button says it; the ring stays dark, and nothing else
		// paints it while muted (suppressPaint), as under the red ring.
		clearLeds(lc)
		return
	}
	leds := make([]led.Led, numLEDs)
	for i := 0; i < numLEDs; i++ {
		leds[i] = led.Led{ID: i, R: 180, G: 0, B: 0} // red ring
	}
	if err := lc.SetLEDs(leds...); err != nil {
		log.Printf("Mute LED set failed: %v", err)
	}
}

func (m *muteController) clearLEDs() {
	lc := m.ledCtrl()
	if lc == nil {
		return
	}
	clearLeds(lc)
}
