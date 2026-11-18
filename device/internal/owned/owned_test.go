package owned

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAChangeOnTheEchoStandsUntilTheControllerCarriesItBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s := Load(p)
	// The controller's push is the value while nothing is pending.
	if got := s.Reconcile("muteRing", true); !got {
		t.Fatal("a push with nothing pending was not taken")
	}
	// HA turns it off on the Echo.
	if err := s.Set("muteRing", false); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Value("muteRing"); v || len(s.Pending()) != 1 {
		t.Fatalf("%v %v", v, s.Pending())
	}
	// A controller that has not heard: its older value is ignored.
	if got := s.Reconcile("muteRing", true); got {
		t.Fatal("an older push undid the Echo's change")
	}
	// Across a reboot.
	s = Load(p)
	if v, ok := s.Value("muteRing"); !ok || v || s.Pending()["muteRing"] != false || len(s.Pending()) != 1 {
		t.Fatalf("after a reboot: %v %v %v", v, ok, s.Pending())
	}
	// The controller carries it back: no longer pending.
	if got := s.Reconcile("muteRing", false); got || len(s.Pending()) != 0 {
		t.Fatalf("%v %v", got, s.Pending())
	}
	// From now the controller's push decides again.
	if got := s.Reconcile("muteRing", true); !got {
		t.Fatal("a later push from the dashboard was not taken")
	}
	if v, _ := Load(p).Value("muteRing"); !v {
		t.Fatal("the dashboard's value was not kept")
	}
}

func TestALocalSettingIsKeptAndNeverReported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s := Load(p)
	if err := s.SetLocal("multiTap", true); err != nil {
		t.Fatal(err)
	}
	if len(s.Pending()) != 0 {
		t.Fatal("a local setting is pending")
	}
	if v, ok := Load(p).Value("multiTap"); !ok || !v {
		t.Fatal("not kept")
	}
}

func TestUnreadableIsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	s := Load(p)
	if _, ok := s.Value("muteRing"); ok || len(s.Pending()) != 0 {
		t.Fatal("garbage read as settings")
	}
	if s.Reconcile("remoteVolumeArc", true) != true {
		t.Fatal()
	}
}
