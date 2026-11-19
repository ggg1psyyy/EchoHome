package sendspin

import (
	"os"
	"path/filepath"
	"testing"
)

// PairedServers reads the pairings without a player and writes nothing.
func TestPairedServersCountsWithoutStartingAPlayer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sendspin.json")
	if n := PairedServers(path); n != 0 {
		t.Fatalf("no store: %d", n)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("reading the count created the store")
	}
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := PairedServers(path); n != 0 {
		t.Fatalf("unpaired store: %d", n)
	}
	if err := s.addRecord("server-a", make([]byte, 32), func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if n := PairedServers(path); n != 1 {
		t.Fatalf("paired once: %d", n)
	}
	os.WriteFile(path, []byte("{"), 0o600)
	if n := PairedServers(path); n != 0 {
		t.Fatalf("unreadable store: %d", n)
	}
}
