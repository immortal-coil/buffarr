package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFixtureCacheFile builds a file shaped like a real nginx
// proxy_cache_path entry: a fixed-size binary header (336 bytes, confirmed
// against a real cache file written by this project's own nginx image),
// then "\nKEY: <rawKey>\n", then arbitrary "body" bytes representing the
// cached response. totalSize is the full file size reconcile.go sees via
// os.Stat -- it pads/truncates the body to make the file exactly that
// size, so tests can assert on a specific on-disk total.
func writeFixtureCacheFile(t *testing.T, path, rawKey string, totalSize int64) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	header := make([]byte, 336)
	keyLine := []byte("\nKEY: " + rawKey + "\n")

	minSize := int64(len(header) + len(keyLine))
	if totalSize < minSize {
		t.Fatalf("totalSize %d too small to fit the fixture header+KEY line (%d)", totalSize, minSize)
	}
	body := make([]byte, totalSize-minSize)

	content := append(header, keyLine...)
	content = append(content, body...)

	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestReconcile_CorrectsDriftedSize(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	cacheRoot := t.TempDir()

	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 500, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	// The on-disk file's real key has the slice_range suffix nginx adds,
	// which reconcile.go must strip to land on the same catalog cache key.
	writeFixtureCacheFile(t, filepath.Join(cacheRoot, "ab", "cd", "file1"), "steam/depot/731/chunk/abytes=0-1048575", 2000)

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.KeysCorrected != 1 {
		t.Fatalf("expected 1 key corrected, got %d (%+v)", result.KeysCorrected, result)
	}

	size, found, err := cat.CacheKeySize("steam/depot/731/chunk/a")
	if err != nil || !found {
		t.Fatalf("CacheKeySize: found=%v err=%v", found, err)
	}
	if size != 2000 {
		t.Fatalf("expected corrected size 2000, got %d", size)
	}
}

func TestReconcile_SumsMultipleSlicesUnderOneCacheKey(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	cacheRoot := t.TempDir()

	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 0, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	writeFixtureCacheFile(t, filepath.Join(cacheRoot, "aa", "bb", "slice0"), "steam/depot/731/chunk/abytes=0-1048575", 1000)
	writeFixtureCacheFile(t, filepath.Join(cacheRoot, "cc", "dd", "slice1"), "steam/depot/731/chunk/abytes=1048576-2097151", 1500)

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.FilesScanned != 2 {
		t.Fatalf("expected 2 files scanned, got %d", result.FilesScanned)
	}

	size, found, err := cat.CacheKeySize("steam/depot/731/chunk/a")
	if err != nil || !found {
		t.Fatalf("CacheKeySize: found=%v err=%v", found, err)
	}
	if size != 2500 {
		t.Fatalf("expected summed size across both slices (2500), got %d", size)
	}
}

func TestReconcile_NoCorrectionWhenSizeMatches(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	cacheRoot := t.TempDir()

	writeFixtureCacheFile(t, filepath.Join(cacheRoot, "ab", "cd", "file1"), "steam/depot/731/chunk/abytes=0-1048575", 2000)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 2000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.KeysCorrected != 0 {
		t.Fatalf("expected no correction when on-disk size already matches, got %d", result.KeysCorrected)
	}
}

func TestReconcile_OrphanedFileNotInCatalogIsCountedNotInserted(t *testing.T) {
	cat := openTestCatalog(t)
	cacheRoot := t.TempDir()

	writeFixtureCacheFile(t, filepath.Join(cacheRoot, "ab", "cd", "file1"), "epic/some/unknown/path", 1000)

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.OrphanedFiles != 1 {
		t.Fatalf("expected 1 orphaned file, got %d", result.OrphanedFiles)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected reconcile to not speculatively create new catalog entries, got %+v", entries)
	}
}

func TestReconcile_PrunesRowWhoseFileIsGone(t *testing.T) {
	cat := openTestCatalog(t)
	cacheRoot := t.TempDir()

	// Catalogued long ago, nothing on disk for it now (nginx evicted it).
	old := time.Now().Add(-time.Hour)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/gone", 500, old); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.KeysPruned != 1 {
		t.Fatalf("expected 1 stale key pruned, got %d (%+v)", result.KeysPruned, result)
	}
	if _, found, _ := cat.CacheKeySize("steam/depot/731/chunk/gone"); found {
		t.Fatalf("expected stale row to be pruned, but it is still present")
	}
}

func TestReconcile_DoesNotPruneRowSeenAfterWalkStart(t *testing.T) {
	cat := openTestCatalog(t)
	cacheRoot := t.TempDir()

	// A game whose download completes mid-walk: ingested with a fresh
	// last_seen_at (modeled here as just after the walk begins by using a
	// future timestamp), but its files land after the walker passed, so it's
	// absent from disk this pass. It must survive pruning -- the tailer's
	// offset is already past its log lines, so a wrongful prune is permanent.
	future := time.Now().Add(time.Minute)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/fresh", 500, future); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.KeysPruned != 0 {
		t.Fatalf("expected a row seen after walk start to be exempt from pruning, got %d pruned", result.KeysPruned)
	}
	if _, found, _ := cat.CacheKeySize("steam/depot/731/chunk/fresh"); !found {
		t.Fatalf("expected the freshly-seen row to survive reconcile, but it was pruned")
	}
}

func TestReconcile_EmptyCacheRoot(t *testing.T) {
	cat := openTestCatalog(t)
	cacheRoot := t.TempDir()

	result, err := Reconcile(cat, cacheRoot)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.FilesScanned != 0 {
		t.Fatalf("expected 0 files scanned for an empty cache root, got %d", result.FilesScanned)
	}
}
