# Buffarr UX Improvements

Findings from a UX review of the running app (July 2026). Buffarr is a self-hosted manager for a LAN game-download cache: browsable library of cached games, per-game disk accounting, lock/delete, and on-demand preloading of owned games. Pages: Dashboard, Library, Library detail (`/library/steam/<appid>`), Preload (`/prefill`), Settings, About.

Work through sections in order. P0 items are bugs that undermine trust in the displayed numbers; fix them before polish.

---

## P0 — Bugs

### 1. Wrong size units on the Preload page
Some estimated game sizes render with TiB units at GiB-scale values:

- BioShock Infinite: shows `~6.0 TiB` (should be ~6.0 GiB)
- Borderlands GOTY: shows `~15.9 TiB` (should be ~15.9 GiB)

Likely a unit-conversion or formatting bug in the size formatter used for preload estimates (library sizes on other pages appear correct). Audit the shared byte-formatting helper and add unit tests around the GiB/TiB boundary.

**Acceptance:** all preload size estimates display the correct unit; formatter has tests for values near 1024-boundaries.

### 2. `/preload` route renders the Dashboard
- Navigating directly to `/preload` renders the Dashboard content (URL stays `/preload`).
- The "Preload" nav link goes to `/prefill`, which renders correctly.

So deep links / refreshes / bookmarks to the page labeled "Preload" are broken or inconsistent. Decide on one canonical route — `/preload` matches the UI naming — and redirect the other (301 or client-side redirect). Ensure unknown routes show a 404, not a silent Dashboard fallback.

**Acceptance:** `/preload` renders the Preload page; `/prefill` redirects to `/preload` (or vice versa); refresh on any route re-renders the same page; unknown routes 404.

### 3. Header disk % disagrees with Dashboard disk %
Header widget shows `Disk … 8%`; Dashboard "Disk usage" panel shows `7%`. Same metric, two values. Root cause is likely different denominators (3.6 TiB physical disk vs 3.4 TiB configured cache cap) and/or different rounding.

Pick one definition — recommend usage vs **cache cap**, since that's what Buffarr manages — use it in both places, and round the same way.

**Acceptance:** header and Dashboard always show the same percentage; a tooltip on the header widget states the definition (e.g. "285.1 GiB of 3.4 TiB cap").

---

## P1 — High-value UX

### 4. Library: add search, sort, and filters
The Preload page has a name search; the Library has none — only a `Size` sort dropdown and a List/grid toggle. With a growing cache this won't scale.

- Add a search-by-name input (same component/behavior as Preload's).
- Extend sort options: Size, Name, Date cached.
- Add filters: platform, Locked/Unlocked.

Also verify the **List** toggle — clicking it produced no visible change during review. Fix or remove.

### 5. Library: quick actions and bulk actions
Every lock/delete currently requires opening a game's detail page. Add:

- Hover quick actions on grid cards (Lock/Unlock, Delete with confirm).
- Multi-select with the same sticky bottom bar pattern the Preload page already uses (`N selected · Select all · Clear · [action]`), with bulk Lock and bulk Delete.

Reuse the Preload selection bar component for consistency.

### 6. Preload: make "already cached" obvious
Cached games currently show only a small unlabeled green dot in the card corner.

- Replace/augment with a visible "Cached" badge on the card.
- Add a "Hide cached" filter toggle near the search box.
- Tooltip on the badge: "Already in cache (7.3 GiB)".

### 7. Preload: show total size of selection + free-space warning
The sticky selection bar shows only a count ("2 selected"). Estimated sizes are already on each card and free space is known (shown in the Status panel).

- Show "N selected · ~XX GiB" in the bar.
- If the selection total exceeds free space (or would push usage past the cache cap), show an inline warning in the bar before Queue is pressed.

### 8. Preload: checkbox contrast
Selection checkboxes are dark gray on dark box art — nearly invisible until checked. Increase contrast (light border, subtle backdrop/scrim behind the checkbox), and give selected cards a visible outline/overlay so selection state is scannable across the grid.

### 9. Dashboard: clarify the disk-usage panel
Panel reads `7% — 285.1 GiB used — of 3.6 TiB · cap 3.4 TiB`. Two denominators next to one percentage is confusing.

Rephrase as one primary line with the cap as the denominator, disk as secondary context, e.g.:
`285.1 GiB of 3.4 TiB cap (8%)` with a secondary line `Disk: 3.6 TiB total · 3.3 TiB free`.

---

## P2 — Polish

### 10. Dashboard: make stat cards actionable
- "15 Games cached" → links to Library.
- "0 Locked" → Library filtered to locked.
- "59.4 MiB Unmanaged" → Library's Unmanaged cache section.
- Empty Preload panel ("Nothing downloading.") → add CTA button "Start a preload →" linking to the Preload page.
- Confirm "Largest cached games" cards link to their detail pages.

### 11. Game detail page: more substance, clearer actions
The detail page shows artwork, description, cached size, download size, lock status, and Update / Lock / Delete buttons — with a lot of empty space.

- Add: date cached, last served (last cache hit), per-game bytes served / hit count if available.
- "Update" is ambiguous — add helper text (e.g. "Re-download changed depots to bring the cached copy up to date").
- Merge the "Unlocked" status text and "Lock" button into a single labeled toggle.
- Ensure Delete has a confirmation dialog stating the size being freed.

### 12. Destructive actions: show impact + confirm
- Unmanaged cache "Clean up" button: show reclaimable total on the button or beside it ("Clean up · reclaims 59.4 MiB") and confirm before executing.
- Settings "Reset stats": confirm, and state what is and isn't affected (stats only; cached games untouched).

### 13. Settings: pause-eviction should auto-resume
Eviction pause is a plain on/off. It exists for LAN events, and an off switch is easy to forget — the cache then grows past cap indefinitely.

- Offer "Pause for 24h / 72h / until date" alongside indefinite.
- When paused, show a persistent banner or header indicator ("Eviction paused · resumes in 18h") on all pages.

### 14. Settings: validate size inputs, warn on cap > disk
- nginx-style size inputs (`100m`, `3500g`) already show a parsed preview ("Current: 100.0 MiB") — keep that; add inline validation for unparseable input.
- Warn when max cache size exceeds physical disk size.

### 15. Unknown sizes: explain the "—"
Some Preload entries show a bare `—` for size (BioShock, Alan Wake's American Nightmare). Add a tooltip: "Size unknown until first metadata fetch" (or the actual reason).

### 16. Truncated titles
Grid cards ellipsize long titles ("The Outer Worlds: S…"). Add `title` attribute / tooltip with the full name.

### 17. Background artwork readability
Random game artwork behind Dashboard/Library/Preload content can get bright and busy behind text. The Settings toggle exists — also strengthen the dark scrim/overlay so text contrast holds regardless of the artwork.

---

## Keep as-is (working well)

- Preload multi-select sticky bar pattern (count / Select all / Clear / Queue selected) — extend it to Library rather than changing it.
- Explanatory microcopy throughout (eviction behavior, unmanaged cache, `~` on estimates, cache-performance counting caveat) and the About/FAQ page.
- Settings previews of parsed size values.
