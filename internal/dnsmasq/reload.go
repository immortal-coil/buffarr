package dnsmasq

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile atomically writes content to path: a temp file in the same
// directory, then a rename over the destination, so dnsmasq's in-container
// watcher (which reacts to the file's create/close_write/moved_to events,
// KTD7) never observes a partially-written file. The watcher decides
// SIGHUP-vs-restart based on which exact filename it sees change (see
// docker/dnsmasq/entrypoint.sh), so path must be one of the two filenames
// that script matches on.
func WriteFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".dnsmasq-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
	}
	return nil
}
