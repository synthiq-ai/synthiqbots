package files

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	DefaultMaxRead = 1 << 20 // 1 MiB
	HardMaxRead    = 5 << 20 // 5 MiB
	MaxSearchHits  = 200
	MaxSearchTime  = 10 * time.Second
)

var (
	ErrUnknownRoot     = errors.New("unknown root")
	ErrPathOutsideRoot = errors.New("path outside allowlisted root")
	ErrPathNotFound    = errors.New("path not found")
	ErrIsDir           = errors.New("path is a directory")
)

type Resolver struct {
	roots map[string]string // label → absolute root path (already resolved)
}

func NewResolver(roots map[string]string) (*Resolver, error) {
	resolved := map[string]string{}
	for label, raw := range roots {
		if !filepath.IsAbs(raw) {
			return nil, fmt.Errorf("root %q must be absolute", label)
		}
		clean := filepath.Clean(raw)
		// Resolve symlinks now so prefix-checks later are sound.
		if real, err := filepath.EvalSymlinks(clean); err == nil {
			clean = real
		}
		resolved[label] = clean
	}
	return &Resolver{roots: resolved}, nil
}

func (r *Resolver) Roots() map[string]string { return r.roots }

// Resolve takes (label, relPath) and returns the absolute path inside the root,
// or an error if the result escapes the root.
func (r *Resolver) Resolve(label, relPath string) (string, string, error) {
	root, ok := r.roots[label]
	if !ok {
		return "", "", ErrUnknownRoot
	}
	// Reject literal `..` segments before any normalization. Belt-and-braces;
	// the prefix check below would also catch it after EvalSymlinks, but bailing
	// early on this very common attack input keeps error messages clear.
	if relPath != "" {
		for _, seg := range strings.Split(relPath, "/") {
			if seg == ".." {
				return "", "", ErrPathOutsideRoot
			}
		}
	}
	joined := filepath.Join(root, relPath)
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", "", err
	}
	// EvalSymlinks resolves any symlinks along the path. The result must still
	// be inside `root`. If the path doesn't exist yet, EvalSymlinks errors —
	// for read/list we'd want existence anyway, so propagate that error.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", ErrPathNotFound
		}
		return "", "", err
	}
	if !strings.HasPrefix(resolved, root+string(os.PathSeparator)) && resolved != root {
		return "", "", ErrPathOutsideRoot
	}
	return resolved, root, nil
}

type Entry struct {
	Path  string    `json:"path"` // relative to root
	IsDir bool      `json:"isDir"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

func (r *Resolver) List(label, relPath, glob string) ([]Entry, error) {
	abs, root, err := r.Resolve(label, relPath)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		// Single file case: return one entry.
		return []Entry{{
			Path:  rel(root, abs),
			IsDir: false,
			Size:  st.Size(),
			MTime: st.ModTime(),
		}}, nil
	}
	dir, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(dir))
	for _, d := range dir {
		name := d.Name()
		if name == ".git" {
			continue
		}
		if glob != "" {
			ok, err := path.Match(glob, name)
			if err != nil || !ok {
				continue
			}
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Path:  rel(root, filepath.Join(abs, name)),
			IsDir: d.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

type ReadResult struct {
	Path       string `json:"path"`
	TotalLines int    `json:"totalLines"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Truncated  bool   `json:"truncated"`
	Content    string `json:"content"`
}

func (r *Resolver) Read(label, relPath string, start, end, maxBytes int) (*ReadResult, error) {
	abs, root, err := r.Resolve(label, relPath)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, ErrIsDir
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRead
	}
	if maxBytes > HardMaxRead {
		maxBytes = HardMaxRead
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limited := bufio.NewReader(f)
	var sb strings.Builder
	read := 0
	totalLines := 0
	truncated := false
	var slice []string
	startLine := start
	if startLine < 1 {
		startLine = 1
	}
	for {
		line, err := limited.ReadString('\n')
		if line != "" {
			totalLines++
			read += len(line)
			if read > maxBytes {
				truncated = true
				break
			}
			if totalLines >= startLine && (end == 0 || totalLines <= end) {
				slice = append(slice, line)
			}
		}
		if err != nil {
			break
		}
	}
	for _, l := range slice {
		sb.WriteString(l)
	}
	endLine := end
	if endLine == 0 || endLine > totalLines {
		endLine = totalLines
	}
	return &ReadResult{
		Path:       rel(root, abs),
		TotalLines: totalLines,
		Start:      startLine,
		End:        endLine,
		Truncated:  truncated,
		Content:    sb.String(),
	}, nil
}

type Hit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (r *Resolver) Search(ctx context.Context, label, relPath, query string, useRegex bool, max int) ([]Hit, bool, error) {
	abs, root, err := r.Resolve(label, relPath)
	if err != nil {
		return nil, false, err
	}
	if max <= 0 || max > MaxSearchHits {
		max = MaxSearchHits
	}

	var matcher func(string) bool
	if useRegex {
		re, err := regexp.Compile(query)
		if err != nil {
			return nil, false, fmt.Errorf("invalid regex: %w", err)
		}
		matcher = re.MatchString
	} else {
		matcher = func(s string) bool { return strings.Contains(s, query) }
	}

	deadline, cancel := context.WithTimeout(ctx, MaxSearchTime)
	defer cancel()

	hits := []Hit{}
	timedOut := false
	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-deadline.Done():
			timedOut = true
			return filepath.SkipAll
		default:
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(hits) >= max {
			return filepath.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		// NUL-byte sniff to skip binaries.
		head := make([]byte, 512)
		n, _ := f.Read(head)
		for i := 0; i < n; i++ {
			if head[i] == 0 {
				return nil
			}
		}
		_, _ = f.Seek(0, 0)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			text := sc.Text()
			if matcher(text) {
				hits = append(hits, Hit{
					Path: rel(root, path),
					Line: ln,
					Text: truncate(text, 400),
				})
				if len(hits) >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return hits, timedOut, walkErr
	}
	return hits, timedOut, nil
}

func rel(root, abs string) string {
	r, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
