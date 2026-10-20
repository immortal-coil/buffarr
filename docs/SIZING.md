# Buffarr Sizing Guide

What numbers to set, and what box to run Buffarr on. For the eviction model these
knobs feed into, see [README.md](../README.md). For the per-variable reference, see
`.env.example`.

---

## Quick reference

Find the row for your cache disk. These are starting points; check your real free
space with `df -h` and adjust.

| Cache disk | `CACHE_MAX_SIZE` | `CACHE_MIN_FREE` | `CACHE_KEYS_ZONE` |
|---|---|---|---|
| 1 TB | `900g`  | `100g` | `32m`  |
| 2 TB | `1800g` | `200g` | `64m`  |
| 4 TB | `3700g` | `300g` | `128m`  |
| 8 TB | `7700g` | `300g` | `256m` |

**Hardware, in one line:** 10 GbE into a capable switch, NVMe (or ZFS with an SSD
cache tier) for the cache disk, and generous RAM for page cache. CPU barely matters.

Everything below is the *why* behind those numbers.

---

## Hardware

The cache box does two jobs with different bottlenecks. **Filling** the cache is
bounded by your internet uplink, which no hardware here can change. That is what
preload is for, see the last section. **Serving** clients is bounded by disk, NIC,
and RAM. The serving side, ranked by impact:

- **Network, meaning the NIC and switch, is the ceiling.** 1 GbE caps the whole
  host at ~118 MB/s, so one client install saturates it and everyone else waits.
  10 GbE, or 2.5/5G, lets many clients pull near line-rate at once. The switch matters as
  much as the card: a fast uplink from the cache, clients on their own ports.
- **Disk: optimize for concurrent random reads.** A multi-TB cache will not fit in
  RAM, and a busy event is dozens of clients reading different slices at once, not
  one sequential stream. That punishes spinning HDDs, which seek-thrash, and rewards
  **NVMe/SSD**. For capacity at reasonable cost, tier it: ZFS with an SSD cache or
  special vdev, plus ARC in RAM, keeps hot slices on fast media and the cold bulk
  on cheap disk. nginx uses `aio threads` and large `output_buffers` so disk reads
  do not block its workers.
- **Filesystem: a full library is hundreds of thousands of small files, mostly for
  reasons outside nginx's control.** For Steam specifically, the file count is
  driven by Valve's own SteamPipe chunking, where each depot chunk is a complete,
  independent object capped around ~1 MB, landing as one cache file each --
  `slice 4m`, set in `10_cache.conf`, never touches them, since nginx's slice module only
  fragments objects *larger* than the slice size, and Steam chunks already come in
  under it. The slice size mainly matters for other CDNs that serve large single
  files -- Battle.net NGDP archives, WSUS `.cab`s, League patches -- where a bigger
  slice genuinely means fewer on-disk fragments. Either way, a Steam-heavy library
  ends up as a lot of small files, and that has filesystem consequences: **ZFS**
  feels it more than most, since ARC caches metadata such as dnodes and indirect
  blocks inside the same capped RAM budget it uses for data, so a very high file count
  competes with the data cache you actually want warm. **XFS** and **ext4** don't
  have that tradeoff -- they lean on the ordinary Linux page cache and dentry/inode
  cache, both of which scale with all available RAM. Of the two, XFS handles huge
  small-file counts better, with B+trees throughout and inodes allocated
  dynamically; ext4 works fine but sizes its inode table at `mkfs` time, so
  over-provision inodes if you go that route. None of this is a reason to avoid
  ZFS -- its other properties such as tiering, snapshots, and checksums are worth
  it -- just don't be surprised if ARC hit rate looks tighter than a raw RAM count
  would suggest on a Steam-heavy library. **Filesystem-level tuning**, such as ZFS
  `zfs_arc_meta_limit`/`zfs_arc_dnode_limit`, a metadata special vdev, or `mkfs`
  inode ratios, is outside the scope of this document -- it's genuinely useful if file-count
  pressure becomes a real problem, but it's a filesystem-administration topic, not
  a Buffarr setting.
- **RAM is the quiet win when a lot of clients want the same thing at once** --
  say, a LAN party where everyone launches the same game together. Whichever
  cache tier holds it -- the OS page cache on XFS/ext4, or ARC on ZFS as noted above --
  the first client to pull a file brings it into RAM, and everyone else asking
  for it around the same time gets it at memory speed instead of touching disk.
  More RAM means a bigger hot set stays resident. The key index, below, also
  lives in RAM.
- **CPU is rarely the limit.** The bulk path is plain HTTP with no TLS, disk I/O is
  offloaded to a thread pool, and the Go backend is light. A modest multi-core is
  plenty.

---

## The settings

**`CACHE_MAX_SIZE`** is nginx's size backstop, not the primary evictor. Set it at
or below the disk size. A larger value never binds and `CACHE_MIN_FREE` becomes
the real guard. The backend's own Lock-aware cap, set in Settings, can sit
below it, so the nginx backstop effectively never fires.

**`CACHE_MIN_FREE`** is the reserve nginx never eats into. On a multi-TB disk,
`10g` is too thin; leave `100g` or more of headroom.

**`CACHE_KEYS_ZONE`** is the easy one to get wrong, and the symptom is invisible:
set it too small and it silently caps the cache well below the disk. nginx keeps an
in-RAM index of every cached 4 MB slice, and roughly **8000 keys fit per 1 MB of
zone**, so `100m` indexes about 800k keys, which is **~3.2 TB** of content. Past
that, nginx evicts index keys and re-downloads, even with free disk. Rule of thumb:
**`CACHE_KEYS_ZONE` in MB is about your cache size in GB divided by 32**, which is
what the table uses. It costs that many MB of RAM, which is cheap insurance on a multi-TB box.

> **A note on game sizes.** Buffarr reports the *compressed download* size, not the
> installed size, so a game Steam lists as 50 GB installed is more like 30 GB
> cached. A 25-game event playlist lands around 1 to 2 TB. Size the cache for the
> library you actually want to keep warm.

---

## Cache maintenance

Buffarr's per-game size comes from tailing nginx's access log, as the FAQ
explains, not from reading the cache directly. A game installed directly through
Steam can show a smaller cached size than the same game preloaded through Buffarr
-- that's normally Steam itself, not a tailer bug: the Steam client skips
re-fetching chunks it can already validate locally, such as a shared
redistributable depot or leftovers from a prior partial install, so a first-time
install can legitimately pull
fewer bytes over the wire than a full depot really contains. Preloading through
Buffarr always forces a full fetch of every depot chunk, so it's the more
reliable number if you want to know a title's true footprint.

Whatever the reason for a gap between tracked and actual bytes, it can't overflow
the cache. Every slice nginx writes gets a `keys_zone` entry the moment it lands,
independent of Buffarr's own catalog, so `max_size`/`min_free` eviction, the
settings above, still LRU-evicts under disk pressure regardless of what Buffarr's
catalog knows about. The disk backstop holds either way; what an inaccurate tally
costs you is evicting the wrong thing first, not running out of space.

Two things keep the catalog itself honest without operator effort:

- **Reconcile** runs on an interval, corrects per-game size drift against what's
  actually on disk, and prunes catalog rows whose backing file is gone.
- **Delete** purges every depot the catalog tracked for that game, plus -- for an
  identified Steam title -- every depot Steam's own API lists under that app ID.
  The second part is the actual guard against orphaned files: even if some request
  was never logged for whatever reason, such as log rotation racing the tailer or
  a chunk Steam fetched a way the tailer didn't parse, delete still knows the
  full depot set for the app and removes it.

If you ever want a clean slate anyway, or `du` on the cache directory looks
meaningfully bigger than the sum of what the Library page reports, stop the
stack, wipe `CACHE_ROOT/cache`, and restart: nginx rebuilds an empty cache and
Reconcile clears the now-stale catalog rows on its next pass.

---

## Filling: that's preload, not hardware

Fast disk and NIC make *serving* scream but do nothing for *filling*, which is gated
by your internet connection no matter what hardware you buy. That is what **preload**
is for: warm the cache the night before, off-peak, so at the event only the fast
LAN-bound serve runs, and even the first copy of each game never touches the internet
during the party.
