package esphome

import (
	"sync"

	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"google.golang.org/protobuf/proto"
)

// Entity is one thing the Echo shows Home Assistant. Its key is what HA
// keys its state on, and with its object id what keeps HA's entity (and the
// automations on it) when the Echo takes over from the controller, so both
// are the controller's (em_esphome.py *_KEY, test-pinned).
type Entity interface {
	Key() uint32
	// Info is its ListEntities response.
	Info() proto.Message
	// State is its state response now; nil for one that has none (an
	// event).
	State() proto.Message
	bind(onChange func(Entity))
}

type base struct {
	mu       sync.Mutex
	onChange func(Entity)
}

func (b *base) bind(f func(Entity)) {
	b.mu.Lock()
	b.onChange = f
	b.mu.Unlock()
}

func (b *base) changed(e Entity) {
	b.mu.Lock()
	f := b.onChange
	b.mu.Unlock()
	if f != nil {
		f(e)
	}
}

// Sensor is a number (ListEntitiesSensorResponse). Until Set it is missing
// (missing_state), never 0: a missing measurement is not a zero one.
type Sensor struct {
	base
	Desc  *api.ListEntitiesSensorResponse
	mu    sync.Mutex
	value float32
	have  bool
}

func (s *Sensor) Key() uint32         { return s.Desc.Key }
func (s *Sensor) Info() proto.Message { return s.Desc }

func (s *Sensor) State() proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &api.SensorStateResponse{Key: s.Desc.Key, State: s.value, MissingState: !s.have}
}

// Set records a reading and sends it to subscribed clients if it changed.
func (s *Sensor) Set(v float32) {
	s.mu.Lock()
	same := s.have && s.value == v
	s.value, s.have = v, true
	s.mu.Unlock()
	if !same {
		s.changed(s)
	}
}

// Clear makes it missing again (a sensor that stopped reading).
func (s *Sensor) Clear() {
	s.mu.Lock()
	was := s.have
	s.have = false
	s.mu.Unlock()
	if was {
		s.changed(s)
	}
}

// BinarySensor is on or off (ListEntitiesBinarySensorResponse).
type BinarySensor struct {
	base
	Desc  *api.ListEntitiesBinarySensorResponse
	mu    sync.Mutex
	value bool
	have  bool
}

func (b *BinarySensor) Key() uint32         { return b.Desc.Key }
func (b *BinarySensor) Info() proto.Message { return b.Desc }

func (b *BinarySensor) State() proto.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &api.BinarySensorStateResponse{Key: b.Desc.Key, State: b.value, MissingState: !b.have}
}

func (b *BinarySensor) Set(v bool) {
	b.mu.Lock()
	same := b.have && b.value == v
	b.value, b.have = v, true
	b.mu.Unlock()
	if !same {
		b.changed(b)
	}
}

// Event fires typed events (ListEntitiesEventResponse): the action button.
// It has no state; Server.Fire sends one.
type Event struct {
	base
	Desc *api.ListEntitiesEventResponse
}

func (e *Event) Key() uint32          { return e.Desc.Key }
func (e *Event) Info() proto.Message  { return e.Desc }
func (e *Event) State() proto.Message { return nil }

// Switch is on or off and takes commands (ListEntitiesSwitchResponse). A
// command calls OnCommand, which applies it and calls Set with the state
// that resulted; the state HA shows is only ever what Set reported.
type Switch struct {
	base
	Desc      *api.ListEntitiesSwitchResponse
	OnCommand func(on bool)
	mu        sync.Mutex
	value     bool
	have      bool
}

func (w *Switch) Key() uint32         { return w.Desc.Key }
func (w *Switch) Info() proto.Message { return w.Desc }

func (w *Switch) State() proto.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return &api.SwitchStateResponse{Key: w.Desc.Key, State: w.value}
}

// Set records the state and sends it to subscribed clients if it changed.
func (w *Switch) Set(v bool) {
	w.mu.Lock()
	same := w.have && w.value == v
	w.value, w.have = v, true
	w.mu.Unlock()
	if !same {
		w.changed(w)
	}
}

// Value is the state last Set.
func (w *Switch) Value() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.value
}

// MediaPlayer is the Echo's media player (ListEntitiesMediaPlayerResponse):
// commands go to OnCommand; HA sees the state Set reports.
type MediaPlayer struct {
	base
	Desc      *api.ListEntitiesMediaPlayerResponse
	OnCommand func(*api.MediaPlayerCommandRequest)
	mu        sync.Mutex
	state     api.MediaPlayerState
	volume    float32
	muted     bool
}

func (m *MediaPlayer) Key() uint32         { return m.Desc.Key }
func (m *MediaPlayer) Info() proto.Message { return m.Desc }

func (m *MediaPlayer) State() proto.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state
	if st == api.MediaPlayerState_MEDIA_PLAYER_STATE_NONE {
		st = api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE
	}
	return &api.MediaPlayerStateResponse{Key: m.Desc.Key, State: st, Volume: m.volume, Muted: m.muted}
}

// Set records the state, volume (0-1) and mute, and sends them to
// subscribed clients if any changed.
func (m *MediaPlayer) Set(state api.MediaPlayerState, volume float32, muted bool) {
	m.mu.Lock()
	same := m.state == state && m.volume == volume && m.muted == muted
	m.state, m.volume, m.muted = state, volume, muted
	m.mu.Unlock()
	if !same {
		m.changed(m)
	}
}

// Light is a light (ListEntitiesLightResponse): commands go to OnCommand;
// HA sees the state Set reports.
type Light struct {
	base
	Desc      *api.ListEntitiesLightResponse
	OnCommand func(*api.LightCommandRequest)
	mu        sync.Mutex
	st        *api.LightStateResponse
}

func (l *Light) Key() uint32         { return l.Desc.Key }
func (l *Light) Info() proto.Message { return l.Desc }

func (l *Light) State() proto.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st == nil {
		return &api.LightStateResponse{Key: l.Desc.Key}
	}
	st := proto.Clone(l.st).(*api.LightStateResponse)
	st.Key = l.Desc.Key
	return st
}

// Set records the light's state and sends it if it changed.
func (l *Light) Set(st *api.LightStateResponse) {
	st = proto.Clone(st).(*api.LightStateResponse)
	st.Key = l.Desc.Key
	l.mu.Lock()
	same := l.st != nil && proto.Equal(l.st, st)
	l.st = st
	l.mu.Unlock()
	if !same {
		l.changed(l)
	}
}

// Action is a user-defined action Home Assistant can call
// (ListEntitiesServicesResponse; esphome.<device>_<name> in HA), with no
// arguments. It has no state.
type Action struct {
	base
	Name   string
	ID     uint32
	OnCall func()
}

func (a *Action) Key() uint32 { return a.ID }
func (a *Action) Info() proto.Message {
	return &api.ListEntitiesServicesResponse{Name: a.Name, Key: a.ID}
}
func (a *Action) State() proto.Message { return nil }
