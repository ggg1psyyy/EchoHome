package incubator

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/wilbowes/EchoMuse/internal/atomicfile"
)

// A standalone satellite (Settings.Standalone) outlives the controller: the
// Echo keeps what it needs to start it, and to look and sound as it did, in
// one file, and starts from it at boot before any controller is found. Only
// what the satellite reads: its identity and port, whether the app may
// listen live, the ring's scene (the listening ring and incubatorRing), the
// mute ring setting, and the volume (an Echo boots silent until a controller
// restores it). A controller that connects later says the same, which keeps
// the running satellite, or something else, which replaces it; one that
// disables the satellite or standalone deletes the file. Anything unreadable
// is no standalone.

// StandalonePath is where the saved satellite lives.
const StandalonePath = "/data/local/etc/echomuse/incubator.json"

// Saved is the file's content.
type Saved struct {
	Name          string          `json:"name"`
	MAC           string          `json:"mac"`
	Port          int             `json:"port"`
	Monitor       bool            `json:"monitor,omitempty"`
	ListeningAnim json.RawMessage `json:"listeningAnim,omitempty"`
	IncubatorRing json.RawMessage `json:"incubatorRing,omitempty"`
	MuteRing      *bool           `json:"muteRing,omitempty"`
	Volume        *int            `json:"volume,omitempty"`
}

// Settings is what the saved satellite runs as.
func (s Saved) Settings() Settings {
	return Settings{Enabled: true, Name: s.Name, MAC: s.MAC, Port: s.Port, Monitor: s.Monitor, Standalone: true}
}

var errNoIdentity = errors.New("incubator: saved satellite has no name or MAC")

// LoadSaved reads the saved satellite; any fault is no standalone.
func LoadSaved(path string) (Saved, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Saved{}, err
	}
	var s Saved
	if err := json.Unmarshal(b, &s); err != nil {
		return Saved{}, err
	}
	if s.Name == "" || s.MAC == "" {
		return Saved{}, errNoIdentity
	}
	if s.Volume != nil && (*s.Volume < 0 || *s.Volume > 127) {
		s.Volume = nil
	}
	return s, nil
}

// Keeper writes the saved satellite only when it changed (eMMC wear), and
// removes it.
type Keeper struct {
	Path string
	mu   sync.Mutex
	last []byte
}

// Save writes s if it differs from what was last written or read.
func (k *Keeper) Save(s Saved) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.last == nil {
		k.last, _ = os.ReadFile(k.Path)
	}
	if bytes.Equal(b, k.last) {
		return nil
	}
	if err := atomicfile.Write(k.Path, b, 0o600); err != nil {
		return err
	}
	k.last = b
	return nil
}

// Remove deletes the saved satellite: no standalone at the next boot.
func (k *Keeper) Remove() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.last = nil
	if err := os.Remove(k.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
