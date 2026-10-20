// Package catalog persists the cache's per-game library (R1/R2) to
// embedded SQLite via a pure-Go driver (KTD5: no cgo, so the backend stays
// a single static binary).
package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS games (
	platform   TEXT NOT NULL,
	game_key   TEXT NOT NULL,
	name       TEXT NOT NULL,
	monitored  INTEGER NOT NULL DEFAULT 0,
	pre_staged INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (platform, game_key)
);

-- pending_prestage tracks an in-flight operator-initiated Pre-Stage: the
-- backend records (request_id -> the game it asked the prefill sidecar to
-- download) the moment it triggers a run, and resolves it when the run's
-- terminal status arrives. On success the game's pre_staged flag is set so
-- it stays in the Library even below the size floor (it was explicitly
-- staged, not incidentally cached); on failure the pending row is just
-- dropped. Keyed by request_id so a backend restart can reconcile against
-- the sidecar's last-written status.json.
CREATE TABLE IF NOT EXISTS pending_prestage (
	request_id TEXT PRIMARY KEY,
	platform   TEXT NOT NULL,
	game_key   TEXT NOT NULL,
	name       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS cache_keys (
	cache_key    TEXT PRIMARY KEY,
	platform     TEXT NOT NULL,
	game_key     TEXT NOT NULL,
	size_bytes   INTEGER NOT NULL DEFAULT 0,
	last_seen_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cache_keys_game ON cache_keys (platform, game_key);

CREATE TABLE IF NOT EXISTS tailer_state (
	path   TEXT PRIMARY KEY,
	inode  INTEGER NOT NULL,
	offset INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cdn_platforms (
	name    TEXT PRIMARY KEY,
	enabled INTEGER NOT NULL DEFAULT 1
);

-- Generic settings store, including the operator-entered Steam Web API
-- key and SteamID64 from the first-run setup form. Storing the API key
-- here is a deliberate, user-approved reversal of this project's original
-- plan, which kept secrets out of the catalog DB entirely (env/secret-file
-- only) -- chosen so the webui can be fully configured without ever
-- editing .env. Never logged (see internal/web's setup handlers).
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- Per-day served-bytes tally (FEATURES #1): the bandwidth-saved / hit-rate
-- metric. Independent of cache_keys (which tracks unique bytes on disk and
-- overwrites size_bytes); this is an additive event counter, so it can report
-- "bytes served from cache" and a hit-rate that cache_keys structurally cannot.
-- total_* counts every served line, hit_* the subset nginx served from cache
-- ($upstream_cache_status = HIT). Daily buckets so lifetime (SUM all) and a
-- date range are both queryable.
CREATE TABLE IF NOT EXISTS bandwidth_daily (
	day            TEXT PRIMARY KEY,
	total_bytes    INTEGER NOT NULL DEFAULT 0,
	total_requests INTEGER NOT NULL DEFAULT 0,
	hit_bytes      INTEGER NOT NULL DEFAULT 0,
	hit_requests   INTEGER NOT NULL DEFAULT 0
);

-- prefill_queue is the batch-preload playlist (FEATURES #2): an ordered,
-- DB-backed waiting list the single-flight prefill worker drains one game at a
-- time, so it survives a backend restart. status is queued | active | failed;
-- the single 'active' row (if any) is what was handed to the sidecar, and is
-- how a restart knows which item was in flight. UNIQUE(platform, app_id) dedups
-- a game to at most one queued entry. pending_prestage stays the per-run tracker
-- the worker reuses; this is the layer in front of it.
CREATE TABLE IF NOT EXISTS prefill_queue (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	position    INTEGER NOT NULL,
	platform    TEXT NOT NULL,
	app_id      TEXT NOT NULL,
	name        TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'queued',
	request_id  TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	enqueued_at TEXT NOT NULL,
	UNIQUE(platform, app_id)
);
`

type Catalog struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite catalog at path and ensures
// its schema exists.
func Open(path string) (*Catalog, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening catalog %s: %w", path, err)
	}
	// Single connection prevents inter-goroutine lock contention; WAL mode
	// allows concurrent reads alongside the single writer; busy_timeout turns
	// SQLITE_BUSY into a retry rather than an immediate error.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging catalog %s: %w", path, err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	// Migrate catalogs created before pre_staged existed. The schema CREATE
	// above is IF NOT EXISTS, so it never adds the column to a pre-existing
	// games table -- only this ALTER does, exactly once. ADD COLUMN with a
	// constant DEFAULT is an O(1) metadata-only operation in SQLite (it does
	// not rewrite existing rows), so this is safe to run on a populated prod
	// DB on the deploy that introduces it.
	if err := ensureColumn(db, "games", "pre_staged", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating games.pre_staged: %w", err)
	}
	return &Catalog{db: db}, nil
}

// ensureColumn adds column to table with the given type/constraints if it is
// not already present, so a freshly-created DB (which got the column from the
// schema CREATE) and an upgraded older DB (which gets it from the ALTER) both
// converge on the same shape.
func ensureColumn(db *sql.DB, table, column, definition string) error {
	exists, err := columnExists(db, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// table/column/definition are package-internal literals, never user
	// input, so the unparameterized DDL here is not an injection surface
	// (PRAGMA/ALTER also can't bind identifiers).
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		// PRAGMA table_info columns: cid, name, type, notnull, dflt_value, pk.
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (c *Catalog) Close() error {
	return c.db.Close()
}

// UpsertCacheKey records one identified cache entry (KTD5: keyed by
// (platform, game_key) for the owning game, and cache_key for the
// individual entry). gameName is written on the game row every call so a
// later, better-informed identification (e.g. Steam resolving a
// previously-Unidentified depot) overwrites the earlier placeholder name.
func (c *Catalog) UpsertCacheKey(platform, gameKey, gameName, cacheKey string, sizeBytes int64, seenAt time.Time) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO games (platform, game_key, name) VALUES (?, ?, ?)
		 ON CONFLICT(platform, game_key) DO UPDATE SET name = excluded.name`,
		platform, gameKey, gameName,
	); err != nil {
		return fmt.Errorf("upserting game: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO cache_keys (cache_key, platform, game_key, size_bytes, last_seen_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(cache_key) DO UPDATE SET
			platform = excluded.platform,
			game_key = excluded.game_key,
			size_bytes = excluded.size_bytes,
			last_seen_at = excluded.last_seen_at`,
		cacheKey, platform, gameKey, sizeBytes, seenAt.UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("upserting cache key: %w", err)
	}

	return tx.Commit()
}

// MinLibrarySizeBytes is the minimum aggregated on-disk size a game must
// have to count as "actually downloaded" and appear in the Library (and in
// the prefill page's cached-game count). The Steam client constantly pulls
// tiny metadata/manifest objects through the cache for games the operator
// never downloaded -- a few bytes to a few KB each -- and recording every
// cache key indiscriminately (ingest.go has no per-request size floor) made
// the Library list dozens of games the operator never touched. 50 MiB sat
// above metadata chatter but still let small incidentally-cached games (a
// few hundred MiB the operator never deliberately downloaded) leak into the
// Library as if they were owned downloads. The floor is now 5 GiB: a real,
// intentionally-downloaded game clears it, while stray sub-game downloads do
// not. Games the operator explicitly Pre-Staged are exempt from the floor
// via the pre_staged flag (see List), so a small-but-deliberately-staged
// title still appears. Adjust here if the cutoff needs tuning; it is
// deliberately a single constant rather than a setting to avoid a config
// surface for a value that rarely changes.
const MinLibrarySizeBytes int64 = 5 * 1024 * 1024 * 1024

// MinGuestSizeBytes is the floor for the Guest cache view. The Steam client
// pulls tiny manifest/metadata objects (tens to hundreds of bytes) through
// the cache for depots it never fully downloads; recording each one made the
// Guest list show entries like "Counter-Strike 2 -- 92 B" that are pure
// noise. 1 MiB drops that chatter while keeping any guest download large
// enough to be worth an operator's attention to purge.
const MinGuestSizeBytes int64 = 1 * 1024 * 1024

// EvictRef identifies a cached entry eligible for cache-size eviction, with
// its aggregated on-disk size. Used by the cache-max enforcer to reclaim space.
type EvictRef struct {
	Platform  string
	GameKey   string
	SizeBytes int64
}

// EvictionCandidates lists every NON-locked cached entry (both identified games
// and guest content) ordered least-recently-accessed first, so the cache-size
// enforcer can evict the coldest unprotected content first. Locked (monitored)
// entries are excluded -- they are never auto-evicted ("Lock wins"). LRU order
// comes from MAX(last_seen_at) per entry, which the access-log tailer updates
// on every request; last_seen_at is stored UTC RFC3339, so a TEXT sort is
// chronological.
func (c *Catalog) EvictionCandidates() ([]EvictRef, error) {
	rows, err := c.db.Query(`
		SELECT g.platform, g.game_key, COALESCE(SUM(ck.size_bytes), 0)
		FROM games g
		JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
		WHERE g.monitored = 0
		GROUP BY g.platform, g.game_key
		ORDER BY MAX(ck.last_seen_at) ASC`)
	if err != nil {
		return nil, fmt.Errorf("listing eviction candidates: %w", err)
	}
	defer rows.Close()

	var refs []EvictRef
	for rows.Next() {
		var r EvictRef
		if err := rows.Scan(&r.Platform, &r.GameKey, &r.SizeBytes); err != nil {
			return nil, fmt.Errorf("scanning eviction candidate: %w", err)
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// LibraryEntry is one browsable row (R1/R2): a game with its aggregated
// on-disk size across every cache key recorded under it.
type LibraryEntry struct {
	Platform      string
	GameKey       string
	Name          string
	Monitored     bool
	SizeBytes     int64
	CacheKeyCount int
	LastSeen      time.Time
}

// List returns the operator's owned, identified games whose aggregated cached
// size is at least minSizeBytes, ordered by name (R1: games "present in the
// cache"). Callers pass MinLibrarySizeBytes for the operator-facing Library
// (hiding metadata chatter); 0 enumerates every owned-game entry that has any
// cached bytes. Three shapes are excluded:
//
//   - Guest content -- per-depot entries ("depot:NNNNN") and the
//     "host:unidentified" catch-all -- belongs to GuestGames(), not the
//     Library; the WHERE clause is the exact complement of GuestGames() so
//     the two partition the catalog rather than double-listing it. (Without
//     this, large guest depots and unidentified host buckets leaked into the
//     Library as fake games.)
//   - A game row with zero cache keys is a leftover bucket -- the INNER JOIN
//     drops it.
//   - When a floor is given, a game whose cached bytes are below it -- a few
//     hundred MiB the Steam client incidentally pulled for a game the
//     operator never deliberately downloaded -- the HAVING clause drops it,
//     UNLESS the operator explicitly Pre-Staged it (pre_staged = 1), in which
//     case it stays regardless of size: a deliberately-staged game belongs in
//     the Library even if it's small. (pre_staged is constant per group, so
//     MAX() over the group just reads that constant; it is never SELECTed.)
//
// The size floor is a display policy of the caller, not a storage rule, so it
// is a parameter here rather than baked in: the ingester still records every
// cache key for accurate byte accounting.
func (c *Catalog) List(minSizeBytes int64) ([]LibraryEntry, error) {
	rows, err := c.db.Query(`
		SELECT g.platform, g.game_key, g.name, g.monitored,
		       COALESCE(SUM(ck.size_bytes), 0), COUNT(ck.cache_key),
		       COALESCE(MAX(ck.last_seen_at), '')
		FROM games g
		JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
		WHERE g.game_key NOT LIKE 'depot:%'
		  AND g.game_key NOT LIKE '%:unidentified'
		GROUP BY g.platform, g.game_key
		HAVING MAX(g.pre_staged) = 1 OR SUM(ck.size_bytes) >= ?
		ORDER BY g.name`, minSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("listing library entries: %w", err)
	}
	defer rows.Close()

	var entries []LibraryEntry
	for rows.Next() {
		var e LibraryEntry
		var lastSeen string
		if err := rows.Scan(&e.Platform, &e.GameKey, &e.Name, &e.Monitored, &e.SizeBytes, &e.CacheKeyCount, &lastSeen); err != nil {
			return nil, fmt.Errorf("scanning library entry: %w", err)
		}
		e.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// TotalCachedBytes returns the sum of all size_bytes across cache_keys.
func (c *Catalog) TotalCachedBytes() (int64, error) {
	var total sql.NullInt64
	err := c.db.QueryRow(`SELECT SUM(size_bytes) FROM cache_keys`).Scan(&total)
	return total.Int64, err
}

// AddBandwidth folds one ingest batch's served-byte totals into the per-day
// tally (additive, unlike cache_keys' size_bytes overwrite). day is a UTC date
// ("2006-01-02"). It also stamps bandwidth_since on first use so the dashboard's
// "since" label is well-defined.
func (c *Catalog) AddBandwidth(day string, totalBytes, totalReqs, hitBytes, hitReqs int64) error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("bandwidth tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO bandwidth_daily (day, total_bytes, total_requests, hit_bytes, hit_requests)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(day) DO UPDATE SET
		   total_bytes    = total_bytes + excluded.total_bytes,
		   total_requests = total_requests + excluded.total_requests,
		   hit_bytes      = hit_bytes + excluded.hit_bytes,
		   hit_requests   = hit_requests + excluded.hit_requests`,
		day, totalBytes, totalReqs, hitBytes, hitReqs,
	); err != nil {
		return fmt.Errorf("adding bandwidth for %s: %w", day, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value) VALUES ('bandwidth_since', ?) ON CONFLICT(key) DO NOTHING`,
		day,
	); err != nil {
		return fmt.Errorf("stamping bandwidth_since: %w", err)
	}
	return tx.Commit()
}

// BandwidthTotals sums the tally from sinceDay (inclusive); pass "" for lifetime.
// Returns served bytes, of-which-from-cache (HIT) bytes, total requests, and HIT
// requests.
func (c *Catalog) BandwidthTotals(sinceDay string) (totalBytes, hitBytes, totalReqs, hitReqs int64, err error) {
	var tb, hb, tr, hr sql.NullInt64
	q := `SELECT SUM(total_bytes), SUM(hit_bytes), SUM(total_requests), SUM(hit_requests) FROM bandwidth_daily`
	var args []any
	if sinceDay != "" {
		q += ` WHERE day >= ?`
		args = append(args, sinceDay)
	}
	err = c.db.QueryRow(q, args...).Scan(&tb, &hb, &tr, &hr)
	return tb.Int64, hb.Int64, tr.Int64, hr.Int64, err
}

// BandwidthSince returns the UTC date the current tally started ("" if none yet).
func (c *Catalog) BandwidthSince() (string, error) {
	v, _, err := c.GetSetting("bandwidth_since")
	return v, err
}

// IsBandwidthEmpty reports whether bandwidth_daily has no rows.
// Cheaper and semantically clearer than summing data columns to check for absence.
func (c *Catalog) IsBandwidthEmpty() (bool, error) {
	var exists bool
	err := c.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM bandwidth_daily)`).Scan(&exists)
	return !exists, err
}

// ResetBandwidth clears the tally and restarts the "since" clock at today (UTC).
func (c *Catalog) ResetBandwidth() error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("reset bandwidth tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM bandwidth_daily`); err != nil {
		return fmt.Errorf("clearing bandwidth: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value) VALUES ('bandwidth_since', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		time.Now().UTC().Format("2006-01-02"),
	); err != nil {
		return fmt.Errorf("resetting bandwidth_since: %w", err)
	}
	return tx.Commit()
}

// writeBandwidthBackfill writes all per-day totals and sets bandwidth_since
// to earliest in a single atomic transaction. Called exclusively by
// BackfillBandwidth, which guarantees the table is empty before calling.
func (c *Catalog) writeBandwidthBackfill(bw map[string]*bwDay, earliest string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("backfill tx: %w", err)
	}
	defer tx.Rollback()
	for day, d := range bw {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO bandwidth_daily (day, total_bytes, total_requests, hit_bytes, hit_requests)
			 VALUES (?, ?, ?, ?, ?)`,
			day, d.totalBytes, d.totalReqs, d.hitBytes, d.hitReqs,
		); err != nil {
			return fmt.Errorf("backfill: recording bandwidth for %s: %w", day, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO settings (key, value) VALUES ('bandwidth_since', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		earliest,
	); err != nil {
		return fmt.Errorf("backfill: setting bandwidth_since: %w", err)
	}
	return tx.Commit()
}

// QueueItem is one entry in the batch-preload playlist (prefill_queue).
type QueueItem struct {
	ID        int64
	Position  int
	Platform  string
	AppID     string // game_key (bare appID for Steam)
	Name      string
	Status    string // queued | active | failed
	RequestID string
	Error     string
}

// EnqueuePrefill appends a game to the prefill queue at the end. added=false if
// the game is already present (UNIQUE(platform, app_id) dedup), so re-queuing is
// idempotent.
func (c *Catalog) EnqueuePrefill(platform, appID, name string) (added bool, err error) {
	res, err := c.db.Exec(
		`INSERT INTO prefill_queue (position, platform, app_id, name, enqueued_at)
		 VALUES ((SELECT COALESCE(MAX(position), 0) + 1 FROM prefill_queue), ?, ?, ?, ?)
		 ON CONFLICT(platform, app_id) DO NOTHING`,
		platform, appID, name, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("enqueueing prefill (%s, %s): %w", platform, appID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListPrefillQueue returns every queue entry, ordered by position.
func (c *Catalog) ListPrefillQueue() ([]QueueItem, error) {
	rows, err := c.db.Query(
		`SELECT id, position, platform, app_id, name, status, request_id, error
		 FROM prefill_queue ORDER BY position`)
	if err != nil {
		return nil, fmt.Errorf("listing prefill queue: %w", err)
	}
	defer rows.Close()
	return scanQueueItems(rows)
}

// NextQueuedPrefill returns the head still waiting (status queued, lowest
// position). found=false when nothing is waiting.
func (c *Catalog) NextQueuedPrefill() (QueueItem, bool, error) {
	return c.queueItemRow(`SELECT id, position, platform, app_id, name, status, request_id, error
		FROM prefill_queue WHERE status = 'queued' ORDER BY position LIMIT 1`)
}

// ActivePrefillItem returns the row currently handed to the sidecar (status
// active); the worker reconciles this on restart. found=false when none.
func (c *Catalog) ActivePrefillItem() (QueueItem, bool, error) {
	return c.queueItemRow(`SELECT id, position, platform, app_id, name, status, request_id, error
		FROM prefill_queue WHERE status = 'active' ORDER BY position LIMIT 1`)
}

func (c *Catalog) queueItemRow(query string, args ...any) (QueueItem, bool, error) {
	rows, err := c.db.Query(query, args...)
	if err != nil {
		return QueueItem{}, false, fmt.Errorf("querying prefill queue: %w", err)
	}
	defer rows.Close()
	items, err := scanQueueItems(rows)
	if err != nil || len(items) == 0 {
		return QueueItem{}, false, err
	}
	return items[0], true, nil
}

func scanQueueItems(rows *sql.Rows) ([]QueueItem, error) {
	var out []QueueItem
	for rows.Next() {
		var q QueueItem
		if err := rows.Scan(&q.ID, &q.Position, &q.Platform, &q.AppID, &q.Name, &q.Status, &q.RequestID, &q.Error); err != nil {
			return nil, fmt.Errorf("scanning prefill queue row: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// MarkPrefillActive marks item id as the in-flight run with its sidecar request
// ID (clearing any prior error).
func (c *Catalog) MarkPrefillActive(id int64, requestID string) error {
	if _, err := c.db.Exec(
		`UPDATE prefill_queue SET status = 'active', request_id = ?, error = '' WHERE id = ?`,
		requestID, id); err != nil {
		return fmt.Errorf("marking prefill %d active: %w", id, err)
	}
	return nil
}

// MarkPrefillFailed marks item id failed with a reason, retaining it so the
// operator can see why and retry.
func (c *Catalog) MarkPrefillFailed(id int64, reason string) error {
	if _, err := c.db.Exec(
		`UPDATE prefill_queue SET status = 'failed', request_id = '', error = ? WHERE id = ?`,
		reason, id); err != nil {
		return fmt.Errorf("marking prefill %d failed: %w", id, err)
	}
	return nil
}

// RequeuePrefill resets item id back to queued (operator retry, or restart
// recovery of a stale active row).
func (c *Catalog) RequeuePrefill(id int64) error {
	if _, err := c.db.Exec(
		`UPDATE prefill_queue SET status = 'queued', request_id = '', error = '' WHERE id = ?`, id); err != nil {
		return fmt.Errorf("requeueing prefill %d: %w", id, err)
	}
	return nil
}

// DeletePrefillItem removes item id (completed, cancelled, or dismissed).
func (c *Catalog) DeletePrefillItem(id int64) error {
	if _, err := c.db.Exec(`DELETE FROM prefill_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting prefill %d: %w", id, err)
	}
	return nil
}

// MovePrefillItem swaps item id's position with its adjacent neighbour (up =
// earlier). A no-op at the edge. The cmp/order tokens are fixed internal
// constants (never user input), so the string build is injection-safe.
func (c *Catalog) MovePrefillItem(id int64, up bool) error {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("reorder tx: %w", err)
	}
	defer tx.Rollback()

	var pos int
	if err := tx.QueryRow(`SELECT position FROM prefill_queue WHERE id = ?`, id).Scan(&pos); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("reorder: reading position: %w", err)
	}
	cmp, order := "<", "DESC"
	if !up {
		cmp, order = ">", "ASC"
	}
	var nbrID int64
	var nbrPos int
	err = tx.QueryRow(
		`SELECT id, position FROM prefill_queue WHERE position `+cmp+` ? ORDER BY position `+order+` LIMIT 1`, pos,
	).Scan(&nbrID, &nbrPos)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reorder: finding neighbour: %w", err)
	}
	if _, err := tx.Exec(`UPDATE prefill_queue SET position = ? WHERE id = ?`, nbrPos, id); err != nil {
		return fmt.Errorf("reorder: moving item: %w", err)
	}
	if _, err := tx.Exec(`UPDATE prefill_queue SET position = ? WHERE id = ?`, pos, nbrID); err != nil {
		return fmt.Errorf("reorder: moving neighbour: %w", err)
	}
	return tx.Commit()
}

// CachedGameCount returns the number of owned games (excluding guest depot
// entries and unidentified buckets) the Library actually shows. It must apply
// the same three rules as List(minSizeBytes) -- the depot:/unidentified
// exclusion, the size floor, and the pre_staged exemption -- so this count
// equals the visible row count. The caller MUST pass the same floor it passes
// to List (the configured min game size); a divergent floor would make the
// prefill page's "N games cached" disagree with the visible list and read as a
// fresh bug. That means it has to JOIN games (the pre_staged flag lives there,
// not on cache_keys); a cache_keys-only count could not see pre_staged and
// would disagree with the list.
func (c *Catalog) CachedGameCount(minSizeBytes int64) (int, error) {
	var n int
	err := c.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM games g
			JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
			WHERE g.game_key NOT LIKE 'depot:%'
			  AND g.game_key NOT LIKE '%:unidentified'
			GROUP BY g.platform, g.game_key
			HAVING MAX(g.pre_staged) = 1 OR SUM(ck.size_bytes) >= ?
		)`, minSizeBytes).Scan(&n)
	return n, err
}

// LockedBytes returns the total cached size of Library games the operator has
// locked (monitored = protected from eviction). It applies the same Library
// membership rules as CachedGameCount/List (depot/unidentified exclusion, size
// floor, pre_staged exemption) so "locked" here means the same set the Library
// shows as locked. Used by the Dashboard/Preload capacity gauge to draw the
// protected (never-evictable) portion of the cache. Pass the same floor as List.
func (c *Catalog) LockedBytes(minSizeBytes int64) (int64, error) {
	var n int64
	err := c.db.QueryRow(`
		SELECT COALESCE(SUM(sz), 0) FROM (
			SELECT SUM(ck.size_bytes) AS sz
			FROM games g
			JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
			WHERE g.game_key NOT LIKE 'depot:%'
			  AND g.game_key NOT LIKE '%:unidentified'
			GROUP BY g.platform, g.game_key
			HAVING (MAX(g.pre_staged) = 1 OR SUM(ck.size_bytes) >= ?)
			   AND MAX(g.monitored) = 1
		)`, minSizeBytes).Scan(&n)
	return n, err
}

// GuestGames returns the Guest cache view: everything cached above the
// MinGuestSizeBytes floor that the Library does NOT show. It is defined as the
// exact complement of List(minLibrarySizeBytes) so the two together partition
// the catalog -- no row is double-listed and no row above the guest floor is
// lost from both views. The caller MUST pass the same floor it passes to List
// (the configured min game size) or the two views would overlap or leak.
// Concretely a row appears here when it has >= MinGuestSizeBytes cached AND it
// is not a Library game, where "Library game" means: identified (not a
// "depot:NNNNN" per-depot entry and not the "platform:unidentified" catch-all)
// AND (explicitly Pre-Staged OR at least minLibrarySizeBytes on disk). So:
//   - depot:/unidentified guest content >= 1 MiB lands here (its natural home);
//   - an identified game the operator never deliberately downloaded, sitting
//     below the Library floor and not Pre-Staged, also lands here rather than
//     vanishing -- it really is incidentally-cached guest content;
//   - sub-1-MiB noise (the "Counter-Strike 2 -- 92 B" depot slivers the Steam
//     client leaves behind) is dropped by the floor and shows in neither view.
//
// (game_key/platform are in GROUP BY so they're referenceable bare in HAVING;
// pre_staged is only PK-functionally-dependent, so it's read via MAX() -- a
// constant per group -- matching List.)
func (c *Catalog) GuestGames(minLibrarySizeBytes int64) ([]LibraryEntry, error) {
	rows, err := c.db.Query(`
		SELECT g.platform, g.game_key, g.name, g.monitored,
		       COALESCE(SUM(ck.size_bytes), 0), COUNT(ck.cache_key)
		FROM games g
		JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
		GROUP BY g.platform, g.game_key
		HAVING SUM(ck.size_bytes) >= ?
		   AND NOT (
		     g.game_key NOT LIKE 'depot:%'
		     AND g.game_key NOT LIKE '%:unidentified'
		     AND (MAX(g.pre_staged) = 1 OR SUM(ck.size_bytes) >= ?)
		   )
		ORDER BY SUM(ck.size_bytes) DESC`, MinGuestSizeBytes, minLibrarySizeBytes)
	if err != nil {
		return nil, fmt.Errorf("listing guest games: %w", err)
	}
	defer rows.Close()

	var entries []LibraryEntry
	for rows.Next() {
		var e LibraryEntry
		if err := rows.Scan(&e.Platform, &e.GameKey, &e.Name, &e.Monitored, &e.SizeBytes, &e.CacheKeyCount); err != nil {
			return nil, fmt.Errorf("scanning guest entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// UpdateGameName overwrites the display name for an existing game row.
func (c *Catalog) UpdateGameName(platform, gameKey, name string) error {
	_, err := c.db.Exec(`UPDATE games SET name = ? WHERE platform = ? AND game_key = ?`, name, platform, gameKey)
	return err
}

// GetGame returns one game's current state regardless of how many cache
// keys it has (unlike List, which hides zero-key games) -- callers like
// lifecycle.Delete need to see a game that the operator's UI snapshot
// already knew about even if reconciliation or a prior delete zeroed it in
// the meantime.
func (c *Catalog) GetGame(platform, gameKey string) (LibraryEntry, bool, error) {
	var e LibraryEntry
	var lastSeen string // MAX over the game's cache keys; '' when it has none (LEFT JOIN)
	err := c.db.QueryRow(`
		SELECT g.platform, g.game_key, g.name, g.monitored,
		       COALESCE(SUM(ck.size_bytes), 0), COUNT(ck.cache_key),
		       COALESCE(MAX(ck.last_seen_at), '')
		FROM games g
		LEFT JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
		WHERE g.platform = ? AND g.game_key = ?
		GROUP BY g.platform, g.game_key`,
		platform, gameKey,
	).Scan(&e.Platform, &e.GameKey, &e.Name, &e.Monitored, &e.SizeBytes, &e.CacheKeyCount, &lastSeen)
	if err == sql.ErrNoRows {
		return LibraryEntry{}, false, nil
	}
	if err != nil {
		return LibraryEntry{}, false, fmt.Errorf("getting game (%s, %s): %w", platform, gameKey, err)
	}
	e.LastSeen, _ = time.Parse(time.RFC3339, lastSeen) // zero value when '' -- template guards with IsZero
	return e, true, nil
}

// SetMonitored flips a game's monitored flag (R4).
func (c *Catalog) SetMonitored(platform, gameKey string, monitored bool) error {
	_, err := c.db.Exec(`UPDATE games SET monitored = ? WHERE platform = ? AND game_key = ?`, monitored, platform, gameKey)
	return err
}

// CacheKeysForGame returns every distinct cache key currently recorded
// under (platform, gameKey), the input lifecycle.Delete needs to compute
// PURGE targets from.
func (c *Catalog) CacheKeysForGame(platform, gameKey string) ([]string, error) {
	rows, err := c.db.Query(`SELECT cache_key FROM cache_keys WHERE platform = ? AND game_key = ?`, platform, gameKey)
	if err != nil {
		return nil, fmt.Errorf("listing cache keys for (%s, %s): %w", platform, gameKey, err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteCacheKeysForGame removes every cache_keys row under (platform,
// gameKey) (R6/KTD11). The games row itself is left in place -- a later
// re-cache upserts against the same stable (platform, game_key), reviving
// the same logical library entry rather than creating a duplicate (R16).
func (c *Catalog) DeleteCacheKeysForGame(platform, gameKey string) error {
	_, err := c.db.Exec(`DELETE FROM cache_keys WHERE platform = ? AND game_key = ?`, platform, gameKey)
	return err
}

// DeleteCacheKeysIfUnmonitored clears (platform, gameKey)'s cache keys only
// while the game is not monitored, check and delete in one transaction, so a
// lock that lands between a caller's earlier check and this delete is honored
// rather than silently overwritten. Returns false when the game is (now)
// monitored and nothing was deleted.
func (c *Catalog) DeleteCacheKeysIfUnmonitored(platform, gameKey string) (bool, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var monitored bool
	err = tx.QueryRow(`SELECT monitored FROM games WHERE platform = ? AND game_key = ?`, platform, gameKey).Scan(&monitored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if monitored {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM cache_keys WHERE platform = ? AND game_key = ?`, platform, gameKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// OrphanGroup is one orphan game: a cached entry below the min game size that
// the operator never deliberately kept (not Pre-Staged, not monitored). It
// carries its cache keys so the caller can derive wildcard PURGE prefixes
// before clearing the catalog rows.
type OrphanGroup struct {
	Platform  string
	GameKey   string
	SizeBytes int64
	CacheKeys []string
}

// OrphanGames returns every game whose aggregated cached size is below
// maxSizeBytes and which the operator never deliberately kept -- i.e. not
// Pre-Staged and not monitored. These are the cache fragments "Purge orphans"
// sweeps: identified games the operator never really downloaded, raw per-depot
// slivers, and unidentified host buckets too small to be a real game. Big
// downloads (>= maxSizeBytes), Pre-Staged entries, and monitored entries are
// NOT orphans at any size and are left alone. The predicate (size floor +
// pre_staged/monitored exemption) deliberately mirrors the Library/Guest
// floor so "orphan" means exactly "below the min game size and not protected".
//
// Each group's cache keys are loaded after the grouping query is fully drained
// (modernc/SQLite runs with a single connection, so a second query cannot run
// while the first's rows are still open; rows.Next returning false releases
// the connection, which the per-group CacheKeysForGame calls then reuse).
func (c *Catalog) OrphanGames(maxSizeBytes int64) ([]OrphanGroup, error) {
	rows, err := c.db.Query(`
		SELECT g.platform, g.game_key, SUM(ck.size_bytes)
		FROM games g
		JOIN cache_keys ck ON ck.platform = g.platform AND ck.game_key = g.game_key
		WHERE g.pre_staged = 0 AND g.monitored = 0
		GROUP BY g.platform, g.game_key
		HAVING SUM(ck.size_bytes) < ?
		ORDER BY g.platform, g.game_key`, maxSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("listing orphan games: %w", err)
	}
	defer rows.Close()

	var groups []OrphanGroup
	for rows.Next() {
		var g OrphanGroup
		if err := rows.Scan(&g.Platform, &g.GameKey, &g.SizeBytes); err != nil {
			return nil, fmt.Errorf("scanning orphan group: %w", err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range groups {
		keys, err := c.CacheKeysForGame(groups[i].Platform, groups[i].GameKey)
		if err != nil {
			return nil, err
		}
		groups[i].CacheKeys = keys
	}
	return groups, nil
}

// MarkPrestagePending records that an operator-initiated Pre-Stage run
// (requestID) is in flight for (platform, gameKey, name). It is written the
// moment the prefill sidecar is triggered, before any bytes land, so a
// backend restart mid-run can still reconcile the result against the
// sidecar's status.json. ON CONFLICT keeps the latest mapping for a reused
// request_id (request IDs are random tokens, so a real collision never
// happens; this is just defensive).
func (c *Catalog) MarkPrestagePending(requestID, platform, gameKey, name string) error {
	_, err := c.db.Exec(
		`INSERT INTO pending_prestage (request_id, platform, game_key, name) VALUES (?, ?, ?, ?)
		 ON CONFLICT(request_id) DO UPDATE SET platform = excluded.platform, game_key = excluded.game_key, name = excluded.name`,
		requestID, platform, gameKey, name,
	)
	return err
}

// CompletePrestage resolves a successful Pre-Stage: it sets pre_staged = 1 on
// the game the run was for (creating the games row if the ingester hasn't
// caught up yet, so the flag is never lost to a race) and removes the pending
// row, atomically. It returns found = false (and changes nothing) if there is
// no pending row for requestID -- e.g. a duplicate completion, or a status
// for a request this backend never tracked. The game's name is NOT
// overwritten when the row already exists: the ingester's Steam-resolved name
// is authoritative over the pending row's snapshot.
func (c *Catalog) CompletePrestage(requestID string) (found bool, err error) {
	tx, err := c.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var platform, gameKey, name string
	err = tx.QueryRow(
		`SELECT platform, game_key, name FROM pending_prestage WHERE request_id = ?`, requestID,
	).Scan(&platform, &gameKey, &name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading pending prestage %s: %w", requestID, err)
	}

	if _, err := tx.Exec(
		`INSERT INTO games (platform, game_key, name, pre_staged) VALUES (?, ?, ?, 1)
		 ON CONFLICT(platform, game_key) DO UPDATE SET pre_staged = 1`,
		platform, gameKey, name,
	); err != nil {
		return false, fmt.Errorf("setting pre_staged for (%s, %s): %w", platform, gameKey, err)
	}
	if _, err := tx.Exec(`DELETE FROM pending_prestage WHERE request_id = ?`, requestID); err != nil {
		return false, fmt.Errorf("clearing pending prestage %s: %w", requestID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ClearPrestagePending removes a pending Pre-Stage row without setting
// pre_staged -- used when the run failed or timed out (nothing was staged, so
// the game should not be exempted from the Library size floor).
func (c *Catalog) ClearPrestagePending(requestID string) error {
	_, err := c.db.Exec(`DELETE FROM pending_prestage WHERE request_id = ?`, requestID)
	return err
}

// CacheKeySize returns the currently recorded size for cacheKey, used by
// reconcile.go to detect drift against the on-disk total.
func (c *Catalog) CacheKeySize(cacheKey string) (sizeBytes int64, found bool, err error) {
	err = c.db.QueryRow(`SELECT size_bytes FROM cache_keys WHERE cache_key = ?`, cacheKey).Scan(&sizeBytes)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return sizeBytes, true, nil
}

// SetCacheKeySize corrects an existing cache key's recorded size (R15). It
// does not create new rows -- reconciliation only corrects drift on cache
// keys the log tailer has already identified and catalogued.
func (c *Catalog) SetCacheKeySize(cacheKey string, sizeBytes int64) error {
	_, err := c.db.Exec(`UPDATE cache_keys SET size_bytes = ? WHERE cache_key = ?`, sizeBytes, cacheKey)
	return err
}

// DeleteStaleCacheKeys removes catalog entries for cache keys that are no
// longer present on disk (evicted by nginx). diskTotals is the size map built
// during a reconcile walk; only its keys matter. Returns the number of rows deleted.
//
// walkStart is the RFC3339 UTC time the reconcile walk began. Rows with
// last_seen_at >= walkStart are never pruned: a download that completed while
// the walk was in progress may have been ingested (fresh last_seen_at) but
// created its files after the walker passed, so it's legitimately absent from
// diskTotals despite being real. Excluding recently-seen rows from the
// candidate set prevents that race from silently deleting a live game.
func (c *Catalog) DeleteStaleCacheKeys(diskTotals map[string]int64, walkStart string) (int, error) {
	rows, err := c.db.Query(`SELECT cache_key FROM cache_keys WHERE last_seen_at < ?`, walkStart)
	if err != nil {
		return 0, fmt.Errorf("reading cache keys: %w", err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return 0, err
		}
		if _, found := diskTotals[key]; !found {
			stale = append(stale, key)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(stale) == 0 {
		return 0, nil
	}

	tx, err := c.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`DELETE FROM cache_keys WHERE cache_key = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, key := range stale {
		if _, err := stmt.Exec(key); err != nil {
			return 0, fmt.Errorf("deleting stale key %q: %w", key, err)
		}
	}
	return len(stale), tx.Commit()
}

// TailState is the persisted log-tailing position for one access log path
// (rotation/restart resilience, see tail.go).
type TailState struct {
	Inode  uint64
	Offset int64
}

func (c *Catalog) LoadTailState(path string) (TailState, bool, error) {
	var st TailState
	err := c.db.QueryRow(`SELECT inode, offset FROM tailer_state WHERE path = ?`, path).Scan(&st.Inode, &st.Offset)
	if err == sql.ErrNoRows {
		return TailState{}, false, nil
	}
	if err != nil {
		return TailState{}, false, err
	}
	return st, true, nil
}

func (c *Catalog) SaveTailState(path string, st TailState) error {
	_, err := c.db.Exec(
		`INSERT INTO tailer_state (path, inode, offset) VALUES (?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET inode = excluded.inode, offset = excluded.offset`,
		path, st.Inode, st.Offset,
	)
	return err
}

// SeedPlatformsIfEmpty inserts every name in allNames into cdn_platforms, but
// only if the table is currently empty (first boot). Names in defaultEnabled
// are seeded as enabled; everything else is seeded as disabled. This makes
// the persisted enabled-CDN set (R9) the actual source of truth instead of
// the plan's "backend's in-memory model" framing -- without persistence,
// every backend restart would silently re-enable every platform regardless
// of what the operator had toggled off.
func (c *Catalog) SeedPlatformsIfEmpty(allNames, defaultEnabled []string) error {
	var count int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM cdn_platforms`).Scan(&count); err != nil {
		return fmt.Errorf("checking cdn_platforms: %w", err)
	}
	if count > 0 {
		return nil
	}

	enabled := make(map[string]struct{}, len(defaultEnabled))
	for _, n := range defaultEnabled {
		enabled[n] = struct{}{}
	}

	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, name := range allNames {
		val := 0
		if _, ok := enabled[name]; ok {
			val = 1
		}
		if _, err := tx.Exec(`INSERT INTO cdn_platforms (name, enabled) VALUES (?, ?)`, name, val); err != nil {
			return fmt.Errorf("seeding platform %q: %w", name, err)
		}
	}
	return tx.Commit()
}

// EnabledPlatformNames returns every platform name currently persisted as
// enabled (R8/R9's read side).
func (c *Catalog) EnabledPlatformNames() ([]string, error) {
	rows, err := c.db.Query(`SELECT name FROM cdn_platforms WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("listing enabled platforms: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// PlatformEnabled returns whether name is currently persisted as enabled.
// found is false if name has never been seeded/toggled.
func (c *Catalog) PlatformEnabled(name string) (enabled bool, found bool, err error) {
	err = c.db.QueryRow(`SELECT enabled FROM cdn_platforms WHERE name = ?`, name).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return enabled, true, nil
}

// SetPlatformEnabled persists name's enabled flag. Callers (internal/cdn's
// toggle) must call this only after every dependent file write (dnsmasq's
// two files, nginx's allow-list) has already succeeded -- persisting
// first and then having a write fail would leave the database asserting a
// state the actual running config doesn't reflect.
func (c *Catalog) SetPlatformEnabled(name string, enabled bool) error {
	_, err := c.db.Exec(
		`INSERT INTO cdn_platforms (name, enabled) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET enabled = excluded.enabled`,
		name, enabled,
	)
	return err
}

// GetSetting returns a value from the generic settings store (e.g. the
// first-run setup form's Steam Web API key / operator SteamID64).
func (c *Catalog) GetSetting(key string) (value string, found bool, err error) {
	err = c.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("getting setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetSetting persists a value in the generic settings store.
func (c *Catalog) SetSetting(key, value string) error {
	_, err := c.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("setting %q: %w", key, err)
	}
	return nil
}
