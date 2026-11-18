package esphome

import (
	"fmt"

	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// A message's wire type is its (id) option (api.MessageIDs, generated from
// the descriptor); one by type is any message the API defines.

var byID = func() map[uint32]protoreflect.MessageType {
	m := make(map[uint32]protoreflect.MessageType, len(api.MessageIDs))
	for name, id := range api.MessageIDs {
		t, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name))
		if err != nil {
			panic(fmt.Sprintf("esphome: %s has an id but no type: %v", name, err))
		}
		m[id] = t
	}
	return m
}()

// idOf is a message's wire type.
func idOf(m proto.Message) (uint32, bool) {
	id, ok := api.MessageIDs[string(m.ProtoReflect().Descriptor().Name())]
	return id, ok
}

// Decode is the message a frame carries; ok false for a type the API does
// not define (ignored, as ESPHome does).
func Decode(typ uint32, payload []byte) (proto.Message, bool, error) {
	t, ok := byID[typ]
	if !ok {
		return nil, false, nil
	}
	m := t.New().Interface()
	if err := proto.Unmarshal(payload, m); err != nil {
		return nil, true, err
	}
	return m, true, nil
}

// Encode is a message's frame type and payload.
func Encode(m proto.Message) (uint32, []byte, error) {
	id, ok := idOf(m)
	if !ok {
		return 0, nil, fmt.Errorf("esphome: %s has no id", m.ProtoReflect().Descriptor().Name())
	}
	b, err := proto.Marshal(m)
	return id, b, err
}
