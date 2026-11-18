package owned

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAValueSetOnTheEchoIsKeptAcrossABoot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := Load(p).SetLocal("multiTap", true); err != nil {
		t.Fatal(err)
	}
	if v, ok := Load(p).Value("multiTap"); !ok || !v {
		t.Fatal("not kept")
	}
}

func TestSeedTakesAValueOnlyWhereTheEchoHasNone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s := Load(p)
	if got := s.Seed("muteRing", true); !got {
		t.Fatal("the first value was not taken")
	}
	if err := s.SetLocal("muteRing", false); err != nil {
		t.Fatal(err)
	}
	// An older controller pushing its value, or the default again: ignored.
	if got := Load(p).Seed("muteRing", true); got {
		t.Fatal("a seed undid the Echo's own value")
	}
}

func TestAFileFromThePendingSchemeKeepsItsValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"values":{"muteRing":false,"bleProxyEnabled":true},"pending":{"muteRing":false}}`), 0o600)
	s := Load(p)
	if v, ok := s.Value("muteRing"); !ok || v {
		t.Fatal("the pending-era value was lost")
	}
	if s.Seed("bleProxyEnabled", false) != true {
		t.Fatal("a kept value lost to a seed")
	}
}

func TestUnreadableIsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	s := Load(p)
	if _, ok := s.Value("muteRing"); ok {
		t.Fatal("garbage read as settings")
	}
	if s.Seed("remoteVolumeArc", true) != true {
		t.Fatal()
	}
}

func TestANumberIsKeptAndSeededByTheSameRules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s := Load(p)
	if got := s.SeedInt("wakeMic", 2); got != 2 {
		t.Fatalf("seed: %d", got)
	}
	if err := s.SetLocalInt("wakeMic", 4); err != nil {
		t.Fatal(err)
	}
	s = Load(p)
	if got := s.SeedInt("wakeMic", 0); got != 4 {
		t.Fatalf("a seed undid the Echo's own value: %d", got)
	}
	if v, ok := s.IntValue("wakeMic"); !ok || v != 4 {
		t.Fatal("not kept across a boot")
	}
}

func TestAFileWithoutNumbersStillReads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"values":{"muteRing":false}}`), 0o600)
	s := Load(p)
	if _, ok := s.IntValue("wakeMic"); ok {
		t.Fatal("a number from nowhere")
	}
	if s.SeedInt("wakeMic", 3) != 3 {
		t.Fatal()
	}
	if v, ok := Load(p).Value("muteRing"); !ok || v {
		t.Fatal("the switches were lost")
	}
}
