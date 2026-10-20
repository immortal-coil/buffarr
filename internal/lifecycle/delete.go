package lifecycle

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/nginx"
)

// DepotsForApp returns every depot ID Steam's own API lists under a numeric
// Steam appID (e.g. *identify.SteamResolver.DepotsForApp). Delete uses it to
// widen its purge scope beyond cache_keys: a real Steam client's own
// downloads don't always route through nginx in a way the log tailer
// captures for every depot, so cache_keys can undercount what actually
// landed on disk. nil is a valid no-op (Steam resolution not configured).
type DepotsForApp func(appID int) []int

// ErrMonitored is returned when Delete is called on a monitored entry.
// R5/KTD12: delete is hard-rejected while monitored, not auto-unmonitored
// -- the operator must call Unmonitor first, preserving an explicit
// two-step audit trail.
var ErrMonitored = errors.New("library entry is monitored; unmonitor it before deleting")

// ErrGameNotFound is returned when (platform, gameKey) has no catalog row
// at all -- distinct from a game that exists with zero cache keys (see the
// no-op path in Delete), which is a normal race with reconciliation/a
// prior delete, not an error.
var ErrGameNotFound = errors.New("library entry not found")

// Delete removes a library entry's cached files (R6). It computes the
// wildcard PURGE prefixes for every distinct depot/NGDP key under the game
// (KTD2/KTD11), then optimistically clears the catalog -- resetting
// monitored to off (R16; defensive, since monitored=true already blocks
// reaching here) and deleting the cache_keys rows -- and RETURNS the
// prefixes for the caller to evict in the background. The games row itself
// is left in place so a later re-cache revives the same logical entry (R16).
//
// This is deliberately optimistic and asynchronous. A wildcard PURGE scans
// the whole cache zone and can take many minutes (see nginx.purgeRequest-
// Timeout); doing it synchronously here is what made delete appear to do
// nothing -- the handler's HTTP client timed out long before nginx finished.
// The operator now sees the row vanish immediately; the returned prefixes go
// to nginx.PurgeQueue, which evicts them off the request path. The tradeoff
// (a failed background purge leaves bytes on disk the catalog reports as
// gone) is documented on PurgeQueue.
//
// Callers pass the returned prefixes to a PurgeQueue. A nil/empty slice with
// a nil error means there was nothing to evict (zero tracked keys and, for
// Steam, zero depots known to Steam's own API -- or a game already emptied
// by reconciliation/a prior delete) -- a no-op success, not an error.
//
// depotsForApp is optional (nil disables widening). When platform is "steam"
// and gameKey is a resolved numeric appID, its purge prefixes are unioned
// with the ones derived from cache_keys, so files a real Steam client
// downloaded but the log tailer failed to attribute still get purged (see
// DepotsForApp).
func Delete(cat *catalog.Catalog, platform, gameKey string, depotsForApp DepotsForApp) (purgePrefixes []string, err error) {
	game, found, err := cat.GetGame(platform, gameKey)
	if err != nil {
		return nil, fmt.Errorf("looking up game (%s, %s): %w", platform, gameKey, err)
	}
	if !found {
		return nil, ErrGameNotFound
	}
	if game.Monitored {
		return nil, ErrMonitored
	}

	keys, err := cat.CacheKeysForGame(platform, gameKey)
	if err != nil {
		return nil, fmt.Errorf("listing cache keys for (%s, %s): %w", platform, gameKey, err)
	}

	prefixes := nginx.PurgePrefixes(keys)
	if platform == "steam" && depotsForApp != nil {
		if appID, convErr := strconv.Atoi(gameKey); convErr == nil {
			seen := make(map[string]struct{}, len(prefixes))
			for _, p := range prefixes {
				seen[p] = struct{}{}
			}
			for _, depotID := range depotsForApp(appID) {
				p := fmt.Sprintf("steam/depot/%d/", depotID)
				if _, ok := seen[p]; ok {
					continue
				}
				seen[p] = struct{}{}
				prefixes = append(prefixes, p)
			}
		}
	}

	if len(keys) == 0 && len(prefixes) == 0 {
		// Already empty, and Steam's API (if configured) knows of no depots
		// either -- the desired postcondition already holds, so this is a
		// no-op success, not an error (KTD11's edge case).
		return nil, nil
	}

	// Clear the catalog before the eviction runs so List()/GuestGames() drop
	// the entry on the next render. Re-checked atomically: if the game was
	// locked since the check above (e.g. the operator clicked Lock while the
	// cache enforcer was evicting it), the lock wins and nothing is purged.
	deleted, err := cat.DeleteCacheKeysIfUnmonitored(platform, gameKey)
	if err != nil {
		return nil, fmt.Errorf("clearing cache keys for (%s, %s): %w", platform, gameKey, err)
	}
	if !deleted {
		return nil, ErrMonitored
	}
	return prefixes, nil
}

// PurgeOrphans sweeps cache fragments in one operation: every orphan game
// (below maxSizeBytes, not Pre-Staged, not monitored -- see
// catalog.OrphanGames) has its cache_keys rows cleared immediately, so the
// Guest list declutters at once. It returns the wildcard PURGE prefixes ONLY
// for orphans whose cached size is at least minPurgeSizeBytes -- the ones big
// enough that a full-zone scan to reclaim their bytes is worthwhile.
//
// Sub-threshold slivers (hundreds of bytes of Steam manifest chatter) are
// cleared from the catalog but deliberately NOT enqueued: a wildcard PURGE
// scans the entire cache zone and takes minutes (see nginx.PurgeQueue), which
// is never worth it to evict a few hundred bytes, and nginx's own LRU reclaims
// that trivial space anyway. This is the "declutter everything, reclaim only
// the big ones" tradeoff. The games rows are left in place so live guest
// traffic re-cached later revives the same logical entries (R16).
//
// Prefixes are computed from the (pre-deletion) snapshot before any rows are
// cleared. As with Delete, the returned slice goes to a PurgeQueue; an empty
// slice with a nil error means nothing was big enough to be worth evicting.
func PurgeOrphans(cat *catalog.Catalog, maxSizeBytes, minPurgeSizeBytes int64) (purgePrefixes []string, err error) {
	orphans, err := cat.OrphanGames(maxSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("listing orphans: %w", err)
	}

	var bigKeys []string
	for _, o := range orphans {
		if o.SizeBytes >= minPurgeSizeBytes {
			bigKeys = append(bigKeys, o.CacheKeys...)
		}
	}
	prefixes := nginx.PurgePrefixes(bigKeys)

	for _, o := range orphans {
		if err := cat.DeleteCacheKeysForGame(o.Platform, o.GameKey); err != nil {
			return nil, fmt.Errorf("clearing orphan (%s, %s): %w", o.Platform, o.GameKey, err)
		}
	}
	return prefixes, nil
}
