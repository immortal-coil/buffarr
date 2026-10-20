package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memStateStore is an in-memory StateStore for tests, standing in for the
// real Catalog-backed one.
type memStateStore struct {
	states map[string]TailState
}

func newMemStateStore() *memStateStore {
	return &memStateStore{states: make(map[string]TailState)}
}

func (s *memStateStore) LoadTailState(path string) (TailState, bool, error) {
	st, found := s.states[path]
	return st, found, nil
}

func (s *memStateStore) SaveTailState(path string, st TailState) error {
	s.states[path] = st
	return nil
}

func TestTailer_FileDoesNotExistYet(t *testing.T) {
	store := newMemStateStore()
	path := filepath.Join(t.TempDir(), "does-not-exist.log")

	tailer, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	lines, err := tailer.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if lines != nil {
		t.Fatalf("expected no lines for a nonexistent file, got %v", lines)
	}
}

func TestTailer_ReadsNewlyAppendedLines(t *testing.T) {
	store := newMemStateStore()
	path := filepath.Join(t.TempDir(), "access.log")

	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	lines, err := tailer.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(lines) != 2 || lines[0] != "line1" || lines[1] != "line2" {
		t.Fatalf("unexpected lines: %v", lines)
	}

	if err := appendToFile(path, "line3\n"); err != nil {
		t.Fatalf("appendToFile: %v", err)
	}

	lines, err = tailer.Poll()
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(lines) != 1 || lines[0] != "line3" {
		t.Fatalf("expected only the newly appended line, got %v", lines)
	}
}

func TestTailer_LargeBacklogDrainsAcrossPollsBounded(t *testing.T) {
	store := newMemStateStore()
	path := filepath.Join(t.TempDir(), "access.log")

	// A backlog larger than one chunk (maxTailChunkBytes). Fixed 64-byte lines
	// make the arithmetic exact: 200k lines = ~12.8 MiB, so it can't come back
	// in a single bounded Poll.
	const lineCount = 200_000
	var b strings.Builder
	b.Grow(lineCount * 64)
	for i := 0; i < lineCount; i++ {
		fmt.Fprintf(&b, "%063d\n", i) // 63 digits + '\n' = 64 bytes
	}
	if int64(b.Len()) <= maxTailChunkBytes {
		t.Fatalf("test setup: backlog %d must exceed one chunk (%d)", b.Len(), maxTailChunkBytes)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	first, err := tailer.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(first) == 0 || len(first) >= lineCount {
		t.Fatalf("first Poll should return a bounded subset, got %d of %d", len(first), lineCount)
	}

	// Drain the rest; every line must arrive exactly once, in order.
	all := append([]string(nil), first...)
	polls := 1
	for {
		lines, err := tailer.Poll()
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if len(lines) == 0 {
			break
		}
		all = append(all, lines...)
		polls++
	}
	if polls < 2 {
		t.Fatalf("expected the backlog to span multiple polls, drained in %d", polls)
	}
	if len(all) != lineCount {
		t.Fatalf("expected %d lines total across polls, got %d", lineCount, len(all))
	}
	if all[0] != fmt.Sprintf("%063d", 0) || all[lineCount-1] != fmt.Sprintf("%063d", lineCount-1) {
		t.Fatalf("lines out of order or corrupted: first=%q last=%q", all[0], all[lineCount-1])
	}
}

func TestTailer_PartialLineCarriesOverToNextPoll(t *testing.T) {
	store := newMemStateStore()
	path := filepath.Join(t.TempDir(), "access.log")

	if err := os.WriteFile(path, []byte("complete\npartial-no-newline-yet"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	lines, err := tailer.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(lines) != 1 || lines[0] != "complete" {
		t.Fatalf("expected only the complete line, got %v", lines)
	}

	if err := appendToFile(path, "-now-finished\n"); err != nil {
		t.Fatalf("appendToFile: %v", err)
	}

	lines, err = tailer.Poll()
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(lines) != 1 || lines[0] != "partial-no-newline-yet-now-finished" {
		t.Fatalf("expected the carried-over partial line to complete, got %v", lines)
	}
}

func TestTailer_RestartResumesFromPersistedOffset(t *testing.T) {
	store := newMemStateStore()
	path := filepath.Join(t.TempDir(), "access.log")

	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer1, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	if _, err := tailer1.Poll(); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	tailer1.Close()

	if err := appendToFile(path, "line3\n"); err != nil {
		t.Fatalf("appendToFile: %v", err)
	}

	// A fresh Tailer (simulating a process restart) over the same store
	// must resume from the persisted offset, not duplicate line1/line2.
	tailer2, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer (restart): %v", err)
	}
	lines, err := tailer2.Poll()
	if err != nil {
		t.Fatalf("Poll after restart: %v", err)
	}
	if len(lines) != 1 || lines[0] != "line3" {
		t.Fatalf("expected no duplicate lines after restart, got %v", lines)
	}
}

func TestTailer_RotationByRenameIsDetectedViaInodeChange(t *testing.T) {
	store := newMemStateStore()
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	rotatedPath := filepath.Join(dir, "access.log.1")

	if err := os.WriteFile(path, []byte("old-line1\nold-line2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	if _, err := tailer.Poll(); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Simulate rename-based rotation: move the old file aside, create a
	// fresh one at the original path with a new inode.
	if err := os.Rename(path, rotatedPath); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.WriteFile(path, []byte("new-line1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile (new log): %v", err)
	}

	lines, err := tailer.Poll()
	if err != nil {
		t.Fatalf("Poll after rotation: %v", err)
	}
	if len(lines) != 1 || lines[0] != "new-line1" {
		t.Fatalf("expected only the new file's line after rotation, got %v", lines)
	}
}

func TestTailer_TruncationResetsOffset(t *testing.T) {
	store := newMemStateStore()
	path := filepath.Join(t.TempDir(), "access.log")

	if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tailer, err := NewTailer(path, store)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	if _, err := tailer.Poll(); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Simulate copy-truncate rotation: same inode, file shrinks.
	if err := os.WriteFile(path, []byte("fresh1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile (truncate): %v", err)
	}

	lines, err := tailer.Poll()
	if err != nil {
		t.Fatalf("Poll after truncation: %v", err)
	}
	if len(lines) != 1 || lines[0] != "fresh1" {
		t.Fatalf("expected only the post-truncation line, got %v", lines)
	}
}

func appendToFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
