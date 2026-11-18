package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// What is saved is what comes back: a fresh Device given the saved message
// has the same configuration as the one that saved it.
func TestTheLastConfigComesBackWhole(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	d := &Device{}
	on, off := true, false
	pw := "1000:abcd:ef01"
	duck := 12.0
	d.Apply(ConfigMessage{Type: "config", MuteRing: &off, RemoteVolumeArc: &on, ESPHomeServer: &on,
		BleProxyEnabled: &on, ConsolePassword: &pw, DuckDb: &duck,
		ListeningAnim: json.RawMessage(`{"pattern":"spin","colors":[[0,0,255]]}`)})
	if wrote, err := d.SaveLast(p); err != nil || !wrote {
		t.Fatalf("%v %v", wrote, err)
	}
	msg, ok := LoadLast(p)
	if !ok {
		t.Fatal("not loaded")
	}
	e := &Device{}
	e.Apply(msg)
	a, _ := json.Marshal(d.Snapshot())
	b, _ := json.Marshal(e.Snapshot())
	if string(a) != string(b) {
		t.Fatalf("came back different:\n%s\n%s", a, b)
	}
	// Unchanged: not written again.
	st, _ := os.Stat(p)
	if wrote, _ := d.SaveLast(p); wrote {
		t.Fatal("an unchanged config was written again")
	}
	if st2, _ := os.Stat(p); !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("the file was touched")
	}
}

func TestNoneOrUnreadableIsNothing(t *testing.T) {
	dir := t.TempDir()
	if _, ok := LoadLast(filepath.Join(dir, "missing.json")); ok {
		t.Fatal("a missing file loaded")
	}
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, ok := LoadLast(p); ok {
		t.Fatal("garbage loaded")
	}
}
