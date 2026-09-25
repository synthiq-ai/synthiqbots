package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/files"
)

type FileWriteDeps struct {
	Resolver        *files.Resolver
	ConfigRoot      string              // dir under which AllowedFiles live
	BackupDir       string              // base dir for *.<ts>.bak
	AllowedFiles    []string            // basenames (file_set_key)
	AllowedKeys     map[string][]string // basename → key globs
	DeniedKeys      []string            // always-deny key globs
	AllowedWriteGlobs map[string][]string // root → list of path globs (file_write)
}

// RegisterFileWriteTools registers file_set_key (key-level conf edit) and
// file_write (full overwrite). Both are atomic write-tmp+rename + timestamped
// backup. Both reject paths and keys that match deny patterns.
func RegisterFileWriteTools(reg *Registry, deps FileWriteDeps) {
	reg.Register(Tool{
		Name: "file_set_key",
		Description: "Edit a key=value entry in an allowlisted .conf file. Args: file (basename), " +
			"key, value. The key must match an entry in OPS_ADMIN_ALLOWED_KEYS_JSON for that " +
			"file AND must NOT match any pattern in OPS_ADMIN_DENIED_KEYS (Token/Password/etc " +
			"are always denied). Atomic write-tmp+rename; a timestamped backup is created in " +
			"OPS_ADMIN_BACKUP_DIR. After a successful edit on a .conf, call config_reload via " +
			"the gameplay MCP to apply the new value to a running worldserver.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"file":{"type":"string","description":"Basename of an allowlisted .conf file"},
			"key":{"type":"string","description":"Config key (e.g. OllamaChat.Gateway.MaxConcurrent)"},
			"value":{"type":"string","description":"New value (string-encoded; numeric/bool as text)"}
		},"required":["file","key","value"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct{ File, Key, Value string }
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.File == "" || a.Key == "" {
				return map[string]any{"error": "file and key are required"}
			}
			if !isSafeBasename(a.File) {
				return map[string]any{"error": "file must be a plain basename"}
			}
			if !contains(deps.AllowedFiles, a.File) {
				return map[string]any{"error": "file not in OPS_ADMIN_ALLOWED_FILES"}
			}
			if MatchesAny(a.Key, deps.DeniedKeys) {
				return map[string]any{"error": "key matches a deny pattern (DeniedKeys)"}
			}
			allowed := deps.AllowedKeys[a.File]
			if len(allowed) == 0 {
				return map[string]any{"error": "no AllowedKeys entry for this file"}
			}
			if !MatchesAny(a.Key, allowed) {
				return map[string]any{"error": "key not in allowlist for this file"}
			}
			if deps.ConfigRoot == "" {
				return map[string]any{"error": "OPS_ADMIN_CONFIG_ROOT not configured"}
			}
			fullPath := filepath.Join(deps.ConfigRoot, a.File)
			if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(deps.ConfigRoot)+string(os.PathSeparator)) {
				return map[string]any{"error": "resolved path escapes config root"}
			}

			backup, err := backupFile(fullPath, a.File, deps.BackupDir)
			if err != nil {
				return map[string]any{"error": "backup: " + err.Error()}
			}
			res, err := atomicSetKey(fullPath, a.Key, a.Value)
			if err != nil {
				return map[string]any{"error": err.Error(), "backup": backup}
			}
			return map[string]any{
				"ok": true, "file": a.File, "key": a.Key,
				"oldValue": res.OldValue, "newValue": a.Value,
				"mode": res.Mode, "line": res.Line, "backup": backup,
				"hint": "Call config_reload via the gameplay MCP to apply.",
			}
		},
	})

	reg.Register(Tool{
		Name: "file_write",
		Description: "Overwrite a file under an allowlisted root. Args: root, path, content, " +
			"optional mode (octal, e.g. \"0644\"). The path must match a glob in " +
			"OPS_ADMIN_WRITE_GLOBS for that root. Atomic write-tmp+rename; timestamped backup " +
			"created in OPS_ADMIN_BACKUP_DIR if the file already existed. There is no " +
			"file_delete — pass empty content (or restore from backup) to clear a file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"root":{"type":"string"},
			"path":{"type":"string"},
			"content":{"type":"string"},
			"mode":{"type":"string","description":"Octal mode like \"0644\" (default keep existing)"}
		},"required":["root","path","content"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Root, Path, Content, Mode string
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.Root == "" || a.Path == "" {
				return map[string]any{"error": "root and path are required"}
			}
			globs, ok := deps.AllowedWriteGlobs[a.Root]
			if !ok || len(globs) == 0 {
				return map[string]any{"error": "root has no entry in OPS_ADMIN_WRITE_GLOBS"}
			}
			if !MatchesAny(a.Path, globs) {
				return map[string]any{"error": "path does not match any allowlisted glob"}
			}
			// Resolve the path. The resolver enforces no-escape.
			abs, err := resolveForWrite(deps.Resolver, a.Root, a.Path)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			basename := filepath.Base(abs)
			var backup string
			if _, statErr := os.Stat(abs); statErr == nil {
				if b, err := backupFile(abs, basename, deps.BackupDir); err != nil {
					return map[string]any{"error": "backup: " + err.Error()}
				} else {
					backup = b
				}
			}
			mode := os.FileMode(0)
			if a.Mode != "" {
				m, err := strconv.ParseUint(a.Mode, 8, 32)
				if err != nil {
					return map[string]any{"error": "invalid mode (use octal like \"0644\")"}
				}
				mode = os.FileMode(m)
			}
			if err := atomicWrite(abs, []byte(a.Content), mode); err != nil {
				return map[string]any{"error": err.Error(), "backup": backup}
			}
			return map[string]any{
				"ok": true, "root": a.Root, "path": a.Path,
				"bytes":  len(a.Content), "backup": backup,
			}
		},
	})
}

// --- helpers ---

type setKeyResult struct {
	OldValue string
	Mode     string // "replaced" | "appended"
	Line     int
}

// atomicSetKey mirrors ConfigAtomicSetValue at src/mod-ollama-chat_tools.cpp:2803-2902.
// First live (uncommented) `key = ...` line is replaced; otherwise the entry is
// appended at EOF with a marker comment. Writes through <path>.mcp.tmp +
// os.Rename for atomicity, preserves mode/owner.
func atomicSetKey(path, key, newValue string) (setKeyResult, error) {
	out := setKeyResult{}
	src, err := os.Open(path)
	if err != nil {
		return out, fmt.Errorf("open %s: %w", path, err)
	}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	src.Close()
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("scan: %w", err)
	}

	replaced := false
	for i := range lines {
		if replaced {
			break
		}
		l := lines[i]
		// Skip leading whitespace + commented lines.
		j := 0
		for j < len(l) && unicode.IsSpace(rune(l[j])) {
			j++
		}
		if j < len(l) && l[j] == '#' {
			continue
		}
		if j+len(key) > len(l) || l[j:j+len(key)] != key {
			continue
		}
		k := j + len(key)
		for k < len(l) && unicode.IsSpace(rune(l[k])) {
			k++
		}
		if k >= len(l) || l[k] != '=' {
			continue
		}
		valStart := k + 1
		for valStart < len(l) && unicode.IsSpace(rune(l[valStart])) {
			valStart++
		}
		// Detect inline comment.
		commentPos := -1
		for p := valStart; p < len(l); p++ {
			if l[p] == '#' {
				commentPos = p
				break
			}
		}
		if commentPos >= 0 {
			valEnd := commentPos
			for valEnd > valStart && unicode.IsSpace(rune(l[valEnd-1])) {
				valEnd--
			}
			out.OldValue = l[valStart:valEnd]
			pre := l[:valStart]
			post := l[commentPos:]
			lines[i] = pre + newValue + " " + post
		} else {
			valEnd := len(l)
			for valEnd > valStart && unicode.IsSpace(rune(l[valEnd-1])) {
				valEnd--
			}
			out.OldValue = l[valStart:valEnd]
			lines[i] = l[:valStart] + newValue
		}
		replaced = true
		out.Line = i + 1
		out.Mode = "replaced"
	}
	if !replaced {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines,
			fmt.Sprintf("# [Added by file_set_key @ %d]", time.Now().Unix()),
			key+" = "+newValue)
		out.Line = len(lines)
		out.Mode = "appended"
	}

	// Build the rendered content and write atomically.
	body := strings.Join(lines, "\n") + "\n"
	if err := atomicWrite(path, []byte(body), 0); err != nil {
		return out, err
	}
	return out, nil
}

// atomicWrite writes data to path via <path>.mcp.tmp + os.Rename. If mode==0,
// the existing mode is preserved (or 0644 if the file is new).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".mcp.tmp"
	useMode := mode
	if useMode == 0 {
		if st, err := os.Stat(path); err == nil {
			useMode = st.Mode().Perm()
		} else {
			useMode = 0644
		}
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, useMode)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func backupFile(srcPath, basename, backupDir string) (string, error) {
	if backupDir == "" {
		return "", errors.New("OPS_ADMIN_BACKUP_DIR not configured")
	}
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir backup: %w", err)
	}
	dst := filepath.Join(backupDir, fmt.Sprintf("%s.%d.bak", basename, time.Now().Unix()))
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("open src: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return "", fmt.Errorf("create backup: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return "", fmt.Errorf("copy backup: %w", err)
	}
	return dst, nil
}

// resolveForWrite mirrors files.Resolver.Resolve but tolerates the file not
// existing yet (file_write must allow create). The parent directory must exist
// AND be inside the configured root.
func resolveForWrite(r *files.Resolver, label, relPath string) (string, error) {
	roots := r.Roots()
	root, ok := roots[label]
	if !ok {
		return "", fmt.Errorf("unknown root %q", label)
	}
	for _, seg := range strings.Split(relPath, "/") {
		if seg == ".." {
			return "", errors.New("path contains '..'")
		}
	}
	abs := filepath.Clean(filepath.Join(root, relPath))
	parent := filepath.Dir(abs)
	if parentReal, err := filepath.EvalSymlinks(parent); err == nil {
		if !strings.HasPrefix(parentReal, root+string(os.PathSeparator)) && parentReal != root {
			return "", errors.New("path outside allowlisted root")
		}
	} else {
		return "", fmt.Errorf("parent dir not resolvable: %w", err)
	}
	return abs, nil
}

func isSafeBasename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	for _, c := range name {
		if c < 0x20 {
			return false
		}
	}
	return true
}

func contains(slice []string, v string) bool {
	for _, s := range slice {
		if s == v {
			return true
		}
	}
	return false
}
