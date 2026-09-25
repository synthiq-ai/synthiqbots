package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.HasUploaded("a") {
		t.Errorf("fresh store reports a uploaded")
	}
	if err := s.MarkUploaded("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUploaded("b"); err != nil {
		t.Fatal(err)
	}
	if !s.HasUploaded("a") || !s.HasUploaded("b") {
		t.Errorf("marks did not stick")
	}
	if s.Stats() != 2 {
		t.Errorf("stats: %d", s.Stats())
	}

	// Reload from disk and confirm persistence.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.HasUploaded("a") || !s2.HasUploaded("b") {
		t.Errorf("reload lost data: %v", s2.st)
	}
}

func TestStateOpenMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "no-such.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Stats() != 0 {
		t.Errorf("expected empty, got %d", s.Stats())
	}
}

func TestStateAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUploaded("x"); err != nil {
		t.Fatal(err)
	}
	// .tmp file should not be left around after a successful flush.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf(".tmp file leaked")
	}
}
