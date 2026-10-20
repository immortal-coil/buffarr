package catalog

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

func TestUpsertCacheKey_CreatesGameAndCacheKey(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()

	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/abc", 1024, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 library entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Platform != "steam" || e.GameKey != "730" || e.Name != "Counter-Strike 2" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.SizeBytes != 1024 {
		t.Fatalf("expected size 1024, got %d", e.SizeBytes)
	}
	if e.CacheKeyCount != 1 {
		t.Fatalf("expected 1 cache key, got %d", e.CacheKeyCount)
	}
}

func TestUpsertCacheKey_SumsMultipleCacheKeysUnderOneGame(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()

	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/b", 2000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected entries from the same game to collapse into 1 library entry, got %d", len(entries))
	}
	if entries[0].SizeBytes != 3000 {
		t.Fatalf("expected summed size 3000, got %d", entries[0].SizeBytes)
	}
	if entries[0].CacheKeyCount != 2 {
		t.Fatalf("expected 2 cache keys, got %d", entries[0].CacheKeyCount)
	}
}

func TestUpsertCacheKey_ReUpsertUpdatesSizeAndName(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()

	if err := cat.UpsertCacheKey("steam", "steam:unidentified", "steam:unidentified", "steam/depot/999/chunk/a", 500, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	// Simulate the same depot later resolving to a real game name -- the
	// game row's name must reflect the newer, better-informed value.
	if err := cat.UpsertCacheKey("steam", "999", "Some Game", "steam/depot/999/chunk/a", 600, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after re-upsert under a new game key, got %d entries: %+v", len(entries), entries)
	}
	if entries[0].GameKey != "999" || entries[0].Name != "Some Game" || entries[0].SizeBytes != 600 {
		t.Fatalf("unexpected entry after re-upsert: %+v", entries[0])
	}
}

func TestList_ExcludesGuestDepotAndUnidentified(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	big := MinLibrarySizeBytes + 1 // above the floor, so size isn't what hides them

	if err := cat.UpsertCacheKey("steam", "440", "Team Fortress 2", "steam/depot/441/chunk/a", big, now); err != nil {
		t.Fatalf("UpsertCacheKey (owned): %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "depot:229006", "Steam Depot 229006", "steam/depot/229006/chunk/a", big, now); err != nil {
		t.Fatalf("UpsertCacheKey (depot): %v", err)
	}
	if err := cat.UpsertCacheKey("cache1.valve.net", "cache1.valve.net:unidentified", "cache1.valve.net:unidentified", "cache1.valve.net/x", big, now); err != nil {
		t.Fatalf("UpsertCacheKey (unidentified): %v", err)
	}

	// The Library shows only the owned, identified game; guest depots and
	// unidentified host buckets belong to GuestGames(), not here.
	entries, err := cat.List(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].GameKey != "440" {
		t.Fatalf("Library should list only the owned game, not guest depot/unidentified buckets, got %+v", entries)
	}

	guests, err := cat.GuestGames(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("GuestGames: %v", err)
	}
	if len(guests) != 2 {
		t.Fatalf("expected the depot and unidentified entries under GuestGames, got %+v", guests)
	}

	// And the headline count agrees with the visible Library (1 owned game).
	n, err := cat.CachedGameCount(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("CachedGameCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("CachedGameCount should match the Library's 1 visible row, got %d", n)
	}
}

func TestList_OrdersByName(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()

	if err := cat.UpsertCacheKey("steam", "2", "Zeta Game", "steam/depot/2/chunk/a", 1, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "1", "Alpha Game", "steam/depot/1/chunk/a", 1, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "Alpha Game" || entries[1].Name != "Zeta Game" {
		t.Fatalf("expected entries ordered by name, got %+v", entries)
	}
}

// TestOpen_MigratesPreStagedColumnOnOldDB exercises the ALTER-TABLE path that
// only runs in production: a catalog created before pre_staged existed must be
// migrated in place on the deploy that introduces it, not rejected. Every
// other test opens a fresh DB (which gets pre_staged from the CREATE TABLE),
// so without this test the migration guard ships unexercised -- and a
// misparse there would fail Open() -> log.Fatalf -> backend down on deploy.
func TestOpen_MigratesPreStagedColumnOnOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build the pre-pre_staged on-disk shape: games has no pre_staged column.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE games (
			platform  TEXT NOT NULL,
			game_key  TEXT NOT NULL,
			name      TEXT NOT NULL,
			monitored INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (platform, game_key)
		);
		CREATE TABLE cache_keys (
			cache_key    TEXT PRIMARY KEY,
			platform     TEXT NOT NULL,
			game_key     TEXT NOT NULL,
			size_bytes   INTEGER NOT NULL DEFAULT 0,
			last_seen_at TEXT NOT NULL
		);
		INSERT INTO games (platform, game_key, name, monitored) VALUES ('steam','440','Team Fortress 2',0);`); err != nil {
		t.Fatalf("creating old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("closing old db: %v", err)
	}

	// Open must migrate (ALTER ADD COLUMN pre_staged) without error.
	cat, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-migration DB must succeed, got: %v", err)
	}

	// The pre-existing row must have gained pre_staged defaulted to 0. A
	// missing column would make this Scan error with "no such column".
	verify, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open (verify): %v", err)
	}
	var ps int
	if err := verify.QueryRow(`SELECT pre_staged FROM games WHERE platform='steam' AND game_key='440'`).Scan(&ps); err != nil {
		t.Fatalf("reading migrated pre_staged column: %v", err)
	}
	if ps != 0 {
		t.Fatalf("expected migrated column to default to 0, got %d", ps)
	}
	verify.Close()
	cat.Close()

	// Re-opening must be a no-op: columnExists sees the column and skips the
	// ALTER (a second ALTER would fail with "duplicate column name").
	cat2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open after migration must be a no-op, got: %v", err)
	}
	cat2.Close()
}

// TestList_PreStagedGameExemptFromSizeFloor proves the pre_staged exemption:
// a small, identified game falls into Guest (below the 5 GiB floor) until the
// operator deliberately Pre-Stages it, after which it stays in the Library
// regardless of size -- and is not double-listed in Guest.
func TestList_PreStagedGameExemptFromSizeFloor(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	small := int64(100 * 1024 * 1024) // 100 MiB, well below the 5 GiB floor

	if err := cat.UpsertCacheKey("steam", "1840", "Oil Rush", "steam/depot/1841/chunk/a", small, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	// Before pre-staging: not in Library, present in Guest.
	if entries, err := cat.List(MinLibrarySizeBytes); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("a small non-pre-staged game must not appear in the Library, got %+v", entries)
	}
	if guests, err := cat.GuestGames(MinLibrarySizeBytes); err != nil {
		t.Fatalf("GuestGames: %v", err)
	} else if len(guests) != 1 || guests[0].GameKey != "1840" {
		t.Fatalf("a small non-pre-staged identified game must fall into Guest, got %+v", guests)
	}

	// Operator Pre-Stages it.
	if err := cat.MarkPrestagePending("req-1", "steam", "1840", "Oil Rush"); err != nil {
		t.Fatalf("MarkPrestagePending: %v", err)
	}
	if found, err := cat.CompletePrestage("req-1"); err != nil || !found {
		t.Fatalf("CompletePrestage: found=%v err=%v", found, err)
	}

	// After: stays in Library below the floor, leaves Guest (no double-listing).
	if entries, err := cat.List(MinLibrarySizeBytes); err != nil {
		t.Fatalf("List after prestage: %v", err)
	} else if len(entries) != 1 || entries[0].GameKey != "1840" {
		t.Fatalf("a pre-staged game must stay in the Library below the floor, got %+v", entries)
	}
	if guests, err := cat.GuestGames(MinLibrarySizeBytes); err != nil {
		t.Fatalf("GuestGames after prestage: %v", err)
	} else if len(guests) != 0 {
		t.Fatalf("a pre-staged game must not also appear in Guest, got %+v", guests)
	}
	if n, err := cat.CachedGameCount(MinLibrarySizeBytes); err != nil {
		t.Fatalf("CachedGameCount: %v", err)
	} else if n != 1 {
		t.Fatalf("CachedGameCount must count the pre-staged game, got %d", n)
	}

	// CompletePrestage is idempotent: the pending row is gone, so a repeat is
	// a no-op (found=false), not a double-apply or error.
	if found, err := cat.CompletePrestage("req-1"); err != nil || found {
		t.Fatalf("expected the second CompletePrestage to be a no-op, found=%v err=%v", found, err)
	}
}

// TestGuestGames_FloorDropsTinyNoise covers the "Counter-Strike 2 -- 92 B"
// report: sub-1-MiB depot slivers are noise and must not show in Guest, while
// a real guest download above the floor still does.
func TestGuestGames_FloorDropsTinyNoise(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()

	if err := cat.UpsertCacheKey("steam", "depot:730", "Counter-Strike 2", "steam/depot/730/chunk/a", 92, now); err != nil {
		t.Fatalf("UpsertCacheKey (noise): %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "depot:5000", "Steam Depot 5000", "steam/depot/5000/chunk/a", 4*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (real guest): %v", err)
	}

	guests, err := cat.GuestGames(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("GuestGames: %v", err)
	}
	if len(guests) != 1 || guests[0].GameKey != "depot:5000" {
		t.Fatalf("expected only the >=1 MiB guest entry (92 B sliver dropped), got %+v", guests)
	}
}

// TestTinyIdentifiedGameVanishesFromBothViews pins the operator's literal
// report ("Counter-Strike 2 steam 92 B"): an *identified* game (key "730",
// not a depot:/unidentified bucket) that incidentally cached only a sliver
// must show in neither the Library (below the 5 GiB floor, not pre-staged)
// nor Guest (below the 1 MiB floor). The guest floor is identifier-agnostic,
// so a named 92 B sliver drops exactly like an anonymous one.
func TestTinyIdentifiedGameVanishesFromBothViews(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/manifest", 92, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	if entries, err := cat.List(MinLibrarySizeBytes); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("a 92 B identified game must not appear in the Library, got %+v", entries)
	}
	if guests, err := cat.GuestGames(MinLibrarySizeBytes); err != nil {
		t.Fatalf("GuestGames: %v", err)
	} else if len(guests) != 0 {
		t.Fatalf("a 92 B identified game must not appear in Guest either, got %+v", guests)
	}
}

// TestClearPrestagePending_DoesNotExemptFromFloor covers the failed-run path:
// a Pre-Stage that failed/timed out clears its pending row WITHOUT setting
// pre_staged, so the game stays subject to the normal size floor.
func TestClearPrestagePending_DoesNotExemptFromFloor(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	small := int64(100 * 1024 * 1024)
	if err := cat.UpsertCacheKey("steam", "1840", "Oil Rush", "steam/depot/1841/chunk/a", small, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.MarkPrestagePending("req-x", "steam", "1840", "Oil Rush"); err != nil {
		t.Fatalf("MarkPrestagePending: %v", err)
	}
	if err := cat.ClearPrestagePending("req-x"); err != nil {
		t.Fatalf("ClearPrestagePending: %v", err)
	}
	// The pending row is gone, so a later CompletePrestage finds nothing.
	if found, err := cat.CompletePrestage("req-x"); err != nil || found {
		t.Fatalf("expected no pending row after clear, found=%v err=%v", found, err)
	}
	// And the game is NOT exempted from the floor.
	if entries, err := cat.List(MinLibrarySizeBytes); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("a failed (cleared) prestage must not exempt the game from the floor, got %+v", entries)
	}
}

func TestCacheKeySize_NotFound(t *testing.T) {
	cat := openTestCatalog(t)
	if _, found, err := cat.CacheKeySize("nonexistent"); err != nil || found {
		t.Fatalf("expected found=false for a nonexistent cache key, got found=%v err=%v", found, err)
	}
}

func TestSetCacheKeySize_CorrectsExistingRow(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	if err := cat.SetCacheKeySize("steam/depot/731/chunk/a", 5000); err != nil {
		t.Fatalf("SetCacheKeySize: %v", err)
	}

	size, found, err := cat.CacheKeySize("steam/depot/731/chunk/a")
	if err != nil || !found {
		t.Fatalf("CacheKeySize: found=%v err=%v", found, err)
	}
	if size != 5000 {
		t.Fatalf("expected corrected size 5000, got %d", size)
	}
}

func TestTailState_SaveAndLoadRoundTrip(t *testing.T) {
	cat := openTestCatalog(t)

	if _, found, err := cat.LoadTailState("/var/log/nginx/cache-access.log"); err != nil || found {
		t.Fatalf("expected no state before the first save, found=%v err=%v", found, err)
	}

	want := TailState{Inode: 12345, Offset: 6789}
	if err := cat.SaveTailState("/var/log/nginx/cache-access.log", want); err != nil {
		t.Fatalf("SaveTailState: %v", err)
	}

	got, found, err := cat.LoadTailState("/var/log/nginx/cache-access.log")
	if err != nil || !found {
		t.Fatalf("LoadTailState: found=%v err=%v", found, err)
	}
	if got != want {
		t.Fatalf("LoadTailState = %+v, want %+v", got, want)
	}
}

func TestGetGame_ReturnsGameWithZeroCacheKeys(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.DeleteCacheKeysForGame("steam", "730"); err != nil {
		t.Fatalf("DeleteCacheKeysForGame: %v", err)
	}

	// Unlike List() (which hides zero-key games), GetGame must still find
	// the game row -- a caller may be acting on a stale UI snapshot of a
	// game that's since been emptied.
	e, found, err := cat.GetGame("steam", "730")
	if err != nil || !found {
		t.Fatalf("GetGame: found=%v err=%v", found, err)
	}
	if e.SizeBytes != 0 || e.CacheKeyCount != 0 {
		t.Fatalf("expected zeroed entry, got %+v", e)
	}
}

func TestGetGame_NotFound(t *testing.T) {
	cat := openTestCatalog(t)
	_, found, err := cat.GetGame("steam", "nonexistent")
	if err != nil || found {
		t.Fatalf("expected found=false, got found=%v err=%v", found, err)
	}
}

func TestSetMonitored_TogglesFlag(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	if err := cat.SetMonitored("steam", "730", true); err != nil {
		t.Fatalf("SetMonitored(true): %v", err)
	}
	e, found, err := cat.GetGame("steam", "730")
	if err != nil || !found || !e.Monitored {
		t.Fatalf("expected monitored=true, got %+v found=%v err=%v", e, found, err)
	}

	if err := cat.SetMonitored("steam", "730", false); err != nil {
		t.Fatalf("SetMonitored(false): %v", err)
	}
	e, found, err = cat.GetGame("steam", "730")
	if err != nil || !found || e.Monitored {
		t.Fatalf("expected monitored=false, got %+v found=%v err=%v", e, found, err)
	}
}

func TestCacheKeysForGame_ReturnsAllDistinctKeys(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/732/chunk/b", 200, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	keys, err := cat.CacheKeysForGame("steam", "730")
	if err != nil {
		t.Fatalf("CacheKeysForGame: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 cache keys, got %v", keys)
	}
}

func TestDeleteCacheKeysForGame_RemovesRowsButKeepsGame(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	if err := cat.DeleteCacheKeysForGame("steam", "730"); err != nil {
		t.Fatalf("DeleteCacheKeysForGame: %v", err)
	}

	keys, err := cat.CacheKeysForGame("steam", "730")
	if err != nil {
		t.Fatalf("CacheKeysForGame: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected no cache keys left, got %v", keys)
	}

	// The game row itself must survive so a later re-cache revives the same
	// logical entry (R16) instead of creating a duplicate.
	_, found, err := cat.GetGame("steam", "730")
	if err != nil || !found {
		t.Fatalf("expected the game row to still exist after delete, found=%v err=%v", found, err)
	}
}

func TestDeleteCacheKeysForGame_ReCacheRevivesSameEntry(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := cat.DeleteCacheKeysForGame("steam", "730"); err != nil {
		t.Fatalf("DeleteCacheKeysForGame: %v", err)
	}

	// Re-cache under the same stable game_key (R16).
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/c", 500, now); err != nil {
		t.Fatalf("UpsertCacheKey (re-cache): %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].GameKey != "730" || entries[0].SizeBytes != 500 {
		t.Fatalf("expected exactly 1 revived entry, got %+v", entries)
	}
}

func TestTailState_SaveOverwritesPriorState(t *testing.T) {
	cat := openTestCatalog(t)
	path := "/var/log/nginx/cache-access.log"

	if err := cat.SaveTailState(path, TailState{Inode: 1, Offset: 100}); err != nil {
		t.Fatalf("first SaveTailState: %v", err)
	}
	if err := cat.SaveTailState(path, TailState{Inode: 2, Offset: 200}); err != nil {
		t.Fatalf("second SaveTailState: %v", err)
	}

	got, found, err := cat.LoadTailState(path)
	if err != nil || !found {
		t.Fatalf("LoadTailState: found=%v err=%v", found, err)
	}
	if got != (TailState{Inode: 2, Offset: 200}) {
		t.Fatalf("expected the latest state to win, got %+v", got)
	}
}

func TestSeedPlatformsIfEmpty_SeedsOnFirstBoot(t *testing.T) {
	cat := openTestCatalog(t)

	if err := cat.SeedPlatformsIfEmpty([]string{"steam", "blizzard", "epicgames"}, []string{"steam"}); err != nil {
		t.Fatalf("SeedPlatformsIfEmpty: %v", err)
	}

	names, err := cat.EnabledPlatformNames()
	if err != nil {
		t.Fatalf("EnabledPlatformNames: %v", err)
	}
	if len(names) != 1 || names[0] != "steam" {
		t.Fatalf("expected only steam seeded as enabled, got %v", names)
	}
}

func TestSeedPlatformsIfEmpty_DoesNotOverwriteExistingToggles(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.SeedPlatformsIfEmpty([]string{"steam", "blizzard"}, []string{"steam"}); err != nil {
		t.Fatalf("first SeedPlatformsIfEmpty: %v", err)
	}
	if err := cat.SetPlatformEnabled("steam", false); err != nil {
		t.Fatalf("SetPlatformEnabled: %v", err)
	}

	// Simulate a restart: seeding again (e.g. a new platform appearing in
	// the vendored list) must not reset the operator's prior toggle.
	if err := cat.SeedPlatformsIfEmpty([]string{"steam", "blizzard", "epicgames"}, []string{"steam"}); err != nil {
		t.Fatalf("second SeedPlatformsIfEmpty: %v", err)
	}

	enabled, found, err := cat.PlatformEnabled("steam")
	if err != nil || !found {
		t.Fatalf("PlatformEnabled: found=%v err=%v", found, err)
	}
	if enabled {
		t.Fatalf("expected steam to remain disabled after a no-op reseed")
	}
}

func TestSetPlatformEnabled_TogglesAndPersists(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.SeedPlatformsIfEmpty([]string{"steam"}, []string{"steam"}); err != nil {
		t.Fatalf("SeedPlatformsIfEmpty: %v", err)
	}

	if err := cat.SetPlatformEnabled("steam", false); err != nil {
		t.Fatalf("SetPlatformEnabled(false): %v", err)
	}
	enabled, found, err := cat.PlatformEnabled("steam")
	if err != nil || !found || enabled {
		t.Fatalf("expected steam disabled, got enabled=%v found=%v err=%v", enabled, found, err)
	}

	if err := cat.SetPlatformEnabled("steam", true); err != nil {
		t.Fatalf("SetPlatformEnabled(true): %v", err)
	}
	enabled, found, err = cat.PlatformEnabled("steam")
	if err != nil || !found || !enabled {
		t.Fatalf("expected steam re-enabled, got enabled=%v found=%v err=%v", enabled, found, err)
	}
}

func TestPlatformEnabled_NotFound(t *testing.T) {
	cat := openTestCatalog(t)
	_, found, err := cat.PlatformEnabled("nonexistent")
	if err != nil || found {
		t.Fatalf("expected found=false, got found=%v err=%v", found, err)
	}
}

func TestEnabledPlatformNames_ExcludesDisabled(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.SeedPlatformsIfEmpty([]string{"steam", "blizzard", "epicgames"}, []string{"steam", "blizzard"}); err != nil {
		t.Fatalf("SeedPlatformsIfEmpty: %v", err)
	}
	if err := cat.SetPlatformEnabled("blizzard", false); err != nil {
		t.Fatalf("SetPlatformEnabled: %v", err)
	}

	names, err := cat.EnabledPlatformNames()
	if err != nil {
		t.Fatalf("EnabledPlatformNames: %v", err)
	}
	for _, n := range names {
		if n == "blizzard" {
			t.Fatalf("expected blizzard excluded from the enabled set, got %v", names)
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 enabled platform after disabling blizzard, got %v", names)
	}
}

func TestGetSetting_NotFound(t *testing.T) {
	cat := openTestCatalog(t)
	_, found, err := cat.GetSetting("steam_web_api_key")
	if err != nil || found {
		t.Fatalf("expected found=false, got found=%v err=%v", found, err)
	}
}

func TestSetSetting_RoundTrip(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.SetSetting("steam_web_api_key", "ABC123"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	value, found, err := cat.GetSetting("steam_web_api_key")
	if err != nil || !found || value != "ABC123" {
		t.Fatalf("GetSetting: value=%q found=%v err=%v", value, found, err)
	}
}

func TestSetSetting_OverwritesExisting(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.SetSetting("steam_operator_id64", "111"); err != nil {
		t.Fatalf("first SetSetting: %v", err)
	}
	if err := cat.SetSetting("steam_operator_id64", "222"); err != nil {
		t.Fatalf("second SetSetting: %v", err)
	}

	value, found, err := cat.GetSetting("steam_operator_id64")
	if err != nil || !found || value != "222" {
		t.Fatalf("expected the latest value to win, got value=%q found=%v err=%v", value, found, err)
	}
}

// TestList_GuestGames_PartitionAtNonDefaultFloor proves the Library/Guest
// split stays a clean partition at a floor OTHER than the default 5 GiB. This
// is the regression guard for the configurable MIN_GAME_SIZE: List,
// GuestGames, and CachedGameCount all take the floor as a parameter now, and a
// caller that passed different floors to two of them would silently double-
// list or drop a game. Here every view gets the same non-default floor (2 GiB)
// and we assert no game_key appears in both views and the count matches List.
func TestList_GuestGames_PartitionAtNonDefaultFloor(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	const floor = int64(2 * 1024 * 1024 * 1024) // 2 GiB, deliberately != MinLibrarySizeBytes

	// Above the floor, identified -> Library.
	if err := cat.UpsertCacheKey("steam", "440", "Big Owned", "steam/depot/441/chunk/a", 3*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (big owned): %v", err)
	}
	// Below the floor, identified, not pre-staged -> Guest (incidental).
	if err := cat.UpsertCacheKey("steam", "730", "Small Owned", "steam/depot/731/chunk/a", 1*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (small owned): %v", err)
	}
	// A depot bucket above the guest floor -> Guest.
	if err := cat.UpsertCacheKey("steam", "depot:6000", "Steam Depot 6000", "steam/depot/6000/chunk/a", 4*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (depot): %v", err)
	}
	// Sub-guest-floor sliver -> neither view.
	if err := cat.UpsertCacheKey("steam", "depot:7000", "Steam Depot 7000", "steam/depot/7000/chunk/a", 92, now); err != nil {
		t.Fatalf("UpsertCacheKey (sliver): %v", err)
	}

	lib, err := cat.List(floor)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	guests, err := cat.GuestGames(floor)
	if err != nil {
		t.Fatalf("GuestGames: %v", err)
	}

	libKeys := map[string]bool{}
	for _, e := range lib {
		libKeys[e.GameKey] = true
	}
	if !libKeys["440"] || len(lib) != 1 {
		t.Fatalf("Library at the 2 GiB floor should be exactly {440}, got %+v", lib)
	}
	guestKeys := map[string]bool{}
	for _, e := range guests {
		guestKeys[e.GameKey] = true
	}
	if !guestKeys["730"] || !guestKeys["depot:6000"] || len(guests) != 2 {
		t.Fatalf("Guest at the 2 GiB floor should be exactly {730, depot:6000}, got %+v", guests)
	}

	// The defining property: no game appears in both views.
	for k := range libKeys {
		if guestKeys[k] {
			t.Fatalf("partition broken: %q appears in both Library and Guest at the same floor", k)
		}
	}

	// CachedGameCount must agree with the visible Library at the same floor.
	n, err := cat.CachedGameCount(floor)
	if err != nil {
		t.Fatalf("CachedGameCount: %v", err)
	}
	if n != len(lib) {
		t.Fatalf("CachedGameCount(%d)=%d must equal the Library row count %d", floor, n, len(lib))
	}
}

// TestOrphanGames_BelowFloorAndUnprotectedOnly proves the orphan predicate:
// an entry is an orphan iff its aggregated size is below the cutoff AND it is
// neither Pre-Staged nor monitored. Big entries, Pre-Staged entries, and
// monitored entries are never orphans regardless of size. Each returned group
// must carry its size and cache keys so the caller can derive PURGE prefixes.
func TestOrphanGames_BelowFloorAndUnprotectedOnly(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	const cutoff = MinLibrarySizeBytes

	// Big, unprotected -> NOT an orphan (size >= cutoff).
	if err := cat.UpsertCacheKey("steam", "440", "Big Owned", "steam/depot/441/chunk/a", MinLibrarySizeBytes+1, now); err != nil {
		t.Fatalf("UpsertCacheKey (big): %v", err)
	}
	// Small but Pre-Staged -> NOT an orphan (deliberately kept).
	if err := cat.UpsertCacheKey("steam", "1840", "Oil Rush", "steam/depot/1841/chunk/a", 100*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (prestaged): %v", err)
	}
	if err := cat.MarkPrestagePending("req-1", "steam", "1840", "Oil Rush"); err != nil {
		t.Fatalf("MarkPrestagePending: %v", err)
	}
	if found, err := cat.CompletePrestage("req-1"); err != nil || !found {
		t.Fatalf("CompletePrestage: found=%v err=%v", found, err)
	}
	// Small but monitored -> NOT an orphan (operator-protected).
	if err := cat.UpsertCacheKey("steam", "999", "Watched", "steam/depot/9991/chunk/a", 100*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (monitored): %v", err)
	}
	if err := cat.SetMonitored("steam", "999", true); err != nil {
		t.Fatalf("SetMonitored: %v", err)
	}
	// Small, unprotected, identified -> orphan.
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/manifest", 92, now); err != nil {
		t.Fatalf("UpsertCacheKey (sliver orphan): %v", err)
	}
	// Small, unprotected, depot bucket, two keys -> orphan with both keys.
	if err := cat.UpsertCacheKey("steam", "depot:6000", "Steam Depot 6000", "steam/depot/6000/chunk/a", 2*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (depot orphan a): %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "depot:6000", "Steam Depot 6000", "steam/depot/6000/chunk/b", 2*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (depot orphan b): %v", err)
	}

	orphans, err := cat.OrphanGames(cutoff)
	if err != nil {
		t.Fatalf("OrphanGames: %v", err)
	}

	byKey := map[string]OrphanGroup{}
	for _, o := range orphans {
		byKey[o.GameKey] = o
	}
	if len(orphans) != 2 || byKey["730"].GameKey == "" || byKey["depot:6000"].GameKey == "" {
		t.Fatalf("expected exactly the two unprotected sub-cutoff entries {730, depot:6000}, got %+v", orphans)
	}
	if byKey["730"].SizeBytes != 92 || len(byKey["730"].CacheKeys) != 1 {
		t.Fatalf("orphan 730 should carry its 92 B size and 1 key, got %+v", byKey["730"])
	}
	if byKey["depot:6000"].SizeBytes != 4*1024*1024 || len(byKey["depot:6000"].CacheKeys) != 2 {
		t.Fatalf("orphan depot:6000 should aggregate 4 MiB over 2 keys, got %+v", byKey["depot:6000"])
	}
}

func TestBandwidth_AddTotalsRangeAndReset(t *testing.T) {
	cat := openTestCatalog(t)

	// Same-day adds are additive; HIT is the "saved" subset of total.
	if err := cat.AddBandwidth("2026-06-26", 1000, 4, 700, 3); err != nil {
		t.Fatalf("AddBandwidth d1: %v", err)
	}
	if err := cat.AddBandwidth("2026-06-26", 200, 1, 0, 0); err != nil {
		t.Fatalf("AddBandwidth d1b: %v", err)
	}
	if err := cat.AddBandwidth("2026-06-27", 500, 2, 500, 2); err != nil {
		t.Fatalf("AddBandwidth d2: %v", err)
	}

	tb, hb, tr, hr, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tb != 1700 || hb != 1200 || tr != 7 || hr != 5 {
		t.Fatalf("lifetime = %d/%d/%d/%d, want 1700/1200/7/5", tb, hb, tr, hr)
	}

	// Date range: only the later day.
	tb, hb, _, _, _ = cat.BandwidthTotals("2026-06-27")
	if tb != 500 || hb != 500 {
		t.Fatalf("ranged = %d/%d, want 500/500", tb, hb)
	}

	// "since" is stamped to the first day and not advanced by later adds.
	if since, _ := cat.BandwidthSince(); since != "2026-06-26" {
		t.Fatalf("BandwidthSince = %q, want 2026-06-26", since)
	}

	// Reset zeroes the tally and advances "since".
	if err := cat.ResetBandwidth(); err != nil {
		t.Fatalf("ResetBandwidth: %v", err)
	}
	if tb, _, tr, _, _ := cat.BandwidthTotals(""); tb != 0 || tr != 0 {
		t.Fatalf("after reset = %d/%d, want 0/0", tb, tr)
	}
	if since, _ := cat.BandwidthSince(); since == "2026-06-26" || since == "" {
		t.Fatalf("reset should advance bandwidth_since, got %q", since)
	}
}

func TestPrefillQueue_EnqueueDedupOrderAndLifecycle(t *testing.T) {
	cat := openTestCatalog(t)

	add := func(appID, name string) bool {
		ok, err := cat.EnqueuePrefill("steam", appID, name)
		if err != nil {
			t.Fatalf("EnqueuePrefill %s: %v", appID, err)
		}
		return ok
	}
	if !add("100", "Alpha") || !add("200", "Beta") || !add("300", "Gamma") {
		t.Fatalf("expected the first enqueue of each game to add it")
	}
	if add("200", "Beta again") {
		t.Fatalf("expected duplicate enqueue to be a no-op (UNIQUE dedup)")
	}

	list, err := cat.ListPrefillQueue()
	if err != nil {
		t.Fatalf("ListPrefillQueue: %v", err)
	}
	if len(list) != 3 || list[0].AppID != "100" || list[2].AppID != "300" {
		t.Fatalf("expected FIFO order 100,200,300, got %+v", list)
	}

	// Head is the lowest-position queued item.
	head, ok, err := cat.NextQueuedPrefill()
	if err != nil || !ok || head.AppID != "100" {
		t.Fatalf("NextQueuedPrefill = %+v ok=%v err=%v, want app 100", head, ok, err)
	}

	// Mark it active -> it leaves the queued head, becomes the active item.
	if err := cat.MarkPrefillActive(head.ID, "req-1"); err != nil {
		t.Fatalf("MarkPrefillActive: %v", err)
	}
	if act, ok, _ := cat.ActivePrefillItem(); !ok || act.AppID != "100" || act.RequestID != "req-1" {
		t.Fatalf("ActivePrefillItem = %+v ok=%v, want app 100 req-1", act, ok)
	}
	if next, ok, _ := cat.NextQueuedPrefill(); !ok || next.AppID != "200" {
		t.Fatalf("next queued after activating head should be 200, got %+v ok=%v", next, ok)
	}

	// Complete the active item (delete); a failed item is retained.
	if err := cat.DeletePrefillItem(head.ID); err != nil {
		t.Fatalf("DeletePrefillItem: %v", err)
	}
	beta, _, _ := cat.NextQueuedPrefill()
	if err := cat.MarkPrefillFailed(beta.ID, "could not download"); err != nil {
		t.Fatalf("MarkPrefillFailed: %v", err)
	}
	list, _ = cat.ListPrefillQueue()
	if len(list) != 2 { // 200 (failed, retained) + 300 (queued)
		t.Fatalf("expected 2 items after one delete + one failure, got %+v", list)
	}
	// A failed item is NOT the queued head; 300 is.
	if next, ok, _ := cat.NextQueuedPrefill(); !ok || next.AppID != "300" {
		t.Fatalf("failed item must not be the queued head; want 300, got %+v ok=%v", next, ok)
	}
	// Retry the failed one.
	if err := cat.RequeuePrefill(beta.ID); err != nil {
		t.Fatalf("RequeuePrefill: %v", err)
	}
	if next, _, _ := cat.NextQueuedPrefill(); next.AppID != "200" {
		t.Fatalf("after requeue, 200 (lower position) should be the head, got %s", next.AppID)
	}
}

func TestPrefillQueue_Reorder(t *testing.T) {
	cat := openTestCatalog(t)
	for _, a := range []string{"100", "200", "300"} {
		if _, err := cat.EnqueuePrefill("steam", a, "g"+a); err != nil {
			t.Fatalf("enqueue %s: %v", a, err)
		}
	}
	list, _ := cat.ListPrefillQueue()
	mid := list[1] // app 200, middle

	if err := cat.MovePrefillItem(mid.ID, true); err != nil { // move up
		t.Fatalf("MovePrefillItem up: %v", err)
	}
	list, _ = cat.ListPrefillQueue()
	if list[0].AppID != "200" || list[1].AppID != "100" {
		t.Fatalf("expected 200 moved ahead of 100, got %+v", list)
	}

	// Moving the head up is a no-op (already at the edge).
	if err := cat.MovePrefillItem(list[0].ID, true); err != nil {
		t.Fatalf("MovePrefillItem edge: %v", err)
	}
	list, _ = cat.ListPrefillQueue()
	if list[0].AppID != "200" {
		t.Fatalf("moving the head up should be a no-op, got %+v", list)
	}
}

// TestGetGame_ReturnsLastSeen covers the game-detail "Last served" stat (U13):
// GetGame must surface MAX(last_seen_at) over the game's cache keys, which the
// List query already did but GetGame previously dropped.
func TestGetGame_ReturnsLastSeen(t *testing.T) {
	cat := openTestCatalog(t)
	seen := time.Now().UTC().Truncate(time.Second)
	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1<<20, seen); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	e, found, err := cat.GetGame("steam", "730")
	if err != nil || !found {
		t.Fatalf("GetGame: found=%v err=%v", found, err)
	}
	if !e.LastSeen.Equal(seen) {
		t.Fatalf("LastSeen: got %v, want %v", e.LastSeen, seen)
	}
}

// TestLockedBytes_SumsOnlyMonitoredLibraryGames covers the capacity gauge's
// "Locked" band: LockedBytes must total the cached size of monitored Library
// games only -- not unlocked ones, and not monitored content that falls below
// the Library floor (which the Library itself would not show as a game).
func TestLockedBytes_SumsOnlyMonitoredLibraryGames(t *testing.T) {
	cat := openTestCatalog(t)
	now := time.Now()
	big := int64(6 * 1024 * 1024 * 1024) // above the 5 GiB Library floor

	if err := cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", big, now); err != nil {
		t.Fatalf("UpsertCacheKey 730: %v", err)
	}
	if err := cat.UpsertCacheKey("steam", "570", "Dota 2", "steam/depot/571/chunk/a", big, now); err != nil {
		t.Fatalf("UpsertCacheKey 570: %v", err)
	}
	// A monitored game below the floor and not pre-staged is not a Library game,
	// so it must not count toward locked bytes.
	small := int64(100 * 1024 * 1024)
	if err := cat.UpsertCacheKey("steam", "1840", "Oil Rush", "steam/depot/1841/chunk/a", small, now); err != nil {
		t.Fatalf("UpsertCacheKey 1840: %v", err)
	}
	if err := cat.SetMonitored("steam", "1840", true); err != nil {
		t.Fatalf("SetMonitored 1840: %v", err)
	}

	// Nothing locked among the Library games yet.
	if got, err := cat.LockedBytes(MinLibrarySizeBytes); err != nil {
		t.Fatalf("LockedBytes: %v", err)
	} else if got != 0 {
		t.Fatalf("no monitored Library games: got %d, want 0", got)
	}

	if err := cat.SetMonitored("steam", "730", true); err != nil {
		t.Fatalf("SetMonitored 730: %v", err)
	}
	if got, err := cat.LockedBytes(MinLibrarySizeBytes); err != nil {
		t.Fatalf("LockedBytes: %v", err)
	} else if got != big {
		t.Fatalf("one monitored Library game locked: got %d, want %d", got, big)
	}
}
