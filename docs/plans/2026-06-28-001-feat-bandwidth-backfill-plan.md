---
title: "feat: Backfill the bandwidth-saved metric from the access log on first run"
type: feat
date: 2026-06-28
status: ready
depth: standard
---

# feat: Backfill the bandwidth-saved metric from the access log on first run

## Summary

The "bandwidth saved" / cache-hit-rate metric (`bandwidth_daily`) only counts
forward from the moment the feature was deployed. On the live cache that means it
reads **0 saved** even though the current nginx access log holds **~111 GB of
real cache hits (27.8% hit-rate by bytes)** — every one of those hits landed
before the counter existed, so it never tallied them. This plan makes the metric
reflect the full current access log by backfilling it once, automatically, when
the bandwidth table is empty, so "total saved" is honest and persists across
restarts and redeploys.

The forward counter (`Ingester.RunOnce`) is unchanged in steady state. The
backfill fills only the gap between the start of the current log and the point
the tailer has already consumed.

---

## Problem Frame

`Ingester.RunOnce` (`internal/catalog/ingest.go`) tallies bandwidth per day as it
tails the access log: every served line adds to `total_bytes`, and `HIT` lines
also add to `hit_bytes` (the "saved" figure). The tally is **forward-only** — it
counts only lines the tailer emits after the bandwidth code shipped.

On an **existing** install the tailer was already far into the log when the
bandwidth feature was added, so all the historical `HIT` lines were emitted (and
catalogued per-game) *before* the bandwidth code ran, and were never counted. The
result is a metric stuck near 0 despite a cache that has demonstrably saved
~111 GB. The data still exists in the log; nothing reads it into `bandwidth_daily`.

A **fresh** install has no such gap — `RunOnce` reads the whole log from offset 0
and counts every line as it goes — so the fix must not double-count there.

---

## Scope Boundaries

**In scope**
- A one-shot backfill that reads the access log from byte 0 up to the tailer's
  persisted offset and populates `bandwidth_daily` per day.
- An automatic trigger: run the backfill on startup when `bandwidth_daily` is
  empty.
- Correcting `bandwidth_since` to the earliest backfilled day.
- A shared accrual helper so the backfill and the forward counter agree exactly
  on what counts as a hit / a served byte.

**Out of scope**
- Changing the forward counter's steady-state behavior.
- Recovering history older than the current log file (log rotation discards it;
  the backfill reflects "since the current log", not literally all-time).
- Any dashboard / UI change — the existing "Cache performance" card already reads
  `bandwidth_daily` and will show the corrected numbers with no template change.

### Deferred to Follow-Up Work

- **One-time production backfill (operator-run).** After this ships, the live
  cache's `bandwidth_daily` is non-empty (it holds today's partial forward data),
  so the on-empty trigger will not fire on its own. To surface the ~111 GB now,
  the operator clicks **Settings → Reset stats** (clears the table) and restarts
  the backend; the on-empty backfill then runs. Per the planning decision this is
  an operator step, not automated here. The "Reset stats + restart" sequence is
  the intended re-backfill mechanism in general.

---

## Key Technical Decisions

1. **Partition at the tailer offset — backfill owns `[0, offset)`, the tailer
   owns `[offset, ∞)`.** The tailer's persisted byte offset (`tailer_state.offset`
   for the log path) is the exact boundary between "already consumed" and "not yet
   read". The backfill counts everything before it; the forward counter counts
   everything at/after it. They never overlap, so there is no double-count — and
   this single rule makes both cases correct automatically:
   - *Fresh install:* offset is 0, so `[0, 0)` is empty → backfill is a no-op, and
     `RunOnce` counts the whole log forward as today.
   - *Existing install (or post–Reset stats):* offset is near the end → backfill
     fills the entire history up to it, the tailer continues forward.

2. **Trigger = `bandwidth_daily` empty at startup.** Simple, and it makes the
   existing **Reset stats** button (which clears the table) plus a restart the
   re-backfill mechanism for an already-running instance. The offset bound (KTD 1)
   is what keeps "empty at startup" safe on a fresh install. (Chosen over an
   explicit admin command; the on-empty trigger also makes fresh installs complete
   by default.)

3. **A partial trailing line needs no special handling.** The persisted offset is
   total-bytes-read and may end mid-line (the tailer keeps the remainder buffered).
   A backfill that reads `[0, offset)` and runs each line through `ParseLine`
   naturally drops that partial last line (no newline → parse error → skipped); the
   tailer emits and counts it forward once it completes. The effective boundary is
   the last newline before the offset, agreed on by both sides.

4. **One shared accrual helper.** Extract the per-line "add to total, add to hit if
   HIT, skip `-`" logic out of `RunOnce` into a helper that both `RunOnce` and the
   backfill call, so the backfilled history and the live counter can never diverge
   on the definition of a hit or a served byte. (This is the load-bearing
   correctness seam.)

5. **Stream the log, don't buffer it.** The live log is ~170 MB. Read it with a
   bounded reader (`io.LimitReader` to the offset) and a line scanner, accruing as
   it goes, rather than loading it into memory.

6. **Set `bandwidth_since` to the earliest backfilled day.** `AddBandwidth` only
   stamps `bandwidth_since` on first insert and `ResetBandwidth` sets it to *today*;
   after a backfill the data spans many days, so the backfill must explicitly set
   `bandwidth_since` to the earliest day it wrote, or the dashboard's "since" label
   contradicts the data.

7. **Run the backfill before the tailer loop starts.** In `main.go`, invoke the
   backfill synchronously after constructing the tailer/ingester and before
   `go ingester.Run(...)`. The offset is read once, up front, so there is no race
   with the forward counter. One-time bounded cost (a few seconds on a ~170 MB log)
   on the rare boot where the table is empty; every other boot it is a single empty
   check. (A background goroutine that captures the offset first is an acceptable
   alternative if boot latency ever matters; not needed now.)

---

## High-Level Technical Design

```
access log (one file, ~170 MB)

byte 0                          tailer offset (persisted)              EOF / now
  |───────────── backfill ──────────────|──────── forward counter ────────|
  |   reads [0, offset), ParseLine,      |  RunOnce: Poll() emits new      |
  |   accrue per-day, AddBandwidth       |  lines, same accrue helper      |
  |                                      |                                 |
  └─ complete lines only; partial        └─ completes + counts the         
     trailing line skipped (no newline)     buffered partial line forward  

Trigger (main.go, startup):
   bandwidth_daily empty? ── no ──> skip, start tailer as usual
        │ yes
        └─> BackfillBandwidth(offset) ──> set bandwidth_since = earliest day
                                          └─> start tailer (go ingester.Run)
```

Both arms feed `bandwidth_daily` through the same `AddBandwidth` upsert and the
same accrual helper; the offset is the only thing keeping them disjoint.

---

## Implementation Units

### U1. Extract a shared per-line bandwidth accrual helper

**Goal:** Make `RunOnce` and the future backfill share one definition of "what a
served line contributes" so they cannot drift.

**Requirements:** Supports KTD 4 (single source of truth for hit/total accounting).

**Dependencies:** none.

**Files:**
- `internal/catalog/ingest.go` (refactor `RunOnce`; add helper)
- `internal/catalog/ingest_test.go` (helper unit test)

**Approach:** Pull the inline body of the `RunOnce` per-line block — skip
`UpstreamCacheStatus == "-"`, bucket by `line.Time.UTC()` day, add
`BodyBytesSent`/`+1 req` to totals, and to hit totals when `UpstreamCacheStatus
== "HIT"` — into a small helper that takes the `map[string]*bwDay` and a
`LogLine` and mutates the map. `RunOnce` calls it; behavior is unchanged. Keep
`bwDay` as-is.

**Patterns to follow:** existing `bwDay` accumulation in `RunOnce`; existing table
of `ParseLine`-based cases in `internal/catalog/ingest_test.go`.

**Test scenarios:**
- A `HIT` line adds its bytes to both total and hit buckets for its UTC day.
- A `MISS` line adds to total only, not hit.
- A `"-"` (direct-return) line is skipped entirely (no bucket created).
- Two lines on the same UTC day accumulate into one bucket; lines on different
  days create separate buckets.
- Regression: existing `RunOnce` bandwidth test (`ingest_test.go`) still passes
  unchanged after the refactor.

**Verification:** `go test ./internal/catalog/` green; `RunOnce` diff is a pure
extraction (no behavior change).

---

### U2. Add `BackfillBandwidth` — read `[0, offset)` and populate the table

**Goal:** Compute the full per-day bandwidth history from the current log up to
the tailer's offset and write it, then fix `bandwidth_since`.

**Requirements:** Core of the feature; realizes KTDs 1, 3, 5, 6.

**Dependencies:** U1.

**Files:**
- `internal/catalog/tail.go` (a bounded full-read helper: open the log, read
  `[0, limit)`, return complete lines; plus an accessor for the persisted offset)
- `internal/catalog/ingest.go` (`Ingester.BackfillBandwidth()`)
- `internal/catalog/catalog.go` (set `bandwidth_since` to a given day; an
  emptiness check if not derivable from `BandwidthTotals`)
- `internal/catalog/ingest_test.go` (backfill tests)

**Approach:** Add an `Ingester.BackfillBandwidth()` that:
1. Reads the tailer's persisted offset for the log path (new accessor on `Tailer`,
   sourced from `tailer_state`).
2. Streams the log over `io.LimitReader(file, offset)` with a line scanner; each
   line goes through `ParseLine`; unparseable lines (including the partial trailing
   line, KTD 3) are skipped.
3. Accrues into a `map[string]*bwDay` via the U1 helper.
4. Writes each day with the existing `AddBandwidth` upsert.
5. Sets `bandwidth_since` to the earliest day written (new small catalog setter,
   or reuse the settings upsert), overriding whatever `ResetBandwidth` stamped.

Do **not** advance or touch `tailer_state` — the tailer owns the offset and
`[offset, ∞)`.

**Technical design (directional, not a spec):**
```
offset := tailer.Offset()              // persisted byte position
if offset == 0 { return nil }          // fresh install: nothing before the tailer
bw := map[string]*bwDay{}
scan lines of LimitReader(log, offset):
    line, err := ParseLine(text); if err != nil { continue }   // skips partial tail
    accrue(bw, line)                   // U1 helper
earliest := ""
for day, d := range bw:
    AddBandwidth(day, d...)
    earliest = min(earliest, day)
if earliest != "" { setBandwidthSince(earliest) }
```

**Patterns to follow:** `Tailer.Poll`'s open/seek/read in `tail.go`; `AddBandwidth`
upsert in `catalog.go`; day formatting `line.Time.UTC().Format("2006-01-02")`.

**Test scenarios:**
- Backfilling a synthetic log of mixed `HIT`/`MISS`/`-` lines across two days
  populates `bandwidth_daily` with the correct per-day total and hit bytes/reqs
  (cross-check against summing the lines directly).
- The `limit`/offset bound is respected: lines written **past** the supplied offset
  are **not** counted (proves `[0, offset)` only).
- A partial final line (bytes after the last newline, within the offset) is skipped
  by `ParseLine` and not counted (KTD 3).
- `offset == 0` → no-op: `bandwidth_daily` stays empty, no error (fresh-install
  guard).
- `bandwidth_since` is set to the **earliest** day present in the backfilled range,
  not the latest and not "today".
- `"-"` lines contribute nothing (delegated to U1, but assert at the backfill level
  too).

**Verification:** `go test ./internal/catalog/` green; a backfill over a fixture log
reproduces hand-summed totals; offset bound and partial-line handling proven by
assertion, not inspection.

---

### U3. Trigger the backfill on startup when the table is empty

**Goal:** Run the backfill automatically, exactly once, on the first boot where
`bandwidth_daily` is empty (fresh install, or after Reset stats), before the
tailer loop starts.

**Requirements:** Realizes KTDs 2 and 7.

**Dependencies:** U2.

**Files:**
- `cmd/backend/main.go` (between the ingester construction at ~`main.go:99` and
  `go ingester.Run(ctx, ...)` at ~`main.go:101`)
- `internal/catalog/ingest_test.go` or `internal/catalog/catalog_test.go`
  (empty-gate + no-double-count behavior at the package level)

**Approach:** After building the tailer + ingester, check whether `bandwidth_daily`
is empty (via `BandwidthTotals` total-requests == 0, or a dedicated count). If
empty, call `ingester.BackfillBandwidth()` (log failures, never fatal — passive
caching must keep working, matching the existing ingest error posture). Then start
`go ingester.Run(...)` as today. Guard the call so it cannot run when the table is
non-empty.

**Patterns to follow:** the non-fatal, log-and-continue error handling around
`ingester.Run` / `reconcile` in `main.go`; `Run`'s "log and keep going" comment.

**Test scenarios:**
- **No double-count across the boundary (load-bearing).** Given a fixture log,
  backfill `[0, offset)` and then run a forward `RunOnce` (tailer resuming from the
  same offset over the same file with new lines appended); assert the combined
  `bandwidth_daily` equals a single full-log accrual — i.e., the boundary line is
  counted exactly once. This is the test that proves the partition (KTD 1).
- Empty-gate: with a non-empty `bandwidth_daily`, the startup path does **not**
  invoke the backfill (no change to existing totals).
- Empty + offset 0 (fresh install): startup invokes the backfill, which is a no-op;
  a subsequent `RunOnce` over the whole log produces the correct totals (history
  comes from the forward path, not a double count).
- Backfill failure (e.g., unreadable log) is logged and does not stop startup or
  the ingest loop.

**Execution note:** Write the no-double-count boundary test first; it is the
invariant the whole design rests on. Make it fail against a naive "backfill the
whole log" implementation, then confirm the `[0, offset)` bound passes it.

**Verification:** `go test ./...` green; `go test -race ./internal/catalog/` clean;
manual reasoning trace that the empty-gate fires on exactly the intended boots.

---

## Risks & Mitigations

- **Double-counting the boundary.** Mitigated by the `[0, offset)` bound (KTD 1)
  and proven by the U3 boundary test. The partial-line case is covered by KTD 3.
- **`bandwidth_since` mismatch after Reset.** `ResetBandwidth` stamps "today";
  the backfill overrides it to the earliest backfilled day (KTD 6 / U2). Without
  this the dashboard label lies.
- **Log rotation loses pre-rotation history.** The backfill reflects only the
  current log file. Acceptable and documented; "saved since the current log" is
  far better than 0. No mitigation beyond honest framing.
- **Boot latency on a large log.** One-time, bounded, and only on an empty-table
  boot (KTD 7). If it ever matters, move to a goroutine that captures the offset
  first.
- **Scanner line-length limits.** nginx cachelog lines are short (~200 B); use a
  scanner buffer comfortably above that if the default is a concern.

---

## Dependencies / Sequencing

U1 → U2 → U3, in order. U1 is a safe pure refactor; U2 is the engine; U3 is the
thin wiring plus the invariant test. No external dependencies; no schema change
(`bandwidth_daily`, `tailer_state`, and the `bandwidth_since` setting all already
exist).

---

## Verification (whole feature)

- `go test ./...` and `go test -race ./internal/catalog/` green.
- A fixture-log backfill reproduces hand-summed per-day totals and hit-rate.
- The boundary test proves backfill + forward = single full accrual (no double
  count).
- Post-merge operator check (deferred, manual): on the live cache, **Reset stats**
  + restart, then confirm the "Cache performance" card shows ~111 GB saved /
  ~27.8% hit-rate and `bandwidth_since` reflects the log's earliest day.
