package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// keyLineRE matches the plaintext "KEY: <cache key>" line nginx writes at
// a fixed-ish offset near the start of every cache file, after a binary
// header (confirmed against a real cache file: the header is ~336 bytes,
// so this searches a bounded prefix rather than assuming an exact offset
// -- hardcoding the offset would couple this to nginx's exact header
// struct size/version, the same coupling the plan's own Approach text
// explicitly avoids for the cache path hashing scheme).
var keyLineRE = regexp.MustCompile(`KEY:\s*(\S+)`)

// sliceSuffixRE strips nginx's per-slice $slice_range suffix
// ("bytes=START-END", concatenated directly after the URI with no
// separator) off an on-disk KEY: value, recovering the same
// $cacheidentifier+URI string CacheKey produces from a log line -- this is
// what correlates a disk file back to its catalog row.
var sliceSuffixRE = regexp.MustCompile(`bytes=\d+-\d+$`)

const keyScanPrefixBytes = 4096

// reconcileFileKey extracts the catalog cache key from one on-disk nginx
// cache file's header.
func reconcileFileKey(path string) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, keyScanPrefixBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", false, err
	}
	buf = buf[:n]

	m := keyLineRE.FindSubmatch(buf)
	if m == nil {
		return "", false, nil
	}
	rawKey := string(m[1])
	cacheKey := sliceSuffixRE.ReplaceAllString(rawKey, "")
	return cacheKey, true, nil
}

// ReconcileResult summarizes one reconciliation pass (R15).
type ReconcileResult struct {
	FilesScanned    int
	KeysCorrected   int
	KeysPruned      int // catalog entries whose file was evicted from nginx cache
	OrphanedFiles   int // on-disk files whose key has no matching catalog row yet
	UnreadableFiles int
}

// Reconcile walks cacheRoot, sums on-disk bytes per cache key (across every
// nginx slice file sharing that key), and corrects any catalog entry whose
// recorded size has drifted from the disk total (R15). It only corrects
// existing entries -- a file whose key isn't in the catalog yet (e.g. the
// access log that would have logged it was rotated away before the tailer
// reached it) is counted as orphaned rather than speculatively catalogued;
// the log tailer remains the sole source of new library entries.
func Reconcile(cat *Catalog, cacheRoot string) (ReconcileResult, error) {
	var result ReconcileResult
	totals := make(map[string]int64)

	// Captured before the walk starts. The walk can take minutes on a large
	// cache; a game whose download completes mid-walk is ingested by the
	// tailer (its rows get last_seen_at ~now) but its files may be created
	// after the walker already passed their directory, so they won't be in
	// totals. Pruning on absence alone would delete those fresh rows and,
	// because the tailer's offset is already past those log lines, they'd
	// never be re-created -- the bytes go permanently invisible. Rows seen at
	// or after walkStart are therefore exempt from pruning (RFC3339 UTC, same
	// format as last_seen_at, so a lexical compare is correct).
	walkStart := time.Now().UTC().Format(time.RFC3339)

	err := filepath.WalkDir(cacheRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		result.FilesScanned++
		key, found, err := reconcileFileKey(path)
		if err != nil {
			result.UnreadableFiles++
			return nil
		}
		if !found {
			result.UnreadableFiles++
			return nil
		}

		info, err := d.Info()
		if err != nil {
			result.UnreadableFiles++
			return nil
		}
		totals[key] += info.Size()
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("walking %s: %w", cacheRoot, err)
	}

	for key, total := range totals {
		current, found, err := cat.CacheKeySize(key)
		if err != nil {
			return result, fmt.Errorf("reading current size for %q: %w", key, err)
		}
		if !found {
			result.OrphanedFiles++
			continue
		}
		if current != total {
			if err := cat.SetCacheKeySize(key, total); err != nil {
				return result, fmt.Errorf("correcting size for %q: %w", key, err)
			}
			result.KeysCorrected++
		}
	}

	pruned, err := cat.DeleteStaleCacheKeys(totals, walkStart)
	if err != nil {
		return result, fmt.Errorf("pruning stale keys: %w", err)
	}
	result.KeysPruned = pruned

	return result, nil
}
