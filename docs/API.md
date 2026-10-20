# Buffarr API

Buffarr exposes a JSON API under `/api/v1/` so scripts and integrations can do
what the web console does: list cached games, lock and delete them, read cache
stats, and trigger preloads. Typical uses are a Grafana or Home Assistant
dashboard pulling stats, or a cron job queuing preloads before a LAN party.

## Enabling and authentication

The API is disabled by default. Set `BUFFARR_API_KEY` in `.env` to enable it:

```bash
BUFFARR_API_KEY=$(openssl rand -hex 32)
```

Every request must present the key, in either header form:

```bash
curl -sk https://<host-ip>:9443/api/v1/stats -H "X-Api-Key: $KEY"
curl -sk https://<host-ip>:9443/api/v1/stats -H "Authorization: Bearer $KEY"
```

Requests without a valid key get `401`. With no key configured, every API
request gets `403`. Errors are always JSON: `{"error": "..."}`.

The API is versioned. These `/api/v1/` shapes are stable; breaking changes
would ship as `/api/v2/`.

## Endpoints

### `GET /api/v1/games`

Lists every cached, identified game above the Library size floor. Same set the
Library page shows.

```json
{
  "games": [
    {
      "platform": "steam",
      "game_key": "730",
      "name": "Counter-Strike 2",
      "locked": false,
      "size_bytes": 32212254720,
      "cache_key_count": 1842,
      "last_seen": "2026-07-01T18:22:04Z"
    }
  ]
}
```

`size_bytes` is the compressed download size, the bytes on the cache disk, not
Steam's installed size.

### `GET /api/v1/games/{platform}/{gameKey}`

One game, same shape as a `games` list element. `404` if not cached.

### `POST /api/v1/games/{platform}/{gameKey}/lock`
### `POST /api/v1/games/{platform}/{gameKey}/unlock`

Locks or unlocks a game. Locked games are never auto-evicted and cannot be
deleted. Returns the updated game. `404` if the game is not in the catalog.

### `DELETE /api/v1/games/{platform}/{gameKey}`

Deletes a game's cached files. Idempotent: deleting an already-gone game is a
`200`. Deleting a locked game is refused with `409`; unlock it first. The
actual file eviction runs in the background after the response.

```json
{"deleted": true}
```

### `GET /api/v1/stats`

Cache and bandwidth figures, matching the dashboard.

```json
{
  "disk_total_bytes": 4000225165312,
  "disk_free_bytes": 1846835937280,
  "disk_used_bytes": 2153389228032,
  "cache_cap_bytes": 3298534883328,
  "locked_bytes": 429496729600,
  "cached_games": 47,
  "bandwidth_saved_bytes": 9895604649984,
  "bandwidth_upstream_bytes": 3298534883328,
  "hit_rate_pct": 75,
  "bandwidth_since": "2026-05-14"
}
```

`cache_cap_bytes` is `0` when no cap is configured. Bandwidth fields are `0`
until the first request is recorded.

### `GET /api/v1/prefill`

Preload state: whether a run is active, and the queue.

```json
{
  "running": true,
  "queue": [
    {"id": 12, "position": 1, "platform": "steam", "app_id": "730",
     "name": "Counter-Strike 2", "status": "active"}
  ]
}
```

`status` is `queued`, `active`, or `failed`; failed rows carry an `error`
string.

### `POST /api/v1/prefill`

Queues owned Steam games for preload.

```bash
curl -sk -X POST https://<host-ip>:9443/api/v1/prefill \
  -H "X-Api-Key: $KEY" \
  -d '{"app_ids": [730, 570]}'
```

Ownership is verified against the operator's Steam library; un-owned or
unknown IDs are skipped, not fatal:

```json
{"queued": 1, "already_queued": 0, "skipped": [570]}
```

`400` on a malformed or empty body, `502` when Steam's Web API is unreachable
for the ownership check.

## Notes

- The key is a bearer credential with full operator power. Treat it like a
  password and rotate it by changing `BUFFARR_API_KEY` and redeploying.
- The API bypasses first-run setup gating, so stats monitoring works even
  before Steam is configured.
- CSRF tokens and Steam login do not apply to API requests; the key is the
  whole story.
