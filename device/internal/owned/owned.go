// Package owned keeps the settings the Echo owns: the ones Home Assistant
// can change on the Echo itself (its own ESPHome server's switches), which
// must outlive a reboot with no controller and must not be undone by a
// controller that has not heard of the change yet.
//
// Each has a value (what is in force) and may be pending (changed here and
// not yet acknowledged by the controller). A config push from the controller
// reconciles a key: pending, the push either carries the same value (the
// controller has it: no longer pending) or an older one (ignored: the
// Echo's stands, and is reported again); not pending, the push is the value.
// So the controller stays the record while it exists, and nothing set on the
// Echo is lost to one that was away.
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
	Values  map[string]bool `json:"values"`
	Pending map[string]bool `json:"pending,omitempty"`
}

// Store is the Echo's owned settings. Safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
	rec  record
}

// Load reads the store at path; unreadable is empty (the controller's push
// or the firmware's defaults then decide).
func Load(path string) *Store {
	s := &Store{path: path, rec: record{Values: map[string]bool{}, Pending: map[string]bool{}}}
	if b, err := os.ReadFile(path); err == nil {
		var r record
		if json.Unmarshal(b, &r) == nil {
			if r.Values != nil {
				s.rec.Values = r.Values
			}
			if r.Pending != nil {
				s.rec.Pending = r.Pending
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

// Set records a change made on the Echo: in force, and pending until the
// controller carries it back.
func (s *Store) Set(key string, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Values[key] = v
	s.rec.Pending[key] = v
	return s.saveLocked()
}

// SetLocal records a setting only the Echo has (no controller key): kept,
// never pending, never reported.
func (s *Store) SetLocal(key string, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Values[key] = v
	return s.saveLocked()
}

// Reconcile takes the value a controller pushed for key and returns the one
// to put in force.
func (s *Store) Reconcile(key string, pushed bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rec.Pending[key]; ok {
		if p == pushed {
			delete(s.rec.Pending, key)
			s.saveLocked()
		}
		return p
	}
	if v, ok := s.rec.Values[key]; !ok || v != pushed {
		s.rec.Values[key] = pushed
		s.saveLocked()
	}
	return pushed
}

// Pending is what the controller has yet to acknowledge.
func (s *Store) Pending() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.rec.Pending))
	for k, v := range s.rec.Pending {
		out[k] = v
	}
	return out
}
