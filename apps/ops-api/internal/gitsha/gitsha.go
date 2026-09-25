package gitsha

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve walks repoRoot/.git/HEAD and follows refs to produce the current commit SHA.
// Mirrors `git rev-parse HEAD` for the common cases (detached HEAD, plain ref, packed-refs).
func Resolve(repoRoot string) (string, error) {
	gitDir := filepath.Join(repoRoot, ".git")
	headPath := filepath.Join(gitDir, "HEAD")
	headBytes, err := os.ReadFile(headPath)
	if err != nil {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	head := strings.TrimSpace(string(headBytes))

	// Detached HEAD: HEAD itself is the sha.
	if !strings.HasPrefix(head, "ref: ") {
		return head, nil
	}
	ref := strings.TrimPrefix(head, "ref: ")

	// Loose ref file.
	refPath := filepath.Join(gitDir, ref)
	if data, err := os.ReadFile(refPath); err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	// Packed refs.
	packed, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", fmt.Errorf("ref %q not loose and packed-refs missing: %w", ref, err)
	}
	defer packed.Close()
	sc := bufio.NewScanner(packed)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == ref {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("ref %q not found in packed-refs", ref)
}
