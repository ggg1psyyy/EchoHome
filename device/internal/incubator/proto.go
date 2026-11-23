// Package incubator speaks the Voice Incubator satellite protocol, version 2,
// as OHF-Voice/firmware-voice-incubator's PROTOCOL.md writes it (commit
// 64ce6c2aa9f17b779293626a76d36fd9169e06d8): the app connects to the
// satellite over TCP, and every message is one frame of a type byte, a
// big-endian 16-bit length of at most 4096, and a payload, which is raw
// 16 kHz S16LE mono PCM for audio and a JSON object for everything else.
//
// This file is the codec only: frames, message types and payloads. It is
// held to the spec's edges and to the app's own protocol.py
// (testdata/interop_*).
package incubator

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// Protocol is the version this package speaks. HELLO always says it: the
	// app reads a HELLO without one as protocol 1, a Voice PE with a second
	// capture and mixednet wake words, which an Echo is not. Protocol 3 added
	// HELLO's output_sample_rates and CONFIG (AUDIO_OUT at 48 kHz).
	Protocol = 3
	// MaxPayload is the largest payload a frame carries.
	MaxPayload = 4096
	// SampleRate, mono S16LE, in both directions -- AUDIO_OUT too unless the
	// app chose OutputRate (CONFIG).
	SampleRate = 16000
	// OutputRate is the speaker's own rate, which AUDIO_OUT may come in
	// (protocol 3): played as it comes, not upsampled from 16 kHz, so a reply
	// keeps what lies above 8 kHz.
	OutputRate = 48000
	// BytesPerSecond of audio.
	BytesPerSecond = SampleRate * 2
	// DefaultPort is where a satellite listens unless configured otherwise.
	DefaultPort = 6055
	// ServiceType is the mDNS service a satellite advertises, with TXT
	// records mac and protocol.
	ServiceType = "_voice-incubator._tcp"
	headerSize  = 3
)

// Message is a frame's type.
type Message uint8

// Every message of protocol 2 (PROTOCOL.md, Frames).
const (
	Hello           Message = 0x01
	CallStart       Message = 0x02
	AudioIn         Message = 0x03
	CallEnd         Message = 0x04
	Status          Message = 0x05
	PlaybackDrained Message = 0x06
	ModelResult     Message = 0x07
	AudioInAEC      Message = 0x08
	DeviceEvent     Message = 0x09
	AudioMonitor    Message = 0x0A
	WakeReport      Message = 0x0B
	AudioMonitorAEC Message = 0x0C

	AudioOut    Message = 0x10
	AudioOutEnd Message = 0x11
	Event       Message = 0x12
	Hangup      Message = 0x13
	Monitor     Message = 0x14
	Configure   Message = 0x15 // CONFIG (protocol 3)
	ModelStart  Message = 0x20
	ModelData   Message = 0x21
	ModelEnd    Message = 0x22
	ModelClear  Message = 0x23
)

// FromSatellite reports whether m is sent by a satellite.
func (m Message) FromSatellite() bool { return m >= Hello && m <= AudioMonitorAEC }

// FromApp reports whether m is sent by the app.
func (m Message) FromApp() bool {
	return (m >= AudioOut && m <= Configure) || (m >= ModelStart && m <= ModelClear)
}

// Binary reports whether m's payload is raw bytes (audio, or a model blob's
// part) rather than JSON.
func (m Message) Binary() bool {
	switch m {
	case AudioIn, AudioInAEC, AudioMonitor, AudioMonitorAEC, AudioOut, ModelData:
		return true
	}
	return false
}

// Payload-free messages: a JSON payload is optional for these.
func (m Message) empty() bool {
	return m == PlaybackDrained || m == AudioOutEnd || m == ModelClear
}

var (
	// ErrTooLong is a frame whose length passes MaxPayload: the stream can
	// no longer be trusted, so the connection should close.
	ErrTooLong = errors.New("incubator: frame longer than 4096 bytes")
	// ErrNotObject is a JSON payload that is not an object.
	ErrNotObject = errors.New("incubator: payload is not a JSON object")
)

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, m Message, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrTooLong
	}
	var h [headerSize]byte
	h[0] = byte(m)
	binary.BigEndian.PutUint16(h[1:], uint16(len(payload)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSON writes v as one frame's JSON payload.
func WriteJSON(w io.Writer, m Message, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, m, b)
}

// WriteAudio writes pcm as as many frames as it takes, each a whole number
// of samples.
func WriteAudio(w io.Writer, m Message, pcm []byte) error {
	for len(pcm) > 0 {
		n := min(len(pcm), MaxPayload)
		if err := WriteFrame(w, m, pcm[:n]); err != nil {
			return err
		}
		pcm = pcm[n:]
	}
	return nil
}

// Reader reads frames. The payload it returns is valid until the next read.
type Reader struct {
	r   *bufio.Reader
	buf [MaxPayload]byte
}

// NewReader reads frames from r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReaderSize(r, 2*MaxPayload)} }

// Read returns the next frame. A length past MaxPayload is ErrTooLong, and
// a stream that ends mid-frame io.ErrUnexpectedEOF; a clean end between
// frames is io.EOF.
func (r *Reader) Read() (Message, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r.r, h[:]); err != nil {
		return 0, nil, err // io.EOF between frames, io.ErrUnexpectedEOF inside one
	}
	n := int(binary.BigEndian.Uint16(h[1:]))
	if n > MaxPayload {
		return 0, nil, ErrTooLong
	}
	p := r.buf[:n]
	if _, err := io.ReadFull(r.r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return Message(h[0]), p, nil
}

// Decode unmarshals a JSON payload into v. Unknown fields are ignored, as
// the spec says; an empty payload is accepted for messages that carry none.
func Decode(m Message, payload []byte, v any) error {
	if len(payload) == 0 && m.empty() {
		return nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil || probe == nil {
		return fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	return json.Unmarshal(payload, v)
}

// HelloInfo is HELLO: what the satellite is and can do. A capability is said
// only once it is implemented: absent fields are false or unknown to the app.
type HelloInfo struct {
	Protocol       int    `json:"protocol"`
	Name           string `json:"name"`
	MAC            string `json:"mac"`
	Model          string `json:"model,omitempty"`
	Firmware       string `json:"firmware,omitempty"`
	SampleRate     int    `json:"sample_rate"`
	Channels       int    `json:"channels"`
	BitsPerSample  int    `json:"bits_per_sample"`
	PrerollMs      int    `json:"preroll_ms"`
	OutputBuffer   int    `json:"output_buffer_bytes,omitempty"`
	AEC            bool   `json:"aec,omitempty"`
	AECLeadSamples *int   `json:"aec_lead_samples,omitempty"`
	MonitorAEC     bool   `json:"monitor_aec,omitempty"`
	WakeFormat     string `json:"wake_format,omitempty"`
	ModelSlots     int    `json:"model_slots,omitempty"`
	WakeModels     *int   `json:"wake_models,omitempty"`
	FeatureSize    int    `json:"feature_size,omitempty"`
	MaxPool        bool   `json:"max_pool,omitempty"`
	StopWord       bool   `json:"stop_word,omitempty"`
	Listen         bool   `json:"listen,omitempty"`
	Monitor        bool   `json:"monitor,omitempty"`
	Intercom       bool   `json:"intercom,omitempty"`
	// OutputSampleRates are the rates AUDIO_OUT may come in (protocol 3).
	OutputSampleRates []int `json:"output_sample_rates,omitempty"`
}

// ConfigInfo is CONFIG, from the app once after HELLO and before any audio:
// AUDIO_OUT at OutputSampleRate from then on, for this connection.
type ConfigInfo struct {
	OutputSampleRate int `json:"output_sample_rate"`
}

// MarshalJSON always says Protocol, whatever the struct holds: a zero
// HelloInfo would otherwise say 0, which the app reads as protocol 1.
func (h HelloInfo) MarshalJSON() ([]byte, error) {
	type plain HelloInfo
	h.Protocol = Protocol
	return json.Marshal(plain(h))
}

// NewHello is a HELLO with what every satellite of this protocol says:
// version, name, MAC and the only audio format. Capabilities start false.
func NewHello(name, mac string) HelloInfo {
	return HelloInfo{Protocol: Protocol, Name: name, MAC: mac, SampleRate: SampleRate, Channels: 1, BitsPerSample: 16,
		OutputSampleRates: []int{SampleRate, OutputRate}}
}

// CallStartInfo is CALL_START.
type CallStartInfo struct {
	WakeWord        string `json:"wake_word"` // a pushed model's id, or manual, follow_up, stop
	PrerollBytes    int    `json:"preroll_bytes"`
	AECPrerollBytes *int   `json:"aec_preroll_bytes,omitempty"`
	Probability     *int   `json:"probability,omitempty"` // 0-255: the window's mean
	MaxProbability  *int   `json:"max_probability,omitempty"`
	Cutoff          *int   `json:"cutoff,omitempty"`
}

// The wake_word of a call no model started.
const (
	WakeManual   = "manual"
	WakeFollowUp = "follow_up"
	WakeStop     = "stop"
)

// CallEndInfo is CALL_END: the satellite ended the call.
type CallEndInfo struct {
	Reason string `json:"reason"` // timeout, cancelled
}

// HangupInfo is HANGUP: the app ended the call; with Listen, the satellite
// opens the call that hears the answer.
type HangupInfo struct {
	Reason string `json:"reason"`
	Listen bool   `json:"listen"`
}

// EventInfo is EVENT: what is happening, to show. Data is kept raw; a
// satellite reads what it shows.
type EventInfo struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// DeviceEventInfo is DEVICE_EVENT.
type DeviceEventInfo struct {
	Type string `json:"type"` // announcing, announced, stop_timer, intercom_end
}

// StatusInfo is STATUS, sent now and then during a call for diagnosis.
type StatusInfo struct {
	FreeBytes       int  `json:"free_bytes"`
	QueuedBytes     int  `json:"queued_bytes"`
	QueuedMs        int  `json:"queued_ms"`
	SpeakerBusy     bool `json:"speaker_busy"`
	MicDroppedBytes int  `json:"mic_dropped_bytes"`
	AECDroppedBytes int  `json:"aec_dropped_bytes"`
}

// ModelStartInfo is MODEL_START: a wake word model's header.
type ModelStartInfo struct {
	Format            string  `json:"format"`
	Slot              int     `json:"slot"`
	ID                string  `json:"id"`
	WakeWord          string  `json:"wake_word"`
	Kind              string  `json:"kind"` // wake, vad
	Size              int     `json:"size"`
	SHA256            string  `json:"sha256"`
	ProbabilityCutoff float64 `json:"probability_cutoff"`
	SlidingWindowSize int     `json:"sliding_window_size"`
	FeatureStepSize   int     `json:"feature_step_size"`
}

// ModelEndInfo is MODEL_END.
type ModelEndInfo struct {
	ID string `json:"id"`
}

// ModelResultInfo is MODEL_RESULT, the satellite's answer to each model.
type ModelResultInfo struct {
	OK     bool   `json:"ok"`
	ID     string `json:"id"`
	Models int    `json:"models"`
	Error  string `json:"error,omitempty"`
}

// MonitorInfo is MONITOR: stream outside calls (Active), as the intercom.
type MonitorInfo struct {
	Active   bool `json:"active"`
	Intercom bool `json:"intercom"`
}
