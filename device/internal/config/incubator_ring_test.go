package config

import (
	"encoding/json"
	"testing"
)

func TestIncubatorRingRoundTripsAndSurvivesASparsePush(t *testing.T) {
	d := &Device{}
	spec := json.RawMessage(`{"spin":{"pattern":"spin"}}`)
	d.Apply(ConfigMessage{Type: "config", IncubatorRing: spec})
	if string(d.Snapshot().IncubatorRing) != string(spec) {
		t.Fatalf("got %s", d.Snapshot().IncubatorRing)
	}
	d.Apply(ConfigMessage{Type: "config", ListeningAnim: json.RawMessage(`{"pattern":"solid"}`)})
	if string(d.Snapshot().IncubatorRing) != string(spec) {
		t.Fatal("a push without it cleared it")
	}
}
