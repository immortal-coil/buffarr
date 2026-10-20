package nginx

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// prefixPatterns extract a purge-scoping prefix from a catalog cache_key
// (KTD2/KTD11): wildcarding at the depot/NGDP level evicts every
// slice-keyed sub-entry for that depot/prefix in one call, without being so
// broad it touches unrelated depots/games sharing the same CDN. A cache_key
// that matches neither pattern (the generic/Unidentified bucket, with no
// shared structural prefix to exploit) falls back to its own exact key --
// correct either way, just less batched.
var prefixPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(steam/depot/\d+/)`),
	regexp.MustCompile(`^([^/]+/tpr/[^/]+/)`),
}

// PurgePrefix returns the wildcard purge target for cacheKey: the
// depot/NGDP-level prefix if cacheKey matches one of the known structured
// platforms, or the exact cacheKey itself otherwise.
func PurgePrefix(cacheKey string) string {
	for _, re := range prefixPatterns {
		if m := re.FindStringSubmatch(cacheKey); m != nil {
			return m[1]
		}
	}
	return cacheKey
}

// PurgePrefixes reduces a game's distinct cache keys to the minimal set of
// purge prefixes that covers all of them (KTD11): batched by depot/NGDP
// prefix where structure allows it, one exact key per remaining entry.
func PurgePrefixes(cacheKeys []string) []string {
	seen := make(map[string]struct{}, len(cacheKeys))
	var prefixes []string
	for _, k := range cacheKeys {
		p := PurgePrefix(k)
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		prefixes = append(prefixes, p)
	}
	return prefixes
}

// keyLineRE and sliceSuffixRE mirror catalog/reconcile.go -- same nginx cache
// file format, same stripping logic. Kept local to avoid an nginx→catalog
// import cycle.
var (
	keyLineRE     = regexp.MustCompile(`KEY:\s*(\S+)`)
	sliceSuffixRE = regexp.MustCompile(`bytes=\d+-\d+$`)
)

const keyScanBytes = 4096

// readCacheFileKey reads the first keyScanBytes of path and returns its nginx
// cache key with the per-slice bytes=N-M suffix stripped. Returns ("", false, nil)
// if no KEY header is found.
func readCacheFileKey(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, keyScanBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", false, err
	}
	m := keyLineRE.FindSubmatch(buf[:n])
	if m == nil {
		return "", false, nil
	}
	return sliceSuffixRE.ReplaceAllString(string(m[1]), ""), true, nil
}

// WalkPurge walks cacheRoot and deletes every nginx cache file whose key
// starts with any of prefixes. Returns the number of deleted files. Per-file
// errors (unreadable header, concurrent eviction) are skipped so one bad
// file cannot abort the batch.
func WalkPurge(cacheRoot string, prefixes []string) (int, error) {
	if len(prefixes) == 0 {
		return 0, nil
	}
	var deleted int
	err := filepath.WalkDir(cacheRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		key, found, err := readCacheFileKey(path)
		if err != nil || !found {
			return nil
		}
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				if rerr := os.Remove(path); rerr == nil || os.IsNotExist(rerr) {
					deleted++
				}
				break
			}
		}
		return nil
	})
	return deleted, err
}
