# Buffarr UX Review — code-grounded pass (2026-07-01)

A second UX pass, this time reading the actual templates and Go handlers
(`internal/web/`) rather than the running app. Purpose: reconcile the earlier
running-app review (`buffarr-ux-improvements.md`) against what the code now
does, correct two root-cause guesses that were wrong, and add findings a visual
review can't see.

TL;DR: several P1/P2 items from the first review have already shipped. Two of
the three P0 "bugs" are real but mis-diagnosed. There are two new bugs the
visual pass missed (no 404 handling; Settings loads stale CSS).

---

## Already shipped since the first review (stop re-doing these)

- **Library bulk actions** (doc #5): list view has a working sticky bulk bar —
  Lock / Unlock / Update / Delete, with select-all and a delete confirm.
  (`library.html:35-96`)
- **Library grid quick actions** (doc #5): hover Lock/Unlock/Delete on each
  card; Delete is correctly disabled on locked cards with an explanatory title.
  (`library.html:137-156`)
- **Library sort extended** (doc #4): Size / Name / Age, plus a List/Grid
  toggle that now actually renders a table. (`library.html:21-31`)
- **Truncated-title tooltips** (doc #16): `title="{{.Name}}"` on every poster
  title. (`library.html:159`, `prefill.html:66`)
- **Preload cached badge + search** (doc #4/#6, partial): name search filters
  client-side; cached games get a corner badge. (`prefill.html:32-36,56`)
- **Preload queue management** (new, beyond the doc): reorder up/down, retry
  failed, remove, cancel active, live progress + speed. (`prefill.html:161-233`)
- **Dashboard "Largest cached games" cards link to detail** (doc #10, partial).
  (`dashboard.html:94`)

---

## P0 — still real, but the earlier root-cause guesses were wrong

### 1. Header disk % disagrees with Dashboard disk % — it's rounding, not denominators
The first review guessed "different denominators (disk vs cap)". Not the cause.
Both figures now use the **same** disk denominator from one `Statfs`:

- Dashboard meter: `CacheUsedPct = int(used*100/total)` (`server.go:1875`)
- Header chip: `DiskUsedPct = 100 - int(free*100/total)` (`server.go:2438`)

`used = total - free`, but each side floors independently and in opposite
directions, so they differ by 1 whenever the true value isn't integral
(free=92.3% → chip shows 8%; used=7.7% → meter shows 7%). That reproduces the
reported 7% vs 8% exactly.

**Fix (one line):** derive the chip from the same expression —
`DiskUsedPct = int(used*100/total)` — so both floor the same quantity the same
way. Root-causes the whole class, not just this pair.

### 2. Dashboard meter labels a disk-based % with a cap denominator
Separate issue, same panel. The meter renders `{{.CacheUsedPct}}%` next to
"of 3.6 TiB · cap 3.4 TiB" (`dashboard.html:22-27`), but `CacheUsedPct` is
**disk-used**, not cap-used. So the percentage and the "cap" label describe
different denominators sitting side by side. The first review's recommendation
(show usage against the cache **cap**, which is what Buffarr manages) still
stands and still isn't done.

### 3. Preload "TiB" size bug — the formatter is fine; suspect the estimate source
`humanBytes` (`assets.go:20`) is correct across the GiB/TiB boundary — a bad
render there is impossible for a correct input. The 6.0 GiB→6.0 TiB report is a
clean 1024× factor, so the wrong value enters upstream in the depot-size sum
(`GetSize` → `parseSteamcmdDepots`, `steam.go:258-289`), which now prefers the
compressed `manifests.public.download` and falls back to `.size`
(uncompressed) then legacy `maxsize`. Recent commits reworked exactly this
path, so **it may already be fixed** — verify against the current build before
spending time. If it recurs, the bug is a depot reporting a mis-scaled field or
a compressed+uncompressed double-count, not the formatter. Add a unit test at
the source, not on `humanBytes`.

---

## New findings the visual review couldn't see

### 4. No 404 — every unknown path silently renders the Dashboard
`GET /` (`handleIndex`) is a catch-all in Go 1.22's mux and ignores
`r.URL.Path` (`server.go:573`). There is no `/preload` route — only `/prefill`
— so `/preload` renders the Dashboard with **200**, and so does any typo'd or
stale URL. The first review saw this as "the /preload page is broken"; the real
scope is broader: no route 404s. Either add a canonical `/preload`→`/prefill`
redirect and a real not-found handler, or at minimum make `handleIndex` return
404 for non-root paths.

### 5. Settings loads a stale CSS version
Every page links `app.css?v=5` except `settings.html`, which links `?v=4`
(`settings.html:7`). The `?v=` string is the cache-buster; after a deploy that
ships new CSS, the Settings page keeps serving the browser's cached v4 until a
hard refresh, so Settings can render visibly out of sync with the rest of the
app. Bump to `v=5`. (Better long-term: a single templated version constant so
these can't drift — but the one-line bump is the fix.)

### 6. Two destructive/mutating actions give no feedback
- **Settings "Reset stats"** has no `hx-confirm` and `hx-swap="none"`
  (`settings.html:78-81`): a click wipes the bandwidth counter with no
  confirmation and no visible result. (Matches doc #12, still open — and the
  no-feedback half is new.)
- **"Update"** on game detail and the bulk Update both use `hx-swap="none"`
  (`game.html:70`, `library.html:53`): clicking appears to do nothing unless a
  toast fires. The button label is also still bare "Update" with no hint that
  it re-downloads changed depots (doc #11).

---

## Still open from the first review (unchanged, condensed)

- **#4** Library has **no search** and **no platform/locked filters** (Preload
  has search; Library still doesn't).
- **#6** Cached state on Preload is still only a corner dot — no "Cached" text
  badge, no "hide cached" filter.
- **#7** Preload selection bar shows count only — no selection total size, no
  free-space / over-cap warning before Queue. (`prefill.html:83-90`)
- **#8** Preload checkboxes are now 18px but sit directly on box art with no
  scrim/border — low contrast until checked. (`prefill.html:51-53`)
- **#10** Dashboard stat tiles (Games cached, Locked, Free, Unmanaged) are not
  links; empty Preload panel has no CTA. (`dashboard.html:29-46`)
- **#11** Game detail is sparse: no date-cached / last-served / hit count;
  Delete confirm doesn't state the size freed.
- **#13** Pause-eviction is plain on/off with no timed auto-resume and no
  persistent "paused" banner across pages. (`settings.html:169-177`)
- **#14** No warning when max cache size is set above physical disk size, and no
  inline validation on the size inputs.
- **#15** Unknown Preload sizes render a bare "—" with no tooltip.
  (`prefill.html:70`)

---

## Keep as-is (working well)

- Preload multi-select sticky bar and queue management — solid, reuse the
  pattern for Library grid multi-select rather than inventing a new one.
- Toast auto-dismiss and client-side table sort in `nav.html` are clean and
  reduce server round-trips.
- Explanatory microcopy on Settings (eviction, cap/Lock-wins, min-game-size) is
  genuinely good.
