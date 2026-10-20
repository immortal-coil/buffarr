# Buffarr FAQ

Frequently asked questions. This mirrors the in-app About page at `/about`; keep
the two in sync when editing.

<details>
<summary>What is Buffarr, and how is it different from lancache?</summary>

Lancache caches game downloads on your LAN, so a game is pulled from the internet
once and then served to every other machine at local-network speed. Buffarr runs
that same caching architecture and adds a management layer on top: a poster-grid
library of what's actually cached, per-game disk usage, the ability to lock or
delete individual games, and on-demand preloading. Lancache gives you the cache. Buffarr lets you see and control what is in it.

</details>

<details>
<summary>Why not just use Steam's Local Network Game Transfers?</summary>

Steam's built-in transfer feature covers the simple case well: one PC has the
game installed, another pulls it at LAN speed, zero setup. Use it if that's all
you need. Buffarr covers what it can't:

- **No peer required.** Steam transfers need another PC that has the game
  installed and is online. Buffarr caches everything ever downloaded,
  independent of any client machine.
- **Preloading.** Buffarr can stage games nobody has installed yet, ahead of a
  LAN party or release day. Steam transfers can only copy what already exists.
- **A real cache, not installs.** Uninstall a game and the Steam-transfer copy
  is gone. Buffarr keeps it until you evict it.
- **Beyond one platform.** Steam transfers are Steam PC-to-PC only. A cache
  serves consoles and other launchers too.
- **Management.** Visibility, locks, size caps, and per-game deletion. Steam
  transfers offer none of that.

Two roommates with big drives should use the Steam feature. Buffarr is for the
dedicated-server case: LAN parties, clubs, and households with many machines.

</details>

<details>
<summary>How does Buffarr know which games are cached?</summary>

It doesn't read the cache files directly, since they're stored under opaque
hashed names. Instead it continuously tails the cache's access log, identifies
which game each request belongs to, mapping Steam depot IDs to app IDs, and keeps
a running per-game tally in a small database. A background pass reconciles those
totals against what's actually on disk.

</details>

<details>
<summary>Why is a game's cached size smaller than what Steam shows?</summary>

The size Buffarr reports is the **compressed download** size, meaning the bytes that
travel over the network and land in the cache. Steam's store and library show the
**installed** size, which is larger because the game is decompressed on disk
after it downloads. A title Steam lists as 50 GB installed might appear as roughly
30 GB cached here.

</details>

<details>
<summary>Why did a game downloaded through Steam show a smaller cached size than one preloaded through Buffarr?</summary>

Buffarr's per-game size comes from tailing nginx's access log, not from reading
the cache directly. Usually this is Steam itself, not a tailer bug: Steam skips
re-fetching chunks it can already validate locally, such as a shared
redistributable depot or leftovers from a prior partial install, so a first-time
install through
Steam can legitimately pull fewer bytes than the depot's full size. Preloading
through Buffarr always forces a full fetch of every chunk, so it's the more
reliable number if you want a title's true footprint. Either way, deleting a
game still cleans up correctly: it purges every depot the catalog tracked, plus,
for an identified Steam title, every depot Steam's own API lists for that app
-- so bytes the tailer never logged for any reason don't get left behind. See
[SIZING.md's cache maintenance section](SIZING.md#cache-maintenance) for more.

</details>

<details>
<summary>What does Lock do?</summary>

Locking a game protects it from removal. When you set a maximum cache size,
Buffarr keeps the cache under that limit by evicting the least-recently-used
*unlocked* games first. Locked games are never auto-evicted, even if that means
going over the limit, since locking always wins, and a locked game can't be
deleted until you unlock it.

</details>

<details>
<summary>A preload reported success, but the Library is still empty. Why?</summary>

Almost always nginx's HTTPS SNI passthrough is not working. Steam fetches its depot
*manifests* over HTTPS on port 443, and without that passthrough a preload
completes but caches close to zero bytes. If a preload reports success yet
nothing lands in the Library, run `docker compose ps` and confirm nginx reports
`healthy`.

</details>

<details>
<summary>How does Buffarr use Steam and my account?</summary>

Buffarr is self-hosted and talks only to Steam's own services:

- **Sign-in** uses Steam OpenID. You log in at Steam, and Buffarr only ever learns
  your public SteamID, never your password. Only the SteamID you configure as the
  operator is allowed in.
- **The Steam Web API key** is used read-only, to list the games you own for the
  Preload page and to confirm ownership before a preload. It reads nothing else
  and changes nothing.
- **Cover art and game details** such as descriptions, genres, and release dates
  come from Steam's public store and CDN endpoints, which need no key.
- **Preloading** is done by a separate, bundled SteamPrefill sidecar that you log
  into once, interactively. That login lets it download depots you already own on
  your behalf. The credentials stay on your server and are sent nowhere else.

Nothing about your library or account leaves your server except the calls Buffarr
makes directly to Steam.

</details>
