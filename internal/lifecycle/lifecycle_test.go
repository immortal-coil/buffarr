package lifecycle

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/immortal-coil/buffarr/internal/catalog"
)

func openTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

func TestMonitor_SetsFlag(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	if err := Monitor(cat, "steam", "730"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}

	e, _, _ := cat.GetGame("steam", "730")
	if !e.Monitored {
		t.Fatalf("expected monitored=true after Monitor")
	}
}

// TestUnmonitor_LeavesFilesAndSizeUnchanged covers AE1 (R4, R5, R7).
func TestUnmonitor_LeavesFilesAndSizeUnchanged(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 5000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := Monitor(cat, "steam", "730"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}

	if err := Unmonitor(cat, "steam", "730"); err != nil {
		t.Fatalf("Unmonitor: %v", err)
	}

	e, found, err := cat.GetGame("steam", "730")
	if err != nil || !found {
		t.Fatalf("GetGame: found=%v err=%v", found, err)
	}
	if e.Monitored {
		t.Fatalf("expected monitored=false after Unmonitor")
	}
	if e.SizeBytes != 5000 || e.CacheKeyCount != 1 {
		t.Fatalf("expected files/size unchanged by Unmonitor, got %+v", e)
	}
}

// TestDelete_UnmonitoredHappyPath covers AE2 (R6, R7) for the optimistic,
// async delete: Delete returns the wildcard PURGE prefixes for the caller to
// evict in the background (it no longer purges inline) and clears the catalog
// immediately.
func TestDelete_UnmonitoredHappyPath(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/b", 2000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	prefixes, err := Delete(cat, "steam", "730", nil)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Both chunks share depot 731, so they reduce to exactly one wildcard
	// prefix for the background queue.
	if want := []string{"steam/depot/731/"}; !reflect.DeepEqual(prefixes, want) {
		t.Fatalf("expected one depot-level purge prefix %v, got %v", want, prefixes)
	}

	keys, err := cat.CacheKeysForGame("steam", "730")
	if err != nil {
		t.Fatalf("CacheKeysForGame: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected catalog cleared immediately (optimistic delete), got keys %v", keys)
	}
}

func TestDelete_RejectedWhileMonitored(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := Monitor(cat, "steam", "730"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}

	prefixes, err := Delete(cat, "steam", "730", nil)
	if !errors.Is(err, ErrMonitored) {
		t.Fatalf("expected ErrMonitored, got %v", err)
	}
	if prefixes != nil {
		t.Fatalf("expected no purge prefixes for a rejected delete, got %v", prefixes)
	}
	// Catalog must be untouched: a rejected delete clears nothing.
	if keys, _ := cat.CacheKeysForGame("steam", "730"); len(keys) != 1 {
		t.Fatalf("expected the monitored game's keys left intact, got %v", keys)
	}

	// Unmonitoring first must allow the delete to proceed.
	if err := Unmonitor(cat, "steam", "730"); err != nil {
		t.Fatalf("Unmonitor: %v", err)
	}
	prefixes, err = Delete(cat, "steam", "730", nil)
	if err != nil {
		t.Fatalf("Delete after Unmonitor: %v", err)
	}
	if len(prefixes) != 1 {
		t.Fatalf("expected one purge prefix after Unmonitor, got %v", prefixes)
	}
}

// The guarded clear Delete ends with must refuse a game locked after
// Delete's own up-front check (lock racing an eviction), not unlock it.
func TestDeleteCacheKeysIfUnmonitored_HonorsLateLock(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := Monitor(cat, "steam", "730"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}
	deleted, err := cat.DeleteCacheKeysIfUnmonitored("steam", "730")
	if err != nil || deleted {
		t.Fatalf("expected no delete for a locked game, got deleted=%v err=%v", deleted, err)
	}
	if g, _, _ := cat.GetGame("steam", "730"); !g.Monitored {
		t.Fatal("lock must survive")
	}
	if keys, _ := cat.CacheKeysForGame("steam", "730"); len(keys) != 1 {
		t.Fatalf("expected keys intact, got %v", keys)
	}
}

// TestDelete_ReCacheRevivesSameEntry covers AE4 (R16).
func TestDelete_ReCacheRevivesSameEntry(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	if _, err := Delete(cat, "steam", "730", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Simulate the same game being cached again later (a fresh chunk hash
	// under the same depot, as a real re-download would produce).
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/c", 1500, now); err != nil {
		t.Fatalf("UpsertCacheKey (re-cache): %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the re-cached game to reappear as one entry, not a duplicate, got %+v", entries)
	}
	if entries[0].Monitored {
		t.Fatalf("expected the revived entry to start unmonitored (R16)")
	}
}

// TestDelete_ZeroKeysIsNoOp covers the edge case where the catalog already
// shows zero keys for a game the operator's UI still has loaded (e.g.
// reconciliation or a prior delete raced ahead of this click): a no-op
// success with no prefixes to evict.
func TestDelete_ZeroKeysIsNoOp(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.DeleteCacheKeysForGame("steam", "730"); err != nil {
		t.Fatalf("DeleteCacheKeysForGame: %v", err)
	}

	prefixes, err := Delete(cat, "steam", "730", nil)
	if err != nil {
		t.Fatalf("expected a no-op success for a game with zero keys, got: %v", err)
	}
	if prefixes != nil {
		t.Fatalf("expected no purge prefixes when there are no keys to evict, got %v", prefixes)
	}
}

// TestDelete_WidensToKnownDepotsWithZeroTrackedKeys covers the case this
// session's investigation surfaced: a real Steam client's own download can
// land files on disk the nginx-log tailer never attributed to the game, so
// cache_keys shows 0 even though Steam's API still lists depots for the app.
// Delete must still purge those depots instead of taking the zero-keys no-op
// path, or the orphaned files are never reclaimed.
func TestDelete_WidensToKnownDepotsWithZeroTrackedKeys(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.DeleteCacheKeysForGame("steam", "730"); err != nil {
		t.Fatalf("DeleteCacheKeysForGame: %v", err)
	}

	depotsForApp := func(appID int) []int {
		if appID != 730 {
			t.Fatalf("expected lookup for appID 730, got %d", appID)
		}
		return []int{731, 732}
	}

	prefixes, err := Delete(cat, "steam", "730", depotsForApp)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := []string{"steam/depot/731/", "steam/depot/732/"}
	if !reflect.DeepEqual(prefixes, want) {
		t.Fatalf("expected widened purge prefixes for untracked depots %v, got %v", want, prefixes)
	}
}

func TestDelete_NotFoundIsAnError(t *testing.T) {
	cat := openTestCatalog(t)
	prefixes, err := Delete(cat, "steam", "nonexistent", nil)
	if !errors.Is(err, ErrGameNotFound) {
		t.Fatalf("expected ErrGameNotFound, got %v", err)
	}
	if prefixes != nil {
		t.Fatalf("expected no prefixes for a not-found game, got %v", prefixes)
	}
}

// TestPurgeOrphans_DeclutterAllReclaimBig covers the "declutter + reclaim big"
// contract: every orphan (below the cutoff, not Pre-Staged, not monitored) is
// cleared from the catalog immediately, but only orphans at least
// minPurgeSizeBytes are returned as PURGE prefixes -- sub-threshold slivers are
// list-cleared only (nginx's LRU reclaims their trivial bytes; a full-zone
// scan to evict a few bytes is never worth it). Protected and big entries are
// left fully intact.
func TestPurgeOrphans_DeclutterAllReclaimBig(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	cutoff := catalog.MinLibrarySizeBytes
	minPurge := catalog.MinGuestSizeBytes // 1 MiB

	// Big, unprotected -> not an orphan, untouched.
	if err := cat.UpsertCacheKey("steam", "440", "Big Owned", "steam/depot/441/chunk/a", catalog.MinLibrarySizeBytes+1, now); err != nil {
		t.Fatalf("UpsertCacheKey (big): %v", err)
	}
	// Small but Pre-Staged -> not an orphan, untouched.
	if err := cat.UpsertCacheKey("steam", "1840", "Oil Rush", "steam/depot/1841/chunk/a", 100*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (prestaged): %v", err)
	}
	if err := cat.MarkPrestagePending("req-1", "steam", "1840", "Oil Rush"); err != nil {
		t.Fatalf("MarkPrestagePending: %v", err)
	}
	if found, err := cat.CompletePrestage("req-1"); err != nil || !found {
		t.Fatalf("CompletePrestage: found=%v err=%v", found, err)
	}
	// Small but monitored -> not an orphan, untouched.
	if err := cat.UpsertCacheKey("steam", "999", "Watched", "steam/depot/9991/chunk/a", 100*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (monitored): %v", err)
	}
	if err := Monitor(cat, "steam", "999"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}
	// Medium orphan (>= minPurge, < cutoff) -> cleared AND enqueued.
	if err := cat.UpsertCacheKey("steam", "depot:6000", "Steam Depot 6000", "steam/depot/6000/chunk/a", 4*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (medium orphan): %v", err)
	}
	// Tiny orphan (< minPurge) -> cleared but NOT enqueued.
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/manifest", 92, now); err != nil {
		t.Fatalf("UpsertCacheKey (tiny orphan): %v", err)
	}

	prefixes, err := PurgeOrphans(cat, cutoff, minPurge)
	if err != nil {
		t.Fatalf("PurgeOrphans: %v", err)
	}

	// Only the medium orphan is worth a zone scan.
	if want := []string{"steam/depot/6000/"}; !reflect.DeepEqual(prefixes, want) {
		t.Fatalf("expected only the >=1 MiB orphan's prefix %v, got %v", want, prefixes)
	}

	// Both orphans cleared from the catalog regardless of size.
	for _, k := range []string{"depot:6000", "730"} {
		if keys, _ := cat.CacheKeysForGame("steam", k); len(keys) != 0 {
			t.Fatalf("expected orphan %q cleared, still has keys %v", k, keys)
		}
	}
	// Protected and big entries untouched.
	for _, k := range []string{"440", "1840", "999"} {
		if keys, _ := cat.CacheKeysForGame("steam", k); len(keys) != 1 {
			t.Fatalf("expected protected/big entry %q left intact, got keys %v", k, keys)
		}
	}
}
