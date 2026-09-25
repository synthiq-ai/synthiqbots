// Package screens locates the screenshot file that corresponds to a feedback
// entry. WoW writes Screenshots/WoWScrnShot_MMDDYY_HHMMSS.tga|jpg with the
// local clock. The addon's `Screenshot()` is async, so the file appears a
// few hundred ms after the addon-recorded timestamp.
//
// Match strategy: scan the Screenshots/ folder for files whose mtime is
// within ±window of the entry's addon_ts; pick the one closest to the
// entry timestamp.
package screens

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Match returns the absolute path + content-type of the screenshot file
// closest in time to addonTs, or an empty string if no match within
// window. Not finding a screenshot is not an error — the addon may have
// failed to write one, or the user emptied Screenshots/ between capture
// and upload.
func Match(screenshotsDir string, addonTs time.Time, window time.Duration) (string, string, error) {
	entries, err := os.ReadDir(screenshotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read %s: %w", screenshotsDir, err)
	}

	var (
		best     string
		bestMime string
		bestDiff time.Duration = window + time.Hour // sentinel
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		mime := mimeForExt(ext)
		if mime == "" {
			continue // skip non-image files (e.g. World of Warcraft - reset_screenshot.txt)
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		diff := absDuration(info.ModTime().Sub(addonTs))
		if diff > window {
			continue
		}
		if diff < bestDiff {
			bestDiff = diff
			best = filepath.Join(screenshotsDir, name)
			bestMime = mime
		}
	}
	return best, bestMime, nil
}

func mimeForExt(ext string) string {
	switch ext {
	case ".tga":
		return "image/x-tga"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".bmp":
		return "image/bmp"
	}
	return ""
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
