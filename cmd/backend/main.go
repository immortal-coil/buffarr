// Command backend is the Buffarr single binary: webui,
// catalog, lifecycle, and orchestration for the nginx/dnsmasq/prefill
// containers it manages.
package main

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"time"

	// time/tzdata embeds the IANA timezone database into the binary so a
	// TZ=<zone> env var resolves even on the Alpine runtime image, which ships
	// no /usr/share/zoneinfo. This is what lets time.Now() render in the
	// operator's local zone, not UTC.
	_ "time/tzdata"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/cdn"
	"github.com/immortal-coil/buffarr/internal/config"
	"github.com/immortal-coil/buffarr/internal/dnsmasq"
	"github.com/immortal-coil/buffarr/internal/identify"
	"github.com/immortal-coil/buffarr/internal/nginx"
	"github.com/immortal-coil/buffarr/internal/prefill"
	"github.com/immortal-coil/buffarr/internal/steamauth"
	"github.com/immortal-coil/buffarr/internal/tlscert"
	"github.com/immortal-coil/buffarr/internal/web"
)

// Ingestion is continuous background work (U4), not operator-tunable per
// the requirements (only the cache storage root, R10, is) -- these stay
// internal constants rather than env vars.
const (
	ingestPollInterval   = 2 * time.Second
	steamSyncInterval    = time.Hour
	reconcileInterval    = 30 * time.Minute
	cacheEnforceInterval = 5 * time.Minute
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := tlscert.EnsureCert(cfg.TLSCertPath, cfg.TLSKeyPath, cfg.CacheIP); err != nil {
		log.Fatalf("tls cert: %v", err)
	}

	cat, err := catalog.Open(cfg.SQLitePath)
	if err != nil {
		log.Fatalf("catalog: %v", err)
	}
	defer cat.Close()

	platforms, skipped, err := dnsmasq.LoadPlatforms()
	if err != nil {
		log.Fatalf("loading CDN platform list: %v", err)
	}
	if len(skipped) > 0 {
		log.Printf("dnsmasq: skipped %d malformed vendored domain entries: %v", len(skipped), skipped)
	}

	platformNames := make([]string, len(platforms))
	for i, p := range platforms {
		platformNames[i] = p.Name
	}
	if err := cat.SeedPlatformsIfEmpty(platformNames, []string{"steam"}); err != nil {
		log.Fatalf("seeding CDN platform toggle state: %v", err)
	}

	toggler := cdn.NewToggler(cat, platforms, cfg.CacheIP, cfg.DNSMasqAddnHostsPath, cfg.DNSMasqWildcardConfPath, cfg.NginxAllowlistPath)
	// Re-assert the persisted enabled-CDN set on every boot (R9): without
	// this, a restarted backend would leave nginx/dnsmasq running whatever
	// a previous instance (or their own bootstrap-permissive defaults,
	// KTD7) last wrote, silently ignoring the operator's persisted toggles
	// until the next manual toggle. Logged, not fatal -- nginx/dnsmasq keep
	// serving traffic on their last-known-good config either way (the
	// plan's "backend-down blast radius is graceful" property).
	if err := toggler.RegenerateFromPersisted(); err != nil {
		log.Printf("cdn: failed to re-assert persisted CDN toggle state on startup: %v", err)
	}

	tailer, err := catalog.NewTailer(cfg.NginxAccessLogPath, cat)
	if err != nil {
		log.Fatalf("catalog tailer: %v", err)
	}

	var steam *identify.SteamResolver
	if cfg.SteamWebAPIKey != "" && cfg.SteamOperatorID64 != "" {
		steam = identify.NewSteamResolver(identify.NewHTTPSteamAPI(cfg.SteamWebAPIKey), cfg.SteamOperatorID64)
		go runSteamSync(steam)
	} else {
		log.Printf("identify: STEAM_WEB_API_KEY/STEAM_OPERATOR_ID64 not set in the environment -- Steam depot resolution disabled, all Steam traffic will be Unidentified. " +
			"If these were instead entered through the webui's first-run setup form, depot resolution picks them up on the next restart, not live -- this background sync is decided once at boot (env only), unlike the webui's own dynamic env-or-DB lookup.")
	}

	ingester := catalog.NewIngester(tailer, platforms, steamLookup(steam), cat)
	ctx := context.Background()
	// Gate on bandwidth_since, not on the table being empty: "Reset bandwidth
	// stats" clears bandwidth_daily but re-stamps bandwidth_since, so an
	// empty-table check would re-run the backfill on the next restart and
	// resurrect the exact stats the operator just reset. bandwidth_since is
	// absent only before the very first backfill/accrual, which is the one
	// time we actually want to backfill.
	if since, err := cat.BandwidthSince(); err != nil {
		log.Printf("catalog: checking bandwidth state failed (skipping backfill): %v", err)
	} else if since == "" {
		if err := ingester.BackfillBandwidth(cfg.NginxAccessLogPath); err != nil {
			log.Printf("catalog: bandwidth backfill failed (non-fatal): %v", err)
		} else {
			log.Printf("catalog: bandwidth backfill complete")
		}
	}
	go ingester.Run(ctx, ingestPollInterval)
	// Reconcile scans the nginx cache zone specifically (CACHE_ROOT/cache, per
	// proxy_cache_path), not all of CACHE_ROOT -- the latter now also holds the
	// asset cache (CACHE_ROOT/assets), whose posters/JSON are not nginx cache
	// files and would otherwise be scanned and logged as "unreadable" every pass.
	go runReconcileLoop(ctx, cat, filepath.Join(cfg.CacheRoot, "cache"))

	sessions := steamauth.NewManager()
	// PurgeQueue walks the nginx cache dir directly -- faster and immune to
	// keys_zone staleness vs. the old HTTP PURGE approach (see nginx.WalkPurge).
	// Unfinished purges persist next to the catalog DB so a restart resumes them.
	purgeQueue := nginx.NewPurgeQueue(filepath.Join(cfg.CacheRoot, "cache"), filepath.Join(filepath.Dir(cfg.SQLitePath), "purge-pending.json"))
	sidecar := prefill.NewSidecar(cfg.PrefillTriggerPath, cfg.PrefillStatusPath, cfg.PrefillSelectionPath, cfg.PrefillLastRunLogPath)

	var lookupAppSize func(int) int64
	var lookupAppDepots func(int) []int
	if steam != nil {
		lookupAppSize = steam.GetSize
		lookupAppDepots = steam.DepotsForApp
	}
	webServer, err := web.NewServer(cat, sessions, toggler, sidecar, purgeQueue, cfg.SteamWebAPIKey, cfg.SteamOperatorID64, cfg.CacheRoot, cfg.CacheMaxBytes, cfg.PosterDir, cfg.CacheIP, cfg.TrustProxyHeaders, lookupAppSize, lookupAppDepots)
	if err != nil {
		log.Fatalf("web server: %v", err)
	}
	webServer.SetAPIKey(cfg.APIKey)
	// Enforce the operator's cache cap (if set) by evicting LRU unlocked games.
	go webServer.RunCacheEnforcer(ctx, cacheEnforceInterval)
	// Drain the batch-preload queue, one game at a time, through the sidecar.
	go webServer.RunPrefillWorker(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.Handle("/", webServer.Handler())

	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers, so a slow/idle connection can't hold a goroutine open forever
	// (a Slowloris-style resource leak). LAN-only, so the risk is low, but the
	// timeout is free; the rest stays unbounded since large uploads aren't a
	// concern for this webui.
	const readHeaderTimeout = 10 * time.Second

	go func() {
		log.Printf("listening (HTTPS) on %s", cfg.HTTPSAddr)
		srv := &http.Server{Addr: cfg.HTTPSAddr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
		if err := srv.ListenAndServeTLS(cfg.TLSCertPath, cfg.TLSKeyPath); err != nil {
			log.Fatalf("https server: %v", err)
		}
	}()

	log.Printf("listening (HTTP) on %s", cfg.HTTPAddr)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// steamLookup converts a possibly-nil *identify.SteamResolver into a
// possibly-nil catalog.SteamLookup. A direct assignment would wrap a nil
// *SteamResolver in a non-nil interface value (the classic Go nil-interface
// footgun), which ingest.go's `if in.steam != nil` guard would then fail to
// catch, panicking on the first Resolve call instead of degrading
// gracefully.
func steamLookup(s *identify.SteamResolver) catalog.SteamLookup {
	if s == nil {
		return nil
	}
	return s
}

// runSteamSync runs an initial sync immediately (so resolution isn't idle
// for a full steamSyncInterval after startup), then resyncs periodically.
// Sync errors are logged, not fatal -- per R3's error path, the API being
// unreachable must degrade depot resolution, not the whole backend.
func runSteamSync(steam *identify.SteamResolver) {
	if err := steam.Sync(); err != nil {
		log.Printf("identify: initial Steam sync failed: %v", err)
	}
	ticker := time.NewTicker(steamSyncInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := steam.Sync(); err != nil {
			log.Printf("identify: Steam sync failed: %v", err)
		}
	}
}

// runReconcileLoop periodically corrects catalog size drift against actual
// on-disk usage (R15).
func runReconcileLoop(ctx context.Context, cat *catalog.Catalog, cacheRoot string) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := catalog.Reconcile(cat, cacheRoot)
			if err != nil {
				log.Printf("catalog: reconcile failed: %v", err)
				continue
			}
			if result.KeysCorrected > 0 || result.KeysPruned > 0 || result.OrphanedFiles > 0 {
				log.Printf("catalog: reconcile scanned %d files, corrected %d keys, pruned %d evicted, %d orphaned, %d unreadable",
					result.FilesScanned, result.KeysCorrected, result.KeysPruned, result.OrphanedFiles, result.UnreadableFiles)
			}
		}
	}
}
