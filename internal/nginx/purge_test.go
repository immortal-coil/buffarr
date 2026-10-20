package nginx

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPurgePrefix_SteamDepotLevel(t *testing.T) {
	got := PurgePrefix("steam/depot/730/chunk/abc123")
	if got != "steam/depot/730/" {
		t.Fatalf("PurgePrefix = %q, want steam/depot/730/", got)
	}
}

func TestPurgePrefix_BattlenetNGDPLevel(t *testing.T) {
	got := PurgePrefix("dist.blizzard.com/tpr/wow/wow-update-12345.zip")
	if got != "dist.blizzard.com/tpr/wow/" {
		t.Fatalf("PurgePrefix = %q, want dist.blizzard.com/tpr/wow/", got)
	}
}

func TestPurgePrefix_UnstructuredFallsBackToExactKey(t *testing.T) {
	got := PurgePrefix("epic.example.com/some/random/path")
	if got != "epic.example.com/some/random/path" {
		t.Fatalf("PurgePrefix = %q, want the exact key unchanged", got)
	}
}

func TestPurgePrefixes_DedupesAcrossSharedDepot(t *testing.T) {
	keys := []string{
		"steam/depot/730/chunk/a",
		"steam/depot/730/chunk/b",
		"steam/depot/731/chunk/c",
	}
	got := PurgePrefixes(keys)
	if len(got) != 2 {
		t.Fatalf("expected 2 deduped prefixes (one per depot), got %v", got)
	}
	want := map[string]bool{"steam/depot/730/": true, "steam/depot/731/": true}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("unexpected prefix %q in %v", p, got)
		}
	}
}

func TestPurgePrefixes_MultiHostBattlenetProducesDistinctPrefixes(t *testing.T) {
	keys := []string{
		"dist.blizzard.com/tpr/wow/a",
		"blizzard.vo.llnwd.net/tpr/wow/b",
	}
	got := PurgePrefixes(keys)
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct host-scoped prefixes, got %v", got)
	}
}

func TestPurgePrefixes_EmptyInput(t *testing.T) {
	if got := PurgePrefixes(nil); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

// makeFakeCacheFile writes a minimal nginx-format file (KEY: header in first
// bytes) under a two-level subdirectory, matching nginx's actual cache layout.
func makeFakeCacheFile(t *testing.T, dir, cacheKey string) string {
	t.Helper()
	sub := filepath.Join(dir, "ab", "cd")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(sub, "cache-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\x00\x00\x00KEY: %s\n", cacheKey); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestWalkPurge_DeletesMatchingFile(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/730/chunk/abc")

	deleted, err := WalkPurge(dir, []string{"steam/depot/730/"})
	if err != nil {
		t.Fatalf("WalkPurge: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, got: %v", err)
	}
}

func TestWalkPurge_SkipsNonMatchingFile(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/731/chunk/abc")

	deleted, err := WalkPurge(dir, []string{"steam/depot/730/"})
	if err != nil {
		t.Fatalf("WalkPurge: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("non-matching file should remain: %v", err)
	}
}

func TestWalkPurge_MultiplePrefixesSingleWalk(t *testing.T) {
	dir := t.TempDir()
	makeFakeCacheFile(t, dir, "steam/depot/730/chunk/a")
	makeFakeCacheFile(t, dir, "steam/depot/731/chunk/b")
	keep := makeFakeCacheFile(t, dir, "steam/depot/732/chunk/c")

	deleted, err := WalkPurge(dir, []string{"steam/depot/730/", "steam/depot/731/"})
	if err != nil {
		t.Fatalf("WalkPurge: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-matching file should remain: %v", err)
	}
}

func TestWalkPurge_EmptyPrefixesIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/730/chunk/abc")

	deleted, err := WalkPurge(dir, nil)
	if err != nil {
		t.Fatalf("WalkPurge: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should remain after no-op: %v", err)
	}
}

func TestWalkPurge_SliceSuffixStripped(t *testing.T) {
	dir := t.TempDir()
	// Raw on-disk key includes the slice range suffix.
	sub := filepath.Join(dir, "ab")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "sliced")
	content := "KEY: steam/depot/730/chunk/abcbytes=0-1048575\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prefix matches the stripped key.
	deleted, err := WalkPurge(dir, []string{"steam/depot/730/"})
	if err != nil {
		t.Fatalf("WalkPurge: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (slice suffix should be stripped before match)", deleted)
	}
}
