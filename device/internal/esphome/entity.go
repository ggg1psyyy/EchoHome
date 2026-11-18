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
