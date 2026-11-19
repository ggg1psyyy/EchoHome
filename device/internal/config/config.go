// Package config provides a shared, concurrency-safe device configuration
// that can be updated at runtime when the controller pushes a config message.
//
// Both the control client (OWW threshold) and the data client (VAD params)
// read from this struct so changes take effect immediately without a restart.
package config

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/wilbowes/EchoMuse/internal/outchain"
)

// Device holds all runtime-tunable parameters for this device.
// Zero values are replaced by defaults on first access via Get().
type Device struct {
	mu sync.RWMutex

	// Microphone / VAD

	// Speaker
	StartupVolume     int
	VolumeButtonSound bool

	// Wake word
	// DuckDb is how far MUSIC is attenuated while a voice turn plays over
	// it, in dB (negative = quieter). Config rather than a constant because
	// it is a taste parameter that needs iterating in a real room, the same
	// reasoning as the LED meter response curve — not something to discover
	// via a firmware OTA per attempt.
	DuckDb float64
	// ResponseLevel is the relative gain for the voice stream: low (0dB),
	// medium (+6dB), or high (+12dB). The speaker caps it against the device
	// volume so their combined gain never exceeds unity.
	ResponseLevel string

	// RemoteVolumeArc shows the cyan volume arc when a live remote command
	// changes the volume. Physical buttons always show it; the boot-time
	// volume restore never does. Off by default because an unprompted ring was
	// found distracting, but useful as an opt-in accessibility setting (#634).
	RemoteVolumeArc bool

	// MuteRing shows the red ring while muted. Off, the ring stays dark and
	// the lit mute button alone says it: honoured only on an Echo whose
	// button lights itself (capability mute_ring_off).
	MuteRing bool

	// ADC gain — applied via tinymix when config is pushed
	AdcDigitalGain int
	AdcMicpga      int

	// MicGainDb is a fixed digital gain (dB) applied to the full 24-bit
	// capture before quantising to the 16-bit stream (see beamformer
	// extractChannel). Measured speech at normal levels sits at 0.0001–
	// 0.0006 FS RMS — only ~3–20 LSB in 16-bit terms — so gain must be
	// applied pre-truncation to recover real captured resolution rather
	// than amplify 16-bit quantisation noise. Fixed by design: this is
	// the "fixed gain" stage of the dumb-transducer architecture — all
	// adaptation lives controller-side as measurement. 0 = unity.
	MicGainDb int

	// WakeMic picks the mic the unlocked path (wake word listening, and
	// turns with beamforming off) reads. 0 = the centre mic, the default and
	// right for nearly every unit; 1-6 = perimeter mic MK1-MK6. An escape
	// hatch for a dead centre mic (#705), not a tuning knob.
	WakeMic int

	// AGC toggle — pointer typed so false is expressible over the wire.
	// Defaults true; applies to bounded lockMic turn streams only (forced
	// off on the always-on wake stream). RNNoise NS was removed 2026-07-12 —
	// noise suppression lives controller-side (em_ns.py) on the ASR path.

	// Acoustic echo cancellation (speexdsp, internal/aec). Applies to the
	// whole mic path (wake stream included) — defaults off until validated
	// per deployment. AecDelayMs is the bulk write-to-ear latency the
	// reference stream is shifted by; measured on hardware (2026-07-08)
	// the right value is 0 — the mic side reads whole 160ms ALSA batches
	// (see GetAudioStream), which eats most of the speaker's ≈340ms output
	// buffering, and the filter tail absorbs the remainder. Values ≥100
	// made the echo arrive before its reference (non-causal → zero
	// cancellation). AecTailMs is the adaptive filter length, which must
	// cover residual delay error plus room reverb. Device clamps: delay
	// 0–1000ms, tail 50–500ms.
	AecEnabled *bool
	AecDelayMs int
	AecTailMs  int

	// AecRefSource picks where the far-end reference comes from: "auto",
	// "hw" or "sw".
	//
	// It is an OVERRIDE for the detection, not a statement about the board
	// — the same shape as OwwOnDevice, and config rather than an env var
	// for the same reason. "auto" detects the hardware loopback and falls
	// back to the software tap on a board without one, which is right
	// almost always; "hw" and "sw" pin it, so the two paths can be
	// A/B'd from the dashboard.
	//
	// This started as EM_AEC_HW_REF, on the argument that the reference is
	// a property of the board rather than a user preference. That was
	// wrong: the board property is already DETECTED, and what a person
	// needs to set is which answer to trust — which cannot be a device
	// env var, because changing one means an edit to start_server.sh on
	// the device and a server restart. Making the measurement expensive is
	// how it stays unmeasured.
	AecRefSource string

	// BLE proxy (passive scan over /dev/stpbt, internal/bluetooth) —
	// pointer typed so false is expressible over the wire. Default off.
	BleProxyEnabled *bool
	// Whether Home Assistant may open connections through the proxy (#656).
	// Separate from the scan, and default off: a connection can operate the
	// device at the other end.
	BleProxyConnections *bool

	// Sendspin player (internal/sendspin, #89). Both default off: the player
	// opens a listening port and an mDNS record, so it runs only where
	// someone asked for it. Unpaired lets an approved but unpaired server
	// play; pairing needs only the device's token pasted into the server.
	SendspinEnabled  *bool
	SendspinUnpaired *bool
	// SendspinName is what Music Assistant lists the player as: the
	// device's label on the dashboard, which the firmware does not otherwise
	// know. Empty falls back to one built from the serial.
	SendspinName string
	// DhcpHostname is the name the Echo gives its DHCP server: its label as
	// a DNS label plus the last four characters of its serial (the
	// controller's em_labels.hostname). Empty leaves emOS's DHCP client as
	// it is.
	DhcpHostname string

	// ListeningAnim carries the controller's current listening-ring
	// animation spec, raw JSON in the led_anim shape, so the device can
	// light it locally at its OWN wake crossing (#263) instead of waiting
	// a controller round trip for the authoritative frame. Nil until the
	// controller sends one; a device that has never received it simply
	// keeps the old behaviour.
	ListeningAnim json.RawMessage

	// IncubatorRing carries the rest of the scene for the Voice Incubator
	// satellite the Echo runs itself (internal/incubator), raw JSON: an
	// object of led_anim specs, "spin" (thinking), "meter" (a reply or an
	// announcement sounding), "done", "error" and "timer" (a timer
	// ringing), each optional. Nil until the controller sends it (only to
	// firmware announcing incubator_ring); a state with no spec goes dark.
	IncubatorRing json.RawMessage

	// Output is the speaker output chain's configuration (eqBands,
	// eqLoudness, bassGuard*, limiter*). Held here whether or not the
	// controller has handed the chain to this device, so the values are
	// already correct the moment it does. Read with OutputChain().
	Output outchain.Params

	initialised bool
}

var global = &Device{}

// Get returns the global device config, initialised from environment
// variables on first call.
func Get() *Device {
	global.mu.Lock()
	defer global.mu.Unlock()
	if !global.initialised {
		global.loadDefaults()
		global.initialised = true
	}
	return global
}

// loadDefaults populates from environment variables, falling back to
// hard-coded defaults. Must be called with mu held.
func (d *Device) loadDefaults() {
	d.StartupVolume = envInt("STARTUP_VOLUME", 85)
	d.VolumeButtonSound = envBool("VOLUME_BUTTON_SOUND", true)
	d.DuckDb = envFloat("DUCK_DB", -18)
	d.ResponseLevel = normaliseResponseLevel(envStr("RESPONSE_LEVEL", ResponseLevelLow))
	d.RemoteVolumeArc = envBool("REMOTE_VOLUME_ARC", false)
	d.MuteRing = envBool("MUTE_RING", true)
	d.AdcDigitalGain = envInt("ADC_DIGITAL_GAIN", 88)
	d.AdcMicpga = envInt("ADC_MICPGA", 40)
	d.MicGainDb = clampMicGainDb(envInt("MIC_GAIN_DB", 24))
	d.WakeMic = clampWakeMic(envInt("WAKE_MIC", 0))
	// true to match em_db.DEFAULT_DEVICE_CONFIG, which now defaults AEC on
	// because barge-in does. The controller's value reaches us on the first
	// config push either way; this only governs the window before it.
	aecEnabled := envBool("AEC_ENABLED", true)
	d.AecEnabled = &aecEnabled
	d.AecDelayMs = envInt("AEC_DELAY_MS", 0)
	d.AecTailMs = envInt("AEC_TAIL_MS", 300)
	// EM_AEC_HW_REF keeps working as the boot default for a device with no
	// controller to push config, and is superseded the moment one does.
	d.AecRefSource = normaliseAecRef(envStr("EM_AEC_HW_REF", AecRefAuto))
	bleProxyEnabled := envBool("BLE_PROXY_ENABLED", false)
	d.BleProxyEnabled = &bleProxyEnabled
	sendspinEnabled, sendspinUnpaired := false, false
	d.SendspinEnabled, d.SendspinUnpaired = &sendspinEnabled, &sendspinUnpaired
	d.Output = outchain.DefaultParams()
}

// Apply updates the config from a controller-pushed config message.
// Only non-zero / non-empty values from the message are applied so that
// a partial config push doesn't zero out unmentioned fields.
func (d *Device) Apply(msg ConfigMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.initialised {
		d.loadDefaults()
		d.initialised = true
	}

	// Negative-going, so the usual "non-zero means set" rule is inverted:
	// a duck of 0dB is a legitimate setting ("do not duck at all") and must
	// be distinguishable from an absent field, hence the pointer.
	if msg.DuckDb != nil {
		d.DuckDb = *msg.DuckDb
	}
	if msg.ResponseLevel != "" {
		d.ResponseLevel = normaliseResponseLevel(msg.ResponseLevel)
	}
	if msg.VolumeButtonSound != nil {
		d.VolumeButtonSound = *msg.VolumeButtonSound
	}
	if msg.RemoteVolumeArc != nil {
		d.RemoteVolumeArc = *msg.RemoteVolumeArc
	}
	if msg.MuteRing != nil {
		d.MuteRing = *msg.MuteRing
	}
	if msg.StartupVolume > 0 {
		d.StartupVolume = msg.StartupVolume
	}
	if msg.AdcDigitalGain != nil {
		d.AdcDigitalGain = *msg.AdcDigitalGain
	}
	if msg.AdcMicpga != nil {
		d.AdcMicpga = *msg.AdcMicpga
	}
	if msg.MicGainDb != nil {
		d.MicGainDb = clampMicGainDb(*msg.MicGainDb)
	}
	if msg.WakeMic != nil {
		d.WakeMic = clampWakeMic(*msg.WakeMic)
	}
	if msg.AecEnabled != nil {
		d.AecEnabled = msg.AecEnabled
	}
	if msg.AecDelayMs != nil {
		d.AecDelayMs = *msg.AecDelayMs
	}
	if msg.AecTailMs > 0 {
		d.AecTailMs = msg.AecTailMs
	}
	if msg.AecRefSource != "" {
		d.AecRefSource = normaliseAecRef(msg.AecRefSource)
	}
	if msg.BleProxyEnabled != nil {
		d.BleProxyEnabled = msg.BleProxyEnabled
	}
	if msg.BleProxyConnections != nil {
		d.BleProxyConnections = msg.BleProxyConnections
	}
	if msg.SendspinEnabled != nil {
		d.SendspinEnabled = msg.SendspinEnabled
	}
	if msg.SendspinUnpaired != nil {
		d.SendspinUnpaired = msg.SendspinUnpaired
	}
	if msg.SendspinName != "" {
		d.SendspinName = msg.SendspinName
	}
	if msg.DhcpHostname != "" {
		d.DhcpHostname = msg.DhcpHostname
	}
	if msg.ListeningAnim != nil {
		d.ListeningAnim = msg.ListeningAnim
	}
	if msg.IncubatorRing != nil {
		d.IncubatorRing = msg.IncubatorRing
	}
	applyOutput(&d.Output, msg)
}

// VolumeButtonSoundEnabled reports whether physical volume changes should
// play their audible preview. The caller still decides whether playback is
// idle; config owns only the preference.
func (d *Device) VolumeButtonSoundEnabled() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.VolumeButtonSound
}

// MuteRingOn reports whether the red ring shows while muted.
func (d *Device) MuteRingOn() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.MuteRing
}

// RemoteVolumeArcEnabled reports whether live remote volume changes should
// show the same cyan level arc as the physical buttons.
func (d *Device) RemoteVolumeArcEnabled() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.RemoteVolumeArc
}

const (
	ResponseLevelLow    = "low"
	ResponseLevelMedium = "medium"
	ResponseLevelHigh   = "high"
)

func normaliseResponseLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case ResponseLevelMedium:
		return ResponseLevelMedium
	case ResponseLevelHigh:
		return ResponseLevelHigh
	default:
		return ResponseLevelLow
	}
}

// ResponseGainDB returns the configured relative voice-stream gain. Unknown
// values fail safely to today's 0dB behaviour.
func (d *Device) ResponseGainDB() float64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	switch d.ResponseLevel {
	case ResponseLevelMedium:
		return 6
	case ResponseLevelHigh:
		return 12
	default:
		return 0
	}
}

// applyOutput merges the output-chain keys. Every one of them has a
// legitimate zero — a flat band, a 0dBFS threshold, "off" — so each is a
// pointer (or a slice) and absent means untouched. eqBands shorter than
// NumBands pads with 0, as em_eq does; longer is truncated.
func applyOutput(p *outchain.Params, msg ConfigMessage) {
	if msg.EqBands != nil {
		var b [outchain.NumBands]float64
		copy(b[:], msg.EqBands)
		p.Bands = b
	}
	if msg.EqLoudness != nil {
		p.Loudness = *msg.EqLoudness
	}
	if msg.BassGuardEnabled != nil {
		p.GuardEnabled = *msg.BassGuardEnabled
	}
	if msg.BassGuardDb != nil {
		p.GuardDb = *msg.BassGuardDb
	}
	if msg.LimiterEnabled != nil {
		p.LimiterEnabled = *msg.LimiterEnabled
	}
	if msg.LimiterThreshold != nil {
		p.LimiterThresholdDb = *msg.LimiterThreshold
	}
	if msg.LimiterRelease != nil {
		p.LimiterReleaseMs = *msg.LimiterRelease
	}
}

// OutputChain returns the output chain's current configuration.
func (d *Device) OutputChain() outchain.Params {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.Output
}

// Snapshot returns a consistent copy of all config values.
func (d *Device) Snapshot() ConfigMessage {
	d.mu.RLock()
	defer d.mu.RUnlock()
	wakeMic := d.WakeMic
	// Copied to locals, never pointed into the mutex-guarded struct: a caller
	// dereferences after RUnlock, racing with Apply() on a config push (C4,
	// 2026-07-05 review).
	micGainDb := d.MicGainDb
	adcDigitalGain := d.AdcDigitalGain
	adcMicpga := d.AdcMicpga
	aecEnabled := false
	if d.AecEnabled != nil {
		aecEnabled = *d.AecEnabled
	}
	aecDelayMs := d.AecDelayMs
	bleProxyEnabled := false
	if d.BleProxyEnabled != nil {
		bleProxyEnabled = *d.BleProxyEnabled
	}
	bleProxyConnections := d.BleProxyConnections != nil && *d.BleProxyConnections
	volumeButtonSound := d.VolumeButtonSound
	sendspinEnabled := d.SendspinEnabled != nil && *d.SendspinEnabled
	sendspinUnpaired := d.SendspinUnpaired != nil && *d.SendspinUnpaired
	remoteVolumeArc := d.RemoteVolumeArc
	muteRing := d.MuteRing
	return ConfigMessage{
		ResponseLevel:       d.ResponseLevel,
		StartupVolume:       d.StartupVolume,
		VolumeButtonSound:   &volumeButtonSound,
		AdcDigitalGain:      &adcDigitalGain,
		AdcMicpga:           &adcMicpga,
		MicGainDb:           &micGainDb,
		WakeMic:             &wakeMic,
		AecEnabled:          &aecEnabled,
		AecDelayMs:          &aecDelayMs,
		AecTailMs:           d.AecTailMs,
		AecRefSource:        d.AecRefSource,
		BleProxyEnabled:     &bleProxyEnabled,
		BleProxyConnections: &bleProxyConnections,
		SendspinEnabled:     &sendspinEnabled,
		SendspinUnpaired:    &sendspinUnpaired,
		SendspinName:        d.SendspinName,
		DhcpHostname:        d.DhcpHostname,
		RemoteVolumeArc:     &remoteVolumeArc,
		MuteRing:            &muteRing,
		ListeningAnim:       d.ListeningAnim,
		IncubatorRing:       d.IncubatorRing,
	}
}

// ConfigMessage mirrors the JSON shape of the config control message
// sent by the controller. JSON tags must match em_controller.py exactly.
type ConfigMessage struct {
	Type string `json:"type,omitempty"`
	// Pointer typed so 0 is expressible. Both are raw tinymix control
	// values and 0 is the bottom of each control's own range — a legitimate
	// setting, and the one somebody reaches for in a loud room. Under the
	// "non-zero means set" rule they were silently ignored: the dashboard
	// slider offers 0, the config stored 0, and the device carried on at
	// whatever gain it already had.
	AdcDigitalGain *int `json:"adcDigitalGain,omitempty"`
	AdcMicpga      *int `json:"adcMicpga,omitempty"`
	MicGainDb      *int `json:"micGainDb,omitempty"`
	StartupVolume  int  `json:"startupVolume,omitempty"`
	// ConsolePassword is the hashed record emOS's init checks before handing
	// over a shell on the USB serial console. A POINTER, and it has to be: an
	// EMPTY record is the legitimate "no password" setting, so with a plain
	// string plus omitempty a removal would be indistinguishable from a field
	// nobody sent, and clearing the password could never reach a device.
	// Same reason DuckDb below is a pointer.
	//
	// Consumed by the firmware only to write it to disk for init — the
	// firmware never checks it, because the console must work when the
	// firmware is not running. Ignored on FireOS, which uses adbd.
	ConsolePassword *string `json:"consolePassword,omitempty"`
	// ConsoleTimeoutMin is the emOS console idle timeout in MINUTES: 0 for no
	// timeout, otherwise 1-90. A POINTER for ConsolePassword's reason — zero
	// is the legitimate "no timeout" setting, so with omitempty it would be
	// indistinguishable from a field nobody sent and could never be turned
	// off once on.
	//
	// Minutes because that is the unit it is chosen in. `TMOUT` is seconds;
	// init multiplies when it builds the shell's environment, so the stored
	// value, the pushed value and the number on screen all agree.
	//
	// Written to disk for init like the password above, and ignored on
	// FireOS, which uses adbd.
	ConsoleTimeoutMin *int `json:"consoleTimeoutMin,omitempty"`
	// A pointer because false ("No wake word") is the value that matters.
	DuckDb              *float64 `json:"duckDb,omitempty"`
	ResponseLevel       string   `json:"responseLevel,omitempty"`
	HasBeamforming      bool     `json:"hasBeamforming,omitempty"`
	WakeMic             *int     `json:"wakeMic,omitempty"`
	AecEnabled          *bool    `json:"aecEnabled,omitempty"`
	AecDelayMs          *int     `json:"aecDelayMs,omitempty"`
	AecTailMs           int      `json:"aecTailMs,omitempty"`
	AecRefSource        string   `json:"aecRefSource,omitempty"`
	BleProxyEnabled     *bool    `json:"bleProxyEnabled,omitempty"`
	BleProxyConnections *bool    `json:"bleProxyConnections,omitempty"`
	SendspinEnabled     *bool    `json:"sendspinEnabled,omitempty"`
	SendspinUnpaired    *bool    `json:"sendspinUnpaired,omitempty"`
	SendspinName        string   `json:"sendspinName,omitempty"`
	DhcpHostname        string   `json:"dhcpHostname,omitempty"`
	// VolumeButtonSound: a pointer so "off" is distinguishable from absent.
	VolumeButtonSound *bool `json:"volumeButtonSound,omitempty"`
	// RemoteVolumeArc: a pointer so "off" is distinguishable from absent.
	RemoteVolumeArc *bool `json:"remoteVolumeArc,omitempty"`
	// MuteRing: a pointer, as false is meaningful (the ring off while muted).
	MuteRing *bool `json:"muteRing,omitempty"`

	// Output chain (internal/outchain). Pointers because zero is a real
	// setting for every one of them; see applyOutput.
	EqBands          []float64 `json:"eqBands,omitempty"`
	EqLoudness       *bool     `json:"eqLoudness,omitempty"`
	BassGuardEnabled *bool     `json:"bassGuardEnabled,omitempty"`
	BassGuardDb      *float64  `json:"bassGuardDb,omitempty"`
	LimiterEnabled   *bool     `json:"limiterEnabled,omitempty"`
	LimiterThreshold *float64  `json:"limiterThreshold,omitempty"`
	LimiterRelease   *float64  `json:"limiterRelease,omitempty"`

	// ListeningAnim: raw led_anim spec for the listening ring (#263).
	// Carried as raw JSON so this package does not depend on the
	// animation renderer's types.
	ListeningAnim json.RawMessage `json:"listeningAnim,omitempty"`

	// IncubatorRing: the Incubator satellite's other ring specs (Device's).
	IncubatorRing json.RawMessage `json:"incubatorRing,omitempty"`
}

// clampMicGainDb bounds the fixed mic gain to a sane range: 0dB (unity —
// the pre-gain behaviour, bit-exact) up to +42dB. The 24-bit capture holds
// 8 bits (48dB) below the old 16-bit truncation point; beyond +42dB the
// gain is amplifying the capture's own noise floor with no headroom left.
func clampMicGainDb(db int) int {
	if db < 0 {
		return 0
	}
	if db > 42 {
		return 42
	}
	return db
}

// AecRefSource values. "auto" detects the hardware loopback and falls back
// to the software tap; the other two pin it for an A/B.
const (
	AecRefAuto = "auto"
	AecRefHW   = "hw"
	AecRefSW   = "sw"
)

// normaliseAecRef keeps an unknown value on the DETECTING path rather than
// pinning one. A typo that pinned "sw" would silently disable the hardware
// reference on every device it reached, and read as the feature not working.
func normaliseAecRef(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case AecRefHW, "on", "true", "1":
		return AecRefHW
	case AecRefSW, "off", "false", "0":
		return AecRefSW
	case "", AecRefAuto:
		return AecRefAuto
	default:
		log.Printf("[config] unknown aecRefSource %q — detecting", v)
		return AecRefAuto
	}
}

// ─── env helpers ──────────────────────────────────────────────────────────────

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "1" || v == "true" || v == "True"
	}
	return def
}

func envStr(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// clampWakeMic keeps an out-of-range value on the centre mic (0) rather than
// guessing a perimeter mic: the centre is the default for a reason, and a
// typo should land there.
func clampWakeMic(v int) int {
	if v < 0 || v > 6 {
		return 0
	}
	return v
}
