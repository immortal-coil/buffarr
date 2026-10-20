---
title: Buffarr UX + visual-identity pass
date: 2026-07-01
execution: code
status: draft
sources:
  - buffarr-ux-review-2026-07-01.md   # code-grounded review (findings)
  - buffarr-ux-improvements.md          # original running-app review
---

# Buffarr UX + visual-identity pass — implementation plan

## Thesis

Buffarr works but reads as a generic dark dashboard (`#0e1014` + `#5b8cff`,
system fonts). This pass gives it one memorable instrument — a **segmented
capacity gauge** that makes the Lock-wins model visible — and folds most of the
review's bugs into that same component, because the gauge *is* the three
confusing disk numbers done right. Everything else is quiet correctness and
identity polish around that one bold move.

Constraint that shapes everything: **single static Go binary, offline LAN, no
CDN, htmx-only, no build toolchain.** Any asset (fonts) must be `go:embed`-ed.
No new runtime dependency is in scope.

## Scope boundaries (non-goals)

- No new dependencies, no JS framework, no CSS build step — stay htmx + one
  `app.css`, assets embedded.
- No changes to caching / eviction / prefill **backend logic**. This pass is
  presentation + a read-model merge only.
- No DB migration unless U13's "date cached / hit count" is explicitly wanted
  (default: deferred — see Implementation-Time Unknowns).
- No Figma sync (no design file exists).

## Implementation-Time Unknowns (resolve before the unit that needs them)

1. **Fonts (blocks U8).** Offline single binary can't link Google Fonts. Options:
   (a) embed one woff2 display/grotesk + one mono via `go:embed` (~80–250 KB,
   keeps the "instrument" identity); (b) curated system stacks only (0 KB, less
   distinctive). **Recommend (a):** embed a mono for data (e.g. a subset of
   JetBrains Mono) + a grotesk for labels. Needs a yes on binary-size cost.
2. **Amber as default theme (blocks U9).** Changes the look for existing users.
   Product call — confirm before flipping the default.
3. **Game-detail history (scopes U13).** `LibraryEntry.LastSeen` exists →
   "Last served" is cheap. **"Date cached" and per-game hit count are NOT in the
   schema** — they'd need new columns + log-tail wiring. Default: ship LastSeen
   only, defer the rest.
4. **Gauge segment palette (part of U6).** Pick locked / evictable / free hues
   that hold in Slate, Paper, and Amber (derive from existing `--ok/--warn/
   --muted` + `color-mix`, don't invent a 4th ramp).

---

## Track A — Correctness (independent, ship first, no design dependency)

### U1 — Real 404s + `/preload` alias
- **Why:** review #4. `handleIndex` is a catch-all; every unknown path renders
  the Dashboard with 200.
- **Files:** `internal/web/server.go` (`handleIndex`, mux), `server_test.go`.
- **Approach:** in `handleIndex`, `if r.URL.Path != "/" { http.NotFound; return }`.
  Add `GET /preload` → 301 to `/prefill` (or rename canonical route to
  `/preload` and redirect `/prefill`; pick one, update nav).
- **Verify:** GET `/nonsense` → 404; GET `/preload` → redirects to the Preload
  page; existing route tests still green. New test asserts the 404.

### U2 — Kill CSS-version drift
- **Why:** review #5. `settings.html` links `app.css?v=4`; every other page uses
  `?v=5` → Settings serves stale cached CSS after a deploy.
- **Files:** all `internal/web/templates/*.html`, `assets.go` or a template func.
- **Approach:** expose one `AssetVer` constant/func to templates, replace every
  `?v=N` literal with `{{assetVer}}`. Bump once, drift becomes impossible.
- **Verify:** grep shows zero hard-coded `?v=` literals; all pages render the
  same version string.

### U3 — Feedback on mutating actions
- **Why:** review #6. "Reset stats" has no confirm and `hx-swap="none"`;
  "Update" buttons look inert.
- **Files:** `settings.html`, `game.html`, `library.html`, handler(s) that
  should emit a toast.
- **Approach:** add `hx-confirm` to Reset stats; have reset + update handlers
  return the OOB `toast` fragment ("Stats reset.", "Update queued for X.").
  Relabel/clarify "Update" (tie to U13's helper text).
- **Verify:** clicking each shows a toast; Reset prompts first.

### U4 — Unify the disk percentage
- **Why:** review #1. Chip `100 - floor(free%)` vs meter `floor(used%)` → 7 vs 8.
- **Files:** `server.go` (`render` sets `DiskUsedPct`).
- **Approach:** `DiskUsedPct = int(used*100/total)`, same expression as
  `CacheUsedPct`. (Absorbed into U5 if U5 lands first — keep here as the
  standalone fix if Track B is deferred.)
- **Verify:** chip and meter always match; test asserts equality for sample
  Statfs values.

---

## Track B — Signature: the capacity gauge (the one bold move)

### U5 — Shared capacity read-model
- **Why:** the gauge, the dashboard meter, the prefill free/total line, and the
  nav chip all need the *same* numbers. Today they're computed in three places
  (`mergeCacheStats`, `handleIndex` locked loop, `diskFreePct`). Roots out
  review #1/#2/#9 at the source.
- **Files:** `server.go` (new `capacityView` merged in one place).
- **Approach:** one struct → `{TotalBytes, FreeBytes, UsedBytes, LockedBytes,
  EvictableBytes, CapBytes, UsedPct}`. `LockedBytes` from the existing `List()`
  locked-sum (already computed on dashboard); `EvictableBytes = Used - Locked`.
  Feed all four surfaces from it. Depends on nothing.
- **Verify:** unit test — the same input yields one `UsedPct`; `Locked +
  Evictable + Free == Total` (within rounding); no surface recomputes.

### U6 — Gauge component (replaces the thin meter)
- **Why:** design signature + review #2/#9. Encode Lock-wins visually.
- **Files:** `dashboard.html`, `prefill.html` (`prefill_status`), `app.css`,
  maybe a `{{define "capacity_gauge"}}` partial.
- **Approach:** segmented bar — locked / evictable / free — with the **cap as a
  marked line** on the disk track, mono readouts under each segment. Theme-aware
  colors (Unknown #4). One partial, included on Dashboard + Preload. Retire
  `.meter-*` or restyle it.
- **Verify:** renders in all three themes; segments sum to the track width;
  screenshot check (Amber + Slate); reduced-motion respected; keyboard/focus
  unaffected.

### U7 — Preload projection + over-cap warning
- **Why:** review #7 + capacity warning. Turns the selection counter into a
  preview of the instrument that matters.
- **Files:** `prefill.html` (JS selection handlers already exist), gauge partial.
- **Approach:** sum selected cards' estimated bytes client-side; show
  "N selected · +XX GiB → NN%" in the sticky bar and ghost the projected fill on
  the gauge; flip a warning style when projected use crosses `CapBytes` or
  `FreeBytes`. Depends U5 + U6.
- **Verify:** selecting games updates the total + ghost fill live; crossing the
  cap shows the warning before Queue is pressed.

---

## Track C — Identity polish

### U8 — Typography (needs Unknown #1 resolved)
- **Files:** `app.css`, `assets.go` (embed), templates as needed.
- **Approach:** per the resolved font decision — data (bytes, %, speeds,
  hit-rate) in the mono role with tabular figures; labels/headings in the
  grotesk. Wire `--font-ui` / `--font-mono` to the embedded faces or the curated
  stacks; add `@font-face` served from the embedded static FS.
- **Verify:** data reads as an instrument panel; no CDN request; binary still
  builds; fonts load offline.

### U9 — Amber as default identity (needs Unknown #2 confirmed)
- **Files:** `:root` in `app.css` (make Amber the default token set; keep Slate/
  Paper as `[data-theme]` alternates), theme-cycle order in `nav.html`.
- **Verify:** first load (no stored theme) is Amber; cycle still reaches all
  three.

---

## Track D — Grid & remaining review items (independent, lower priority)

### U10 — Poster selection/state language
- Review #6, #8. Visible "Cached" badge (not a bare dot) + "hide cached" toggle
  on Preload; checkbox scrim/border for contrast on box art; selected-card
  outline. Files: `prefill.html`, `library_card`, `app.css`.

### U11 — Actionable dashboard
- Review #10. Tiles (Games cached / Locked / Free / Unmanaged) link into the
  relevant filtered view; empty Preload panel gets a "Start a preload →" CTA.
  (Locked/Unmanaged targets depend on U12 filters existing.)

### U12 — Library search + filters
- Review #4. Add name search (reuse Preload's client-side filter) + platform and
  Locked/Unlocked filters. Files: `library.html`, nav sort JS.

### U13 — Game-detail substance (scoped by Unknown #3)
- Review #11. Wire `LastSeen` → "Last served"; add Update helper text
  ("Re-download changed depots…"); delete confirm states the size freed. Defer
  date-cached / hit-count unless columns are added.

### U14 — Settings safety
- Review #13/#14/#15. Timed pause (24h / 72h / until date) + a persistent
  "Eviction paused · resumes in Xh" banner; warn when cap > physical disk +
  inline size-input validation; tooltip on unknown-size "—".

---

## Execution order & dependencies

```
Track A (U1 U2 U3 U4) ── independent, land first (small, high-confidence)
Track B  U5 → U6 → U7  ── the signature; U4 folds into U5
Track C  U8 (needs fonts decision) · U9 (needs product OK) ── parallel to B/D
Track D  U10 U11 U12 U13 U14 ── independent; U11 wants U12
```

Suggested first slice to prove the direction: **U5 + U6** (the gauge) plus the
cheap Track A fixes. That delivers the identity move and clears review
#1/#2/#9/#4/#5 in one visible increment; the rest is additive.

## Testing posture

- `internal/web/server_test.go` already exercises routes and fragments — extend
  it: 404 test (U1), capacity-merge invariants (U5), projection math (U7),
  render smoke tests per template.
- No new framework. Prefer one integration-style render assertion per unit over
  mock-heavy unit tests, matching the existing test file.

## Open decisions for you (before I build)

1. Fonts: **embed a woff2 pair** (distinctive, +binary size) or **system stacks
   only** (safe, plainer)?
2. Make **Amber the default** theme, yes/no?
3. Game detail: ship **LastSeen only** now, or add **date-cached + hit-count**
   columns (bigger, touches the log-tail path)?
4. First build slice: **U5+U6+Track A**, or a different starting scope?
