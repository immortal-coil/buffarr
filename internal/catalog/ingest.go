package catalog

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/immortal-coil/buffarr/internal/dnsmasq"
	"github.com/immortal-coil/buffarr/internal/identify"
)

// SteamLookup is the subset of identify.SteamResolver the ingester needs
// -- kept as an interface so a nil/no-op implementation can stand in when
// Steam resolution isn't configured (R3's error-path: identification
// being unavailable must never block ingestion).
type SteamLookup interface {
	Resolve(depotID int) (appID int, name string, ok bool)
}

// Ingester is the "tail access log continuously, identify, catalog" loop
// (U4's goal). One Poll-classify-identify-upsert pass per RunOnce call;
// Run just ticks RunOnce on an interval until ctx is cancelled.
type Ingester struct {
	tailer    *Tailer
	platforms []dnsmasq.Platform
	steam     SteamLookup // nil if Steam resolution isn't configured
	catalog   *Catalog
}

func NewIngester(tailer *Tailer, platforms []dnsmasq.Platform, steam SteamLookup, catalog *Catalog) *Ingester {
	return &Ingester{tailer: tailer, platforms: platforms, steam: steam, catalog: catalog}
}

// RunOnce polls the tailer once and ingests every complete line it
// returns. It returns the number of lines successfully ingested.
func (in *Ingester) RunOnce() (int, error) {
	rawLines, err := in.tailer.Poll()
	if err != nil {
		return 0, err
	}

	n := 0
	// Per-day bandwidth tally for this batch (FEATURES #1), flushed once per
	// distinct day after the loop -- not per line, since the catalog is
	// single-connection and per-line transactions would be needless contention.
	bw := map[string]*bwDay{}
	for _, raw := range rawLines {
		line, err := ParseLine(raw)
		if err != nil {
			log.Printf("catalog: skipping unparseable log line: %v", err)
			continue
		}
		// Skip direct-return responses (access_log off in location /healthz,
		// CDN-disabled 403s, etc.) -- $upstream_cache_status is "-" when
		// nginx never touched the cache, so these carry no useful accounting
		// information and pollute the catalog with infrastructure noise.
		if line.UpstreamCacheStatus == "-" {
			continue
		}
		accrueBandwidth(bw, line)

		in.ingestLine(line)
		n++
	}
	for day, d := range bw {
		if err := in.catalog.AddBandwidth(day, d.totalBytes, d.totalReqs, d.hitBytes, d.hitReqs); err != nil {
			log.Printf("catalog: recording bandwidth for %s: %v", day, err)
		}
	}
	return n, nil
}

// bwDay accumulates one day's served-byte totals.
type bwDay struct {
	totalBytes, totalReqs, hitBytes, hitReqs int64
}

// accrueBandwidth adds line's contribution to bw. Lines with
// UpstreamCacheStatus == "-" must be filtered by the caller before invoking
// this (they carry no cache accounting data).
func accrueBandwidth(bw map[string]*bwDay, line LogLine) {
	day := line.Time.UTC().Format("2006-01-02")
	d := bw[day]
	if d == nil {
		d = &bwDay{}
		bw[day] = d
	}
	d.totalBytes += line.BodyBytesSent
	d.totalReqs++
	if line.UpstreamCacheStatus == "HIT" {
		d.hitBytes += line.BodyBytesSent
		d.hitReqs++
	}
}

// BackfillBandwidth reads the access log from byte 0 up to (but not including)
// the tailer's persisted offset and populates bandwidth_daily with the
// per-day totals found there. It then sets bandwidth_since to the earliest
// day written so the dashboard label is accurate.
//
// Called on startup when bandwidth_daily is empty. If the tailer offset is 0
// (fresh install), it is a no-op. The forward counter (RunOnce) owns
// [offset, ∞), so there is no double-count. All writes are committed in a
// single transaction so a mid-backfill error leaves the table empty.
func (in *Ingester) BackfillBandwidth(logPath string) error {
	offset := in.tailer.Offset()
	if offset == 0 {
		return nil
	}

	log.Printf("catalog: backfilling bandwidth from %s (up to %d bytes)", logPath, offset)

	// Guard against log rotation: if the current file's inode differs from
	// the tailer's persisted inode, the log was rotated since the last run.
	// The tailer will reset to offset 0 on its first Poll, so there is no
	// [0, offset) history in the current file to backfill.
	fi, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("backfill: stat log: %w", err)
	}
	if in.tailer.Inode() != 0 && inodeOf(fi) != in.tailer.Inode() {
		return nil
	}
	// Guard against inode reuse: if the current file is smaller than the
	// persisted offset, the log was rotated and a new file was assigned the
	// same inode. Treat this the same as a rotation (no history to backfill).
	if fi.Size() < offset {
		return nil
	}

	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("backfill: open log: %w", err)
	}
	defer f.Close()

	bw := map[string]*bwDay{}
	scanner := bufio.NewScanner(io.LimitReader(f, offset))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024) // match the uncapped forward path
	for scanner.Scan() {
		line, err := ParseLine(scanner.Text())
		if err != nil {
			continue // partial trailing line or malformed -- skip
		}
		if line.UpstreamCacheStatus == "-" {
			continue
		}
		accrueBandwidth(bw, line)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("backfill: scanning log: %w", err)
	}

	if len(bw) == 0 {
		return nil
	}

	// Find earliest day before writing so bandwidth_since is set correctly
	// even if the map iteration order is non-deterministic.
	earliest := ""
	for day := range bw {
		if earliest == "" || day < earliest {
			earliest = day
		}
	}

	return in.catalog.writeBandwidthBackfill(bw, earliest)
}

// Run ticks RunOnce every interval until ctx is cancelled. Errors from
// RunOnce (e.g. a transient stat/read failure) are logged and retried on
// the next tick rather than stopping the loop -- passive caching must keep
// working even if cataloging hiccups.
func (in *Ingester) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := in.RunOnce(); err != nil {
				log.Printf("catalog: ingest tick failed: %v", err)
			}
		}
	}
}

func (in *Ingester) ingestLine(line LogLine) {
	platform, ok := dnsmasq.ClassifyHost(line.Host, in.platforms)
	if !ok {
		// nginx only proxies allowlisted hosts (KTD3), so this should be
		// rare; fall back to the literal host as the platform bucket
		// rather than dropping the entry.
		platform = line.Host
	}

	gameKey, gameName := in.identifyGame(platform, line.URI)
	cacheKey := CacheKey(line)

	if err := in.catalog.UpsertCacheKey(platform, gameKey, gameName, cacheKey, line.BodyBytesSent, line.Time); err != nil {
		log.Printf("catalog: failed to upsert cache key %q: %v", cacheKey, err)
	}
}

func (in *Ingester) identifyGame(platform, uri string) (gameKey, gameName string) {
	switch platform {
	case "steam":
		if depotID, ok := identify.SteamDepotURI(uri); ok {
			if in.steam != nil {
				if appID, name, ok := in.steam.Resolve(depotID); ok {
					return strconv.Itoa(appID), name
				}
			}
			// Unknown depot: track per-depot so guest games stay individually
			// addressable instead of being lumped into one unidentified bucket.
			key := fmt.Sprintf("depot:%d", depotID)
			return key, fmt.Sprintf("Steam Depot %d", depotID)
		}
	case "blizzard":
		if prefix, name, ok := identify.ResolveBattlenet(uri); ok {
			return prefix, name
		}
	}
	return identify.UnidentifiedKey(platform), identify.UnidentifiedKey(platform)
}
