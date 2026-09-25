package screens

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touch creates a file at path with the supplied mtime.
func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestMatchPicksClosestInWindow(t *testing.T) {
	dir := t.TempDir()
	addonTs := time.Date(2026, 4, 26, 14, 0, 3, 0, time.UTC)

	// 3 candidates, only the middle one is closest in time.
	touch(t, filepath.Join(dir, "WoWScrnShot_042626_135950.tga"), addonTs.Add(-13*time.Second))
	touch(t, filepath.Join(dir, "WoWScrnShot_042626_140005.jpg"), addonTs.Add(2*time.Second))
	touch(t, filepath.Join(dir, "WoWScrnShot_042626_140020.tga"), addonTs.Add(17*time.Second))

	path, mime, err := Match(dir, addonTs, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "WoWScrnShot_042626_140005.jpg" {
		t.Errorf("matched %q, expected the +2s jpg", path)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime: %q", mime)
	}
}

func TestMatchSkipsNonImages(t *testing.T) {
	dir := t.TempDir()
	addonTs := time.Now().UTC()
	touch(t, filepath.Join(dir, "config.txt"), addonTs)
	touch(t, filepath.Join(dir, "screenshot.tga"), addonTs)

	path, mime, err := Match(dir, addonTs, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "screenshot.tga" {
		t.Errorf("matched %q", path)
	}
	if mime != "image/x-tga" {
		t.Errorf("mime: %q", mime)
	}
}

func TestMatchOutsideWindow(t *testing.T) {
	dir := t.TempDir()
	addonTs := time.Now().UTC()
	touch(t, filepath.Join(dir, "old.tga"), addonTs.Add(-1*time.Hour))

	path, _, err := Match(dir, addonTs, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("expected no match, got %q", path)
	}
}

func TestMatchMissingDir(t *testing.T) {
	path, _, err := Match("/does/not/exist/screenshots", time.Now(), time.Minute)
	if err != nil {
		t.Errorf("missing dir should not error, got: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}
