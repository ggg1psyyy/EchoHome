// Package owned keeps the settings the Echo owns: the ones Home Assistant
// changes on the Echo itself (its own ESPHome device's switches), and the
// ones only the Echo has. They must outlive a reboot with no controller.
//
// Since EchoHome the Echo is their only owner. The controller once kept them
// too and reconciled a change made here against its pushes (a pending value,
// reported until carried back); it no longer has them, so a value here is
// simply kept. Seed takes a value from elsewhere (the firmware default, or
// what an older controller pushed) only for a key the Echo has none of yet,
// so an Echo upgraded from that scheme keeps what it had. A file written
// then still reads: its "pending" map is ignored, and its values stand.
package owned

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/wilbowes/EchoMuse/internal/atomicfile"
)

// DefaultPath is where the Echo keeps them.
const DefaultPath = "/data/local/etc/echomuse/settings.json"

type record struct {
	Values map[string]bool `json:"values"`
	// Ints are the settings that are a number, not a switch (the wake word
	// microphone), kept by the same rules.
	Ints map[string]int `json:"ints,omitempty"`
}

// Store is the Echo's owned settings. Safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
	rec  record
}

// Load reads the store at path; unreadable is empty (the firmware's
// defaults then decide, and are seeded).
func Load(path string) *Store {
	s := &Store{path: path, rec: record{Values: map[string]bool{}, Ints: map[string]int{}}}
	if b, err := os.ReadFile(path); err == nil {
		var r record
		if json.Unmarshal(b, &r) == nil {
			if r.Values != nil {
				s.rec.Values = r.Values
			}
			if r.Ints != nil {
				s.rec.Ints = r.Ints
			}
		}
	}
	return s
}

func (s *Store) saveLocked() error {
	b, err := json.Marshal(s.rec)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, b, 0o600)
}

// Value is the key's value in force, if there is one.
func (s *Store) Value(key string) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.rec.Values[key]
	return v, ok
}

// SetLocal keeps a value set on the Echo.
func (s *Store) SetLocal(key string, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Values[key] = v
	return s.saveLocked()
}

// Seed returns the key's kept value, keeping `v` first if it has none.
func (s *Store) Seed(key string, v bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept, ok := s.rec.Values[key]; ok {
		return kept
	}
	s.rec.Values[key] = v
	s.saveLocked()
	return v
}

// IntValue is the numeric key's value in force, if there is one.
func (s *Store) IntValue(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.rec.Ints[key]
	return v, ok
}

// SetLocalInt keeps a number set on the Echo.
func (s *Store) SetLocalInt(key string, v int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Ints[key] = v
	return s.saveLocked()
}

// SeedInt returns the numeric key's kept value, keeping `v` first if it has
// none.
func (s *Store) SeedInt(key string, v int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept, ok := s.rec.Ints[key]; ok {
		return kept
	}
	s.rec.Ints[key] = v
	s.saveLocked()
	return v
}
