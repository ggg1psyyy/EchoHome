package incubator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheSavedSatelliteRoundTripsAndIsWrittenOnlyOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "incubator.json")
	k := &Keeper{Path: path}
	on, vol := false, 80
	s := Saved{Name: "n", MAC: "02:ec:00:00:00:01", Port: 6055, Monitor: true,
		ListeningAnim: json.RawMessage(`{"pattern":"solid"}`), MuteRing: &on, Volume: &vol}
	if err := k.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSaved(path)
	if err != nil || got.Settings() != (Settings{Enabled: true, Name: "n", MAC: "02:ec:00:00:00:01", Port: 6055, Monitor: true, Standalone: true}) ||
		*got.Volume != 80 || *got.MuteRing || string(got.ListeningAnim) != `{"pattern":"solid"}` {
		t.Fatalf("%+v %v", got, err)
	}
	st, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	k.Save(s) // the same
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("an unchanged save wrote the file")
	}
	if err := k.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSaved(path); err == nil {
		t.Fatal("removed, and still loaded")
	}
	if err := k.Remove(); err != nil {
		t.Fatal("removing twice:", err)
	}
}

func TestAnUnreadableSavedSatelliteIsNone(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"garbage": "{not json", "no name": `{"mac":"m","port":1}`, "no mac": `{"name":"n"}`,
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := LoadSaved(p); err == nil {
			t.Errorf("%s loaded", name)
		}
	}
	p := filepath.Join(dir, "loud")
	os.WriteFile(p, []byte(`{"name":"n","mac":"m","volume":300}`), 0o600)
	if s, err := LoadSaved(p); err != nil || s.Volume != nil {
		t.Errorf("an out-of-range volume kept: %+v %v", s, err)
	}
}
