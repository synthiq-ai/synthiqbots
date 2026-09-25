package artifact

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir, BaseURL: "https://ops.example"}

	rel, url, err := s.Save("tacmap", "png", []byte("PNGBYTES"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(url, "https://ops.example/v1/artifacts/") || !strings.HasSuffix(url, rel) {
		t.Errorf("url = %q, rel = %q", url, rel)
	}
	if !strings.HasSuffix(rel, ".png") || !strings.Contains(rel, "tacmap_") {
		t.Errorf("rel = %q, want tacmap_<token>.png", rel)
	}

	f, err := s.Open(rel)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "PNGBYTES" {
		t.Errorf("content = %q", got)
	}
}

func TestSaveTokensAreUnique(t *testing.T) {
	s := &Store{Dir: t.TempDir(), BaseURL: "https://x"}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		rel, _, err := s.Save("shot", "jpg", []byte("x"))
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if seen[rel] {
			t.Fatalf("duplicate rel path %q", rel)
		}
		seen[rel] = true
	}
}

func TestOpenRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	// Plant a secret outside the store dir.
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: dir, BaseURL: "https://x"}

	for _, bad := range []string{
		"../secret.txt",
		"../../etc/passwd",
		"foo/../../secret.txt",
	} {
		if _, err := s.Open(bad); err == nil {
			t.Errorf("Open(%q) succeeded, want rejection", bad)
		}
	}
}

func TestDisabledStore(t *testing.T) {
	s := &Store{}
	if s.Enabled() {
		t.Fatal("empty Dir should be disabled")
	}
	if _, _, err := s.Save("x", "png", []byte("y")); err == nil {
		t.Error("Save on disabled store should error")
	}
	if _, err := s.Open("a/b.png"); err == nil {
		t.Error("Open on disabled store should error")
	}
}

func TestSanitizeExt(t *testing.T) {
	cases := map[string]string{
		"png":      ".png",
		".png":     ".png",
		"JPG":      ".jpg",
		"mp4":      ".mp4",
		"":         ".bin",
		"../evil":  ".bin",
		"toolongx": ".bin",
		"p/g":      ".bin",
	}
	for in, want := range cases {
		if got := sanitizeExt(in); got != want {
			t.Errorf("sanitizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}
