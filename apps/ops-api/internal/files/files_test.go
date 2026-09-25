package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	// Real subdir we'll allow.
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "ok.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := NewResolver(map[string]string{"repo": root})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		rel     string
		wantErr error
	}{
		{"plain file", "src/ok.txt", nil},
		{"empty rel == root", "", nil},
		{"dot-dot literal", "src/../../etc", ErrPathOutsideRoot},
		{"dot-dot deep", "../../../etc/passwd", ErrPathOutsideRoot},
		{"missing", "src/missing.txt", ErrPathNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := r.Resolve("repo", c.rel)
			if c.wantErr == nil && err != nil {
				t.Fatalf("got %v, want nil", err)
			}
			if c.wantErr != nil && err != c.wantErr {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestResolverSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a symlink inside repo that points outside it.
	link := filepath.Join(repo, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks not supported on this filesystem")
	}

	r, err := NewResolver(map[string]string{"repo": repo})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = r.Resolve("repo", "escape/secret.txt")
	if err != ErrPathOutsideRoot {
		t.Fatalf("got %v, want ErrPathOutsideRoot", err)
	}
}

func TestResolverUnknownRoot(t *testing.T) {
	r, err := NewResolver(map[string]string{"repo": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("nope", "x"); err != ErrUnknownRoot {
		t.Fatalf("got %v, want ErrUnknownRoot", err)
	}
}

func TestReadLineSlice(t *testing.T) {
	root := t.TempDir()
	body := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := NewResolver(map[string]string{"r": root})
	got, err := r.Read("r", "f.txt", 2, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "line2\nline3\nline4\n"
	if got.Content != want {
		t.Fatalf("got %q want %q", got.Content, want)
	}
	if got.TotalLines != 5 {
		t.Fatalf("got %d total, want 5", got.TotalLines)
	}
	if !strings.HasSuffix(got.Path, "f.txt") {
		t.Fatalf("unexpected path %q", got.Path)
	}
}
