# Buffarr Feature Roadmap

A wishlist of features, prioritized by the scenario that drives them hardest:
running a real, busy LAN party. Each is grounded in what Buffarr does today.
The top three include implementation sketches. The rest are framed but not yet
designed. Items carrying a **Reviewed** note have been triaged with an explicit
build/scope decision.

## Priority list

1. **Bandwidth saved + cache hit-rate** — the headline event metric. Buffarr's
   catalog tracks *unique bytes on disk*, not *bytes served*, so it currently
   can't show "served 4 TB off cache, pulled 600 GB from upstream." Add an
   ingest-time counter, sketched below. **Shipped 2026-06-28** — `bandwidth_daily`
   table plus a dashboard "Cache performance" card showing bytes served from cache,
   hit-rate, and since-date, and a Settings reset. Forward-only from deploy.
2. **Batch + non-owned preload queue** — queue a playlist of games to warm
   overnight, including free-to-play titles the operator doesn't own. Preload is
   one-at-a-time and owned-only today. Sketched below. **Shipped 2026-06-28,
   owned-only, deployed.** All four stages live: the catalog `prefill_queue`
   layer, a restart-safe single-driver worker where the manual Preload enqueues and
   a second Preload queues behind instead of being rejected, the queue operation
   routes for cancel, reorder, multi-add, and retry, and the grid multi-select UI,
   a "Queue selected" bar plus a queue card with per-item controls. Follow-ups still
   open: **non-owned** preload, meaning free-to-play, and partial/cruft cleanup, the
   discard-on-cancel piece, which pairs with #10's cleanup theme.
3. **Live activity feed** — who's downloading what, right now, HIT vs MISS, so a
   MISS storm on a new release is visible immediately. Sketched below.
4. **Pause-eviction toggle** — a simple **Settings toggle** that suspends the LRU
   cache evictor `RunCacheEnforcer` while it's on, so nothing gets evicted
   mid-party right before someone installs it. Deliberately just a toggle, not a
   separate "event mode": reuse the existing settings-write pattern, the
   min-game-size and dashboard-bg controls. **Shipped 2026-06-27** — a Settings
   "Cache eviction" toggle; `eviction_paused` gates `RunCacheEnforcer`.
5. **Loud health alerting** — a big dashboard banner plus an optional webhook,
   Discord or ntfy, for the silent disasters: sniproxy down, which makes preloads
   cache 0 bytes; disk near-full; cache offline. Buffarr logs these but doesn't shout.
6. **Multi-platform management** — the cache layer already handles Epic,
   Battle.net, Riot, EA, since DNS interception and the nginx cache are
   platform-agnostic for the vendored platforms, but the Library, identification,
   and Preload are Steam-first. At a school LAN it's Fortnite/Valorant/League/Overwatch/Apex too;
   even just identifying and showing those, where Battle.net resolution half-exists,
   would be a big visibility win. **Reviewed 2026-06-27: wanted, but blocked on
   testing — validating identification needs real accounts and live traffic on
   Epic/Battle.net/Riot/EA, which we don't currently have. Treat as exploratory;
   build behind real test traffic, not blind.**
7. **Capacity planner** — drop the event playlist in and get "this is 1.8 TB,
   your cache is 2 TB, here's what won't fit," using the per-game download-size
   estimates Buffarr already has, before the night instead of at 11pm.
   **Reviewed 2026-06-27: approved, good housekeeping.**
8. **Attendee kiosk / BYOD self-setup** — a read-only, no-login status page
   reading "cache online, N games ready, point your DNS here", plus the client DNS
   handout that `internal/dnsmasq` `GenerateBYODExport` already produces but
   doesn't surface. Hand it to 150 teenagers instead of walking each through DNS.
   **Shipped 2026-06-28** — a public, no-login `GET /join` with per-platform DNS
   instructions and the cache IP `BUFFARR_IP` injected. Scoped to the how-to
   page; the machine-readable `GenerateBYODExport` handout remains a future add-on.
9. **Missed → suggest preload** — Buffarr sees MISS storms in the log; surface
   "these 5 games got pulled from upstream a lot, preload them?", a natural
   extension of #3.
10. **Keep the CDN domain list fresh.** The uklans `cache-domains` list is
    vendored and `go:embed`-compiled at a pinned commit, currently ~10 months
    old, with no update mechanism, so coverage gaps accumulate over time. GOG,
    for instance, is missing entirely, so it never caches and 403s if a client is
    redirected to it. Add a one-command re-vendor, `scripts/update-cache-domains.sh`,
    that fetches `cache_domains.json` plus the referenced domain files at a given
    commit, validates them against the existing syntactic allowlist, runs
    `go test ./internal/dnsmasq/...`, and bumps the SHA in `cachedomains/SOURCE.md`.
    Keep the security model intact: still vendored, validated, and pinned at build
    time, with no runtime auto-fetch, per the untrusted-input rule KTD3. Optional
    follow-on: a CI check that flags when the pin is more than N months behind
    upstream. **Reviewed 2026-06-28: approved, low-risk housekeeping; the script
    is the real fix, and a fresh re-vendor also picks up missing platforms like GOG.**

---

## Sketch 1 — Bandwidth saved + cache hit-rate

**Problem.** In `internal/catalog/ingest.go` `ingestLine`, every access-log line
becomes an `UpsertCacheKey(...)` whose `size_bytes` is *overwritten* via
`catalog.go`'s `ON CONFLICT ... DO UPDATE SET size_bytes = excluded.size_bytes`.
The catalog tracks unique bytes on disk; the HIT history that would yield a
"bytes saved" / hit-rate is discarded at write time and cannot be reconstructed.
Fix: an additive byte counter at ingest, independent of the per-game accounting.
Do not touch `UpsertCacheKey`.

**Data model.** One new table in the `schema` const in `catalog.go`; a new table
needs no `ensureColumn`/ALTER, which is only for adding columns to existing
tables. Store totals + hit primitives, derive the rest, because
`$upstream_cache_status` is not binary, spanning HIT/MISS/EXPIRED/STALE/UPDATING/
REVALIDATED/BYPASS:

```
bandwidth_daily(
  day TEXT PRIMARY KEY,        -- UTC date
  total_bytes INTEGER, total_requests INTEGER,
  hit_bytes   INTEGER, hit_requests   INTEGER )
```

- Saved = `SUM(hit_bytes)`; from-upstream = `SUM(total_bytes) - SUM(hit_bytes)`;
  hit-rate = `SUM(hit_requests)/SUM(total_requests)`.
- "hit" = `UpstreamCacheStatus == "HIT"` only, a documented choice, since
  REVALIDATED is rare for immutable game-chunk objects. Per-day buckets subsume a
  lifetime counter, where lifetime = SUM all rows, and enable a "this weekend" date range.
- `bandwidth_since` key in the existing `settings` table for a well-defined
  "Saved since ..." label and reset.

**Increment.** In the ingest path, the single writer. After `RunOnce`'s existing
`UpstreamCacheStatus == "-"` skip, and *not* gated by identification or the size
floor, since every served byte counts. Aggregate per `Poll` batch in a
`day -> {totals}` map and flush one upsert per distinct day; the catalog is
single-connection, so avoid per-line transactions. New methods:
`AddBandwidth(day, totalBytes, totalReqs, hitBytes, hitReqs)`, an additive upsert;
`BandwidthTotals(sinceDay)`, a `SELECT SUM` with `sql.NullInt64`; and `ResetBandwidth`.

**Surfacing.** A `mergeBandwidthStats(data)` beside `mergeCacheStats` in
`server.go`, wired into `handleIndex`; new dashboard tiles, "Bandwidth saved" =
`{{humanBytes .BandwidthSaved}}` and "Cache hit-rate" = `{{.HitRatePct}}%`, reusing
the existing `.tiles` markup; arithmetic in Go, not the template. Reset via a
CSRF-protected `POST /settings/reset-bandwidth` on the Settings page.

**Caveats, carry into the doc.** Backfill on empty table, shipped as a follow-up:
on startup, if `bandwidth_daily` is empty, the backend reads the current access log
up to the tailer's persisted offset; history older than the current log file, meaning
pre-rotation, is not recoverable. At-most-once, not exactly-once: the
tailer offset is saved *before* lines are ingested, so an unclean restart may
slightly under-count; double-counting is structurally impossible, the same property
as `cache_keys`. Range requests inflate request counts but not byte totals.
Counter is append-only and never reconciled against disk.

**Tests.** Mirror `ingest_test.go`, using `openTestCatalog` and `writeLogFile`: synthetic
HIT/MISS/EXPIRED lines across two UTC dates assert per-day rows + derived
figures; a `-` line is excluded; unidentified/guest lines still count; reset
zeroes + advances `bandwidth_since`; batch aggregation = one bucket.

**Files:** `internal/catalog/catalog.go`, `internal/catalog/ingest.go`,
`internal/web/server.go`, `internal/web/templates/dashboard.html`,
`internal/catalog/ingest_test.go`.

---

## Sketch 2 — Batch + non-owned preload queue

**Scope.** Web + catalog only; the sidecar handshake, file contract, and
`internal/prefill/sidecar.go`, meaning `Trigger`/`IsRunning`/`WaitForCompletion`/
`Progress`/`Cancel`, are reused as-is. Key choice: **one game per sidecar run**
even though `Trigger` accepts many app IDs, because `ParseProgress`/`Progress`
read one `last-run.log` and emit one blended percent with one terminal pass/fail
— batching natively would collapse per-item progress/cancel/status. So the queue
feeds the sidecar one app at a time.

**Data model.** A new `prefill_queue` table, alongside rather than replacing
`pending_prestage`:

```
prefill_queue(
  id INTEGER PRIMARY KEY AUTOINCREMENT, position INTEGER,
  platform TEXT, app_id TEXT, name TEXT,
  owned INTEGER DEFAULT 1,                 -- 0 => "not in library" warning
  status TEXT DEFAULT 'queued',            -- queued | active | failed
  request_id TEXT, error TEXT, enqueued_at TEXT,
  UNIQUE(platform, app_id) )               -- dedup
```

`pending_prestage` stays the "currently handed to the sidecar" tracker, and its
restart-reconcile path already works; `prefill_queue` is the waiting list, and
its `active` row records what was running if the process died. New methods modeled
on `MarkPrestagePending`/`CompletePrestage`: `EnqueuePrefill`, which does INSERT ON
CONFLICT DO NOTHING with position = MAX+1; `ListPrefillQueue`, `NextQueuedPrefill`,
`MarkPrefillActive/Failed`, `DeletePrefillItem`, `ReorderPrefill`,
`ActivePrefillItem`.

**Worker.** A single background goroutine modeled on `nginx.PurgeQueue` but with
state in the DB, which survives restart: pop head → `Trigger([]int{appID}, owned)` →
`MarkPrefillActive` + `MarkPrestagePending`, reusing the exact existing tracking,
→ `WaitForCompletion` + `resolvePrestage`, which sets `pre_staged` on success or
clears on failure, → `DeletePrefillItem` on success or `MarkPrefillFailed` on failure,
retained and visible, → next. A failed item never wedges the queue.

**Single-driver consolidation.** Route the manual Preload button through the
queue too, meaning enqueue plus signal worker, instead of `handlePrefillTrigger` calling
`Trigger` directly. Then there's exactly one sidecar driver, the single-flight
`ErrAlreadyRunning` edge case dissolves, and "what's running" is the `active`
row. `mergePrefillStatus` reads the active row + `sidecar.Progress()`.

**Non-owned, the honest part.** SteamPrefill can only download depots the
logged-in account *owns*. Free-to-play titles such as CS2 730, Dota2 570, Apex,
and Warframe are owned by every account and preload from any login; paid non-owned
titles **fail at download** and we surface that truthfully. Move the ownership
check out of `Trigger`, where it returns `ErrNotOwned`, up to enqueue as a
*warning annotation, not a hard reject*: accept any app ID, set `owned=0` for
ones not in the owned list, and toast "free-to-play will preload; paid titles
need the sidecar's Steam account to own them." The entrypoint already detects
download failure via `grep "Unable to download..."` → `state:"failed"`, which flows
through to a `failed` queue row. Note the operational option of a shared or secondary
Steam account for the sidecar, in docs not code.

**UI, `prefill.html`.** A Queue card with ordered rows: position, poster, status
badge or live percent for the active row or failed-with-reason, plus per-row cancel
and reorder; the poster button becomes "Queue"; checkbox multi-select plus a "Queue
selected (N)" bar; a paste-app-IDs textarea as the non-owned entry point. New
routes: `POST /prefill/queue`, `/prefill/queue/{id}/cancel|up|down`.

**Edge cases.** Restart mid-queue reconciles the in-flight `pending_prestage` and
`active` row, then resumes; duplicate enqueue is handled by UNIQUE + ON CONFLICT;
single-flight is dissolved; free-vs-paid is surfaced as failure; cancel-active
releases the lock via `sidecar.Cancel()`.

**Tests.** Reuse `newTestServer`, with a temp-dir sidecar, `fetchOwnedGames` fake,
and `authedRequest`. Make the worker poll interval injectable + expose a `step()` so
table tests drive it deterministically without sleeps, simulating completion by
writing `status.json`/`last-run.log` into the test prefill dir as
`TestPrefill_StaleLockAfterRestartStillCancellable` does. Rewrite the two
ownership/concurrency tests, `TestPrefill_TriggerNonOwnedAppRejected` and
`TestPrefill_ConcurrentTriggerRejected`, to the new enqueue/queue behavior.

**Files:** `internal/catalog/catalog.go`, `internal/web/server.go`,
`internal/prefill/sidecar.go`, `internal/web/templates/prefill.html`,
`internal/web/server_test.go`.

---

## Sketch 3 — Live activity feed

**Goal.** During the party, show which client IP is pulling which game right now,
HIT vs fetch, and an approximate rate — so a MISS storm, meaning many distinct
clients fetching the same new release from upstream, is visible and one-click
preloadable. Ephemeral, in-memory; a restart starting empty is correct.

**Capture the client IP, a prerequisite.** `internal/catalog/parse.go`'s
`logLineRE` already captures `$remote_addr` as group `m[2]` and discards it. Add a
`RemoteAddr string` field to `LogLine` and assign `m[2]` in `ParseLine`. No regex
change.

**Producer→consumer seam.** Follow the `SteamLookup` pattern: define in `catalog`
a `LiveEvent{ClientIP, Platform, GameKey, GameName, CacheStatus; Bytes; Time}` and
an `EventSink interface { Record(LiveEvent) }`; add an optional nil-safe `sink`
to `Ingester`, emit from `ingestLine` after identification, so the `-` skip in
`RunOnce` has already filtered noise. **Wiring is load-bearing:** in
`cmd/backend/main.go`, `ingester.Run` starts at line ~101 *before* `web.NewServer`
at line ~120, so construct the feed *before* line 99 and inject the same instance
into both `NewIngester` as the sink and `NewServer` as `s.live` — no setter,
no race.

**Rolling structure.** A LAN is high-volume in lines but small in working set,
dozens of clients × a few games, so aggregate, never
store every line: a map keyed by `(ClientIP, GameKey)` → `{GameName, Hits,
Fetches, Bytes, FirstSeen, LastSeen}`. **Stamp times from `event.Time`, the parsed
`$time_local`, not `time.Now()`** — after downtime the tailer replays a backlog in
one pass; log-time recency filters it out for free, wall-time would render it as a
fake "now" flood. Classify `HIT` as hit, everything else non-`-` as fetch; an
EXPIRED-heavy release still counts toward a storm, and nginx slices a download, so
a mid-download game shows a HIT/MISS mix — that mixture *is* the signal. Lazy GC
entries older than a ~90–120s window + a hard map-size cap. Rate ≈ `Bytes /
max(window, LastSeen - FirstSeen)`, where the `max` floor stops a just-appeared
entry reading near-zero. One `sync.Mutex`; handlers snapshot-copy under lock then render unlocked;
`-race` the snapshot path.

**MISS-storm rollup.** At snapshot time, group active entries by `GameKey`, count
distinct client IPs with `Fetches > 0`; flag ≥ threshold, e.g. 3, as a storm with
a "Preload now" button. The button POSTs the CSRF-protected `/prefill/trigger`,
threading the token via `hx-headers` as `prefill.html` does, and is gated on
`posterAppID.MatchString(gameKey)`, since only numeric Steam appIDs are preloadable
and `depot:NNN`/unidentified can't be.

**View.** A dedicated authed page, so the 2–3s poll runs only while parked on
Live and not on every dashboard load: `GET /live` + `GET /live/feed` as a fragment,
authed by construction, so don't add it to public paths. Reuse the `nav`/`frame_close`
shell and the `hx-trigger="every 3s"` pattern; new `live.html` + `live_feed`
sub-template; add a Live link to `nav.html`. States: idle, an empty note; normal,
per-client/game rows with HIT/fetch/bytes/rate; and storm, highlighted with preload.

**Edge cases.** Idle is empty; restart is empty with log-time recency; NAT or a
single proxy IP collapses per-client rows but the game-level storm rollup still
works, since on a flat LAN `$remote_addr` is the real device IP.

**Tests.** `parse_test.go`: assert `RemoteAddr`, including IPv6. `ingest_test.go`:
fake the `EventSink`, assert `ingestLine` emits the expected event and a `-` line
never reaches the sink. Feed unit tests: window/cap eviction, HIT-vs-fetch
classification, rate floor, storm threshold, `-race` concurrent Record+Snapshot.
httptest: `/live` + `/live/feed` idle/populated/storm; preload button only for
numeric appIDs and carries CSRF.

**Files:** `internal/catalog/parse.go`, `internal/catalog/ingest.go`,
`internal/web/server.go`, `cmd/backend/main.go`,
`internal/web/templates/`, a new `live.html` and a `nav.html` link.
