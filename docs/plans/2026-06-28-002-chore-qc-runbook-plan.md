---
title: "chore: Buffarr QC runbook — end-to-end verification after each deploy"
type: chore
date: 2026-06-28
status: ready
depth: standard
---

# chore: Buffarr QC runbook — end-to-end verification after each deploy

## Summary

A practical checklist for verifying that Buffarr is healthy and behaving
correctly after a deploy or restart. Covers seven areas in order of dependency:
automated tests first, then infrastructure health, DNS interception, cache
hit/miss, bandwidth metric accuracy, UI features, and a live preload smoke test.
All areas are runnable today against the prod stack at `192.168.1.10`.

No code changes. This is a run-it document.

---

## Problem Frame

Buffarr is a four-container stack with several independently-failable layers:
dnsmasq DNS interception, nginx HTTP caching + TLS SNI passthrough, the Go
backend, and the SteamPrefill sidecar. A deploy that builds and starts cleanly
can still fail silently — DNS resolving to the wrong IP, nginx serving stale
config, the bandwidth backfill skipping when it should run, or the UI reflecting
stale catalog state. This runbook makes the verification explicit and repeatable.

---

## Scope Boundaries

**In scope**
- Manual and CLI-based verification runnable by the operator without writing code
- Verification of all major features: health, DNS, cache, bandwidth, library UI,
  lock/delete, preload queue, kiosk
- Post-deploy verification steps (run after rebuilding and redeploying the backend)

**Out of scope**
- Writing new automated test suites or CI pipelines
- Load testing or performance benchmarking
- Security / penetration testing

---

## Key Technical Decisions

1. **Run in dependency order.** Infrastructure must be healthy before DNS, DNS must
   work before cache, cache must be hit before bandwidth counts are meaningful. A
   failure at any layer explains all downstream failures.

2. **Use the nginx access log as the ground truth for cache behavior.** The
   backend catalog is a derived view; the log is authoritative. When the UI and
   the log disagree, trust the log and investigate the tailer.

3. **The bandwidth backfill trigger is observable in the startup log.** The log
   line `catalog: bandwidth backfill complete` only appears on a fresh install or
   after Settings → Reset stats. On every routine redeploy `bandwidth_daily`
   persists in the named Docker volume, so the line is absent -- that is the
   normal healthy state. Verify correctness via Step 2 (catalog shows multiple
   days) rather than the startup log. The failure line `catalog: bandwidth backfill
   failed` is the signal worth watching; its absence means backfill either ran
   cleanly or was not triggered.

---

## Implementation Units

### U1. Automated test suite

**Goal:** Confirm the Go codebase is green before touching prod.

**Dependencies:** none.

**Approach:** Run from the source repo. The suite covers catalog ingestion,
parse, tailer, reconcile, and bandwidth backfill. Race detector catches
concurrency bugs in the catalog package specifically.

**Test scenarios / steps:**

```
# From the source repo root
go test ./...
go test -race ./internal/catalog/
```

**Pass criteria:**
- All packages report `ok` or `(cached)`
- No data race reported by the race detector
- If any package fails: do not deploy; fix first

**Verification:** Both commands exit 0 with no error output.

---

### U2. Container and infrastructure health

**Goal:** Confirm all five containers are up and passing their healthchecks
before proceeding to functional tests.

**Dependencies:** U1.

**Approach:** Check container status on the prod host, then verify the nginx
cache proxy and backend respond to their respective health endpoints.

**Test scenarios / steps:**

```bash
# On the prod host (192.168.1.10):
sudo docker ps --format "table {{.Names}}\t{{.Status}}" | grep buffarr
```

Expected output — all four containers in `(healthy)` state:
```
buffarr-backend-1    Up X minutes (healthy)
buffarr-dnsmasq-1    Up X minutes (healthy)
buffarr-nginx-1      Up X minutes (healthy)
buffarr-prefill-1    Up X minutes (healthy)
```

```bash
# Cache proxy health (from anywhere on the LAN):
curl -s http://192.168.1.10/healthz
# Expected: 200 OK, body "ok"

# Backend health (TLS, self-signed — skip cert check):
curl -sk https://192.168.1.10:9443/healthz
# Expected: 200 OK
```

**Pass criteria:**
- All four containers running; all show `(healthy)`
- Both curl health checks return HTTP 200

**Verification:** No container in `(unhealthy)` or `Exited` state; health
endpoints return 200.

---

### U3. DNS interception verification

**Goal:** Confirm dnsmasq is intercepting Steam CDN hostnames and returning
the cache server IP.

**Dependencies:** U2.

**Approach:** Query dnsmasq directly with `nslookup` or `dig` for a known
Steam CDN hostname. The answer must be `192.168.1.10`, not a real Akamai/CDN IP.
Also verify a non-intercepted hostname passes through to the upstream DNS
resolver unchanged.

**Test scenarios / steps:**

```bash
# Query dnsmasq on the prod host:
nslookup lancache.steamcontent.com 192.168.1.10
# Expected: Address: 192.168.1.10

nslookup steamcdn-a.akamaihd.net 192.168.1.10
# Expected: Address: 192.168.1.10

# Non-CDN hostname should pass through to upstream DNS (1.1.1.1):
nslookup google.com 192.168.1.10
# Expected: a real Google IP (NOT 192.168.1.10)
```

Verify the full list of intercepted domains is present in the config:
```bash
ssh 192.168.1.10 "sudo docker exec buffarr-dnsmasq-1 \
  grep -c address /shared/dnsmasq/cdn-wildcards.conf"
# Expected: >10 entries
```

**Pass criteria:**
- Steam CDN hostnames resolve to `192.168.1.10`
- Unrelated hostnames (`google.com`) resolve to real IPs
- `cdn-wildcards.conf` has entries for all enabled platforms

**Verification:** `nslookup` output shows `192.168.1.10` for CDN hostnames;
passes through real IPs for unrelated queries.

---

### U4. Cache hit/miss verification

**Goal:** Confirm nginx is caching Steam content and serving hits on subsequent
requests.

**Dependencies:** U3.

**Approach:** Point a test DNS query through dnsmasq, then use `curl` to fetch
a known Steam depot URL twice — first fetch should be a MISS (populates cache),
second should be a HIT. Verify in the nginx access log.

The easiest proxy: watch the live access log while downloading something real
from a client whose DNS is pointed at `192.168.1.10`.

**Test scenarios / steps:**

**Option A — watch the live log while a client downloads a Steam game:**
```bash
ssh 192.168.1.10 "sudo docker exec buffarr-nginx-1 \
  tail -f /var/log/nginx/cache-access.log" | grep -v healthz
```
Start a Steam download on any client using Buffarr's DNS. Within a few
seconds, lines with `"HIT"` should appear if the game was previously cached.
A first-ever download shows all `"MISS"` then `"HIT"` on a second download.

**Option B — curl a specific CDN URL twice (requires a real URL):**
To get a valid URL, grab a path from a recent HIT line in the access log (Option C
below) while a client is downloading. Then:
```bash
# First fetch: MISS (if not cached) — or HIT if already cached
curl -sv http://lancache.steamcontent.com/depot/<appid>/chunk/<hash>
# Second fetch of the same URL: HIT
curl -sv http://lancache.steamcontent.com/depot/<appid>/chunk/<hash>
```
Options A and C are simpler for quick verification; use Option B when you want
to confirm a specific URL round-trips through the cache.

**Option C — query the access log for recent activity:**
```bash
ssh 192.168.1.10 "sudo docker exec buffarr-nginx-1 \
  tail -50 /var/log/nginx/cache-access.log | grep -E 'HIT|MISS'"
```
Recent real traffic should show both HITs (served from cache) and MISSes
(cache population). If only MISSes appear, check that client DNS is actually
pointing at `192.168.1.10`.

**Pass criteria:**
- HIT entries appear in the nginx access log for previously-cached content
- MISS entries appear for genuinely new content (expected and correct)
- No sustained stream of MISSes for content that should be cached (would
  indicate a DNS misconfiguration or nginx cache corruption)

**Verification:** `tail` of access log shows `HIT` lines from real client
traffic.

---

### U5. Bandwidth metric accuracy

**Goal:** Confirm the bandwidth saved metric reflects real cache history and
the backfill ran correctly.

**Dependencies:** U4 (some real traffic needed for meaningful numbers).

**Approach:** Check the startup log for the backfill confirmation, then compare
the catalog DB totals against a manual awk summary of the nginx access log.
They will not match exactly (the log is append-only; the catalog is a running
total including pre-log history), but the order of magnitude should be consistent.

**Test scenarios / steps:**

**Step 1 — Check for backfill errors at last boot:**
```bash
ssh 192.168.1.10 "sudo docker logs buffarr-backend-1 2>&1 | \
  grep -E 'backfill|bandwidth'"
# Pass: no output, or only "catalog: bandwidth backfill complete"
# Fail: "catalog: bandwidth backfill failed" — means the log was unreadable at boot
```

Note: the completion line is only emitted on a fresh install or after Settings →
Reset stats. Its absence on a routine redeploy is normal — `bandwidth_daily`
persists in the named Docker volume across redeploys. Verify the data is correct
in Step 2 rather than checking for the completion line here.

**Step 2 — Check the current totals in the catalog:**
```bash
ssh 192.168.1.10 "sudo docker run --rm \
  -v buffarr_backend-data:/data alpine sh -c \
  \"apk add sqlite --quiet 2>/dev/null && \
  sqlite3 /data/catalog.db \
  'SELECT COUNT(*) as days, \
   ROUND(SUM(total_bytes)/1073741824.0,2) as total_gb, \
   ROUND(SUM(hit_bytes)/1073741824.0,2) as hit_gb, \
   SUM(total_requests) as reqs, \
   SUM(hit_requests) as hits FROM bandwidth_daily;'\"" 2>&1
# Expected: multiple days, total_gb >> hit_gb, hit-rate ~20-40%
```

**Step 3 — Quick awk cross-check against the nginx log:**
```bash
ssh 192.168.1.10 "sudo docker exec buffarr-nginx-1 awk '
  /\"HIT\"/ { hits++; hb += \$11 }
  !/\"HIT\"/ && \$11 ~ /^[0-9]+$/ { total++; tb += \$11 }
  END { printf \"HIT: %d reqs / %.2f GB\\nTotal: %d reqs / %.2f GB\\n\",
    hits, hb/1073741824, total+hits, (tb+hb)/1073741824 }
' /var/log/nginx/cache-access.log"
```

**Pass criteria:**
- Startup log confirms `catalog: bandwidth backfill complete`
- Catalog shows multiple days of data with >0 GB saved
- awk totals are in the same order of magnitude as the catalog (within 2x;
  exact match not expected — log rotation and the partition boundary explain gaps)

**Verification:** Catalog hit_gb > 0; awk and catalog agree on order of magnitude.

---

### U6. UI feature walkthrough

**Goal:** Verify the major UI features are functional in a browser session.

**Dependencies:** U2 (backend healthy).

**Approach:** Walk through the five key areas in the Buffarr UI. Each check
is a smoke test — confirm the feature loads, shows real data, and responds
to interaction.

**Access:** `https://192.168.1.10:9443` (accept self-signed cert warning).

**Test scenarios / steps:**

**6a. Library**
- Log in with Steam OpenID
- Confirm the game grid loads with poster art and size labels
- Largest games should appear first
- Click a game — confirm the detail page loads (description, genres, cached size)

**6b. Bandwidth / Cache performance card**
- On the dashboard, find the "Cache performance" card
- Confirm it shows a non-zero "saved" figure and a hit rate (expect ~28% based
  on current log data)
- Confirm `bandwidth_since` shows a date in June 2026 (not today's date, which
  would indicate the backfill didn't run and the forward counter just started)

**6c. Lock and delete**
- Find a game in the library you are comfortable testing with (a small one)
- Lock it: confirm the lock icon appears and the game is marked as protected
- Attempt to delete the locked game: confirm the UI rejects it
- Unlock it: confirm lock icon clears
- Delete it: confirm the game disappears from the library and the cache size
  for that game drops to 0

**6d. Preload queue**
- Find a game in your owned-games list that is NOT cached (or has a small
  cached size relative to its download size)
- Queue it for preload
- Confirm it appears in the preload queue card
- Confirm the progress bar animates and the status reflects downloading
- After completion: confirm the game appears in the library with a non-zero size

**6e. Kiosk (`/join`)**
- Navigate to `https://192.168.1.10:9443/join` (no login required)
- Confirm the poster grid of cached games loads
- Confirm the setup instructions are shown (Windows / macOS / Steam Deck /
  console tabs)
- Confirm no login prompt appears (the page must be publicly accessible)

**Pass criteria:**
- All five areas load without errors
- Library shows real game data with sizes
- Bandwidth card shows >0 GB saved and a date in June 2026
- Lock/unlock/delete cycle completes without errors
- Preload queues and runs a game through to completion
- `/join` loads without auth

**Verification:** No 500 errors or blank pages; features behave as described.

---

### U7. Preload smoke test (end-to-end)

**Goal:** Verify the full preload pipeline works: backend → SteamPrefill sidecar
→ nginx cache → catalog update.

**Dependencies:** U6 (Preload queue test), U4 (cache is functional).

**Approach:** Queue a small game for preload via the UI or directly via the
preload API, then verify the chain: the prefill sidecar receives the trigger,
downloads through nginx (visible as MISSes then completion), the game appears
in the library with a size, and the bandwidth counter increments.

**Test scenarios / steps:**

**Watch the full pipeline in one terminal:**
```bash
# Terminal 1 — nginx log (cache population)
ssh 192.168.1.10 "sudo docker exec buffarr-nginx-1 \
  tail -f /var/log/nginx/cache-access.log" | grep -v healthz

# Terminal 2 — prefill sidecar output
ssh 192.168.1.10 "sudo docker logs -f buffarr-prefill-1"

# Terminal 3 — backend log (catalog updates)
ssh 192.168.1.10 "sudo docker logs -f buffarr-backend-1"
```

Trigger a preload from the UI (Library → game → Preload, or dashboard → Queue
card).

Expected sequence:
1. Terminal 2: SteamPrefill begins, logs depot downloads
2. Terminal 1: nginx log fills with MISS lines (first download), then HIT lines
   if any chunks were already cached
3. Terminal 3: backend logs catalog updates as the tailer processes new lines
4. UI: preload bar advances; game size grows in the library

**Pass criteria:**
- SteamPrefill sidecar receives and runs the trigger without error
- nginx log shows download traffic (MISS lines) during the preload
- After completion, the game appears in the library with a non-zero size
- The bandwidth totals increment (visible in the Cache performance card after
  the next tailer tick, ~5 seconds)

**Verification:** Game moves from 0 to non-zero cached size in the library; nginx
log shows traffic during the preload.

---

## Risks and Notes

- **Bandwidth card shows today's date as `since`:** The backfill didn't run
  (table was non-empty at boot). Fix: Settings → Reset stats → restart backend.
  Watch for `catalog: bandwidth backfill complete` in the startup log.

- **All-MISS access log after a deploy:** Check that client DNS is pointing at
  `192.168.1.10`. The most common cause is a client using a different DNS
  resolver that doesn't forward CDN hostnames to dnsmasq.

- **Backend redeploys log the operator out.** Sessions are in-memory. One logout
  per backend deploy is expected.

- **`/join` returns 401:** The auth gate is misconfigured or the public path
  bypass for `/posters/` was lost. Check `setupGate` and `authGate` in
  `internal/web/server.go`.

- **Preload exits 0 but library stays empty:** the nginx stream SNI passthrough
  on :443 may be misconfigured. Steam fetches depot manifests over HTTPS; without
  the passthrough SteamPrefill exits cleanly with 0 bytes cached. Check
  `buffarr-nginx-1` is healthy and port 443 is bound (`ss -tlnp | grep :443` on
  the host).
