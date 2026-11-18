package config

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/wilbowes/EchoMuse/internal/atomicfile"
)

// LastPath is where the Echo keeps the configuration it was last given, so
// it boots with it when no controller is there: a controller pushes the
// whole of it on every connect, but an Echo that runs on its own (EchoHome:
// the controller only sets it up) must not fall back to the firmware's
// defaults on a reboot. Read at boot and applied as a push would be.
const LastPath = "/data/local/etc/echomuse/config.json"

// SaveLast writes the configuration in force (Snapshot), only when it
// changed: a push on every connect must not cost a flash write each time.
// It reports whether it wrote.
func (d *Device) SaveLast(path string) (bool, error) {
	b, err := json.Marshal(d.Snapshot())
	if err != nil {
		return false, err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	if err := atomicfile.Write(path, b, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// LoadLast reads what SaveLast wrote; false when there is none or it is
// unreadable (the firmware's defaults then stand, as before).
func LoadLast(path string) (ConfigMessage, bool) {
	var msg ConfigMessage
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &msg) != nil {
		return ConfigMessage{}, false
	}
	return msg, true
}
