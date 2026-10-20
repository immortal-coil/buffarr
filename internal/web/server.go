package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/cdn"
	"github.com/immortal-coil/buffarr/internal/config"
	"github.com/immortal-coil/buffarr/internal/lifecycle"
	"github.com/immortal-coil/buffarr/internal/prefill"
	"github.com/immortal-coil/buffarr/internal/steamauth"
)

// steamID64RE is the same 17-digit SteamID64 shape internal/steamauth
// extracts from a verified OpenID claimed_id -- used here only as a basic
// sanity check on the setup form's input, not as identity verification.
var steamID64RE = regexp.MustCompile(`^\d{17}$`)

// publicPaths and publicPrefixes are the only routes reachable without a
// session (KTD16). Every other route -- present or future -- is gated by
// construction: Handler wraps the whole mux in authGate, so a new route
// is protected automatically unless explicitly added here.
var publicPaths = map[string]bool{
	"/setup":               true,
	"/login":               true,
	"/auth/steam/login":    true,
	"/auth/steam/callback": true,
	"/favicon.ico":         true,
	"/join":                true,
}

const publicStaticPrefix = "/static/"

// publicPosterPrefix lets the public kiosk (/join) show cover art for cached
// games without a session. Posters are public Steam cover art, not sensitive.
const publicPosterPrefix = "/posters/"

// PurgeEnqueuer schedules background cache eviction for a set of wildcard
// purge prefixes. Delete is optimistic: the catalog is cleared synchronously
// and the (slow, whole-zone-scanning) PURGE is handed here to run off the
// request path. Satisfied by *nginx.PurgeQueue in production, faked in tests.
type PurgeEnqueuer interface {
	Enqueue(prefixes []string)
	// Idle reports whether the queue has no work pending or in flight. The
	// cache enforcer uses this to skip a tick while a purge is still
	// draining -- see enforceCacheMax.
	Idle() bool
}

// Server is the webui (R1, R2, R4, R6, R8, R9, R11, R12, R13). It composes
// every previously-built unit's library-level API behind authenticated,
// CSRF-protected HTTP routes -- none of U5/U6/U7/U8 had any HTTP surface
// before this.
type Server struct {
	cat        *catalog.Catalog
	sessions   *steamauth.Manager
	toggler    *cdn.Toggler
	sidecar    *prefill.Sidecar
	purgeQueue PurgeEnqueuer

	// envSteamWebAPIKey/envOperatorSteamID64 come from STEAM_WEB_API_KEY/
	// STEAM_OPERATOR_ID64 (set once at boot); they take precedence over
	// the DB-stored settings the first-run setup form writes (see
	// steamConfig). Either source, or a mix of one from each, is valid.
	envSteamWebAPIKey    string
	envOperatorSteamID64 string
	cacheRoot            string
	cacheMaxBytes        int64
	posterDir            string
	cacheIP              string // LAN IP clients resolve CDN hostnames to (cfg.CacheIP); shown on the kiosk /join page
	trustProxyHeaders    bool   // honor X-Forwarded-Proto/Host in requestBaseURL (cfg.TrustProxyHeaders)
	apiKey               string // enables the /api/v1/ JSON API when non-empty (cfg.APIKey); see authGate

	httpClient *http.Client
	templates  *template.Template

	// fetchOwnedGames defaults to steamauth.GetOwnedGames (the real Steam
	// Web API call); tests in this package substitute a fake to exercise
	// the error-path rendering without a live network call.
	fetchOwnedGames func(webAPIKey, steamID64 string) ([]steamauth.OwnedGame, error)
	// lookupAppSize returns the estimated total download size for a Steam
	// appID (sum of depot maxsize values from the last sync). Returns 0
	// when unknown. Substituted in tests.
	lookupAppSize func(appID int) int64
	// lookupAppDepots returns every depot ID Steam's API lists under an
	// appID (nil/empty when unknown or unconfigured). Passed to
	// lifecycle.Delete so it can purge depots a real Steam client cached
	// but the log tailer never attributed to this game. Substituted in
	// tests.
	lookupAppDepots func(appID int) []int

	// prefillWake nudges the prefill worker to drain the queue immediately
	// instead of waiting for its next poll tick. prefillRetryAfter is touched
	// only by the worker goroutine (no lock) to back off after a transient
	// start failure (e.g. the Steam API being briefly unreachable).
	prefillWake       chan struct{}
	prefillRetryAfter time.Time
	owned             ownedCache

	speed struct {
		mu         sync.Mutex
		pct        int
		at         time.Time
		totalBytes int64
	}

	// depotResolve negative-caches failed depot->app-name Storefront lookups
	// so a busy guest page doesn't re-hit the rate-limited Store API for the
	// same unresolved depot on every render (finding #7). Bounded by the
	// number of distinct unresolved depots in the guest library (small, and
	// single-operator), so no eviction sweep is needed. Successful
	// resolutions persist as the game name and never reach here again.
	depotResolve struct {
		mu     sync.Mutex
		failed map[int]time.Time
	}
}

// depotResolveTTL is how long a failed depot resolution is remembered before
// the guest page will try the Store API for it again -- long enough to spare
// the rate-limited endpoint on every render, short enough that a depot which
// only failed transiently (or gains a Store mapping later) still recovers.
const depotResolveTTL = time.Hour

// ownedCache memoizes the operator's owned-Steam-games list (the Preload
// library) so the hero backdrop pool doesn't re-hit the Steam Web API on every
// page load. Refreshed in the background; reads serve whatever is cached.
type ownedCache struct {
	mu         sync.Mutex
	games      []steamauth.OwnedGame
	fetchedAt  time.Time
	refreshing atomic.Bool
}

// wakePrefillWorker nudges RunPrefillWorker to drain the queue now rather than
// waiting for its next tick. Non-blocking: the buffered channel coalesces
// repeated signals into one.
func (s *Server) wakePrefillWorker() {
	select {
	case s.prefillWake <- struct{}{}:
	default:
	}
}

func NewServer(
	cat *catalog.Catalog,
	sessions *steamauth.Manager,
	toggler *cdn.Toggler,
	sidecar *prefill.Sidecar,
	purgeQueue PurgeEnqueuer,
	envSteamWebAPIKey, envOperatorSteamID64 string,
	cacheRoot string,
	cacheMaxBytes int64,
	posterDir string,
	cacheIP string,
	trustProxyHeaders bool,
	lookupAppSize func(appID int) int64,
	lookupAppDepots func(appID int) []int,
) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	if lookupAppSize == nil {
		lookupAppSize = func(int) int64 { return 0 }
	}
	if lookupAppDepots == nil {
		lookupAppDepots = func(int) []int { return nil }
	}
	s := &Server{
		cat:                  cat,
		sessions:             sessions,
		toggler:              toggler,
		sidecar:              sidecar,
		purgeQueue:           purgeQueue,
		envSteamWebAPIKey:    envSteamWebAPIKey,
		envOperatorSteamID64: envOperatorSteamID64,
		cacheRoot:            cacheRoot,
		cacheMaxBytes:        cacheMaxBytes,
		posterDir:            posterDir,
		cacheIP:              cacheIP,
		trustProxyHeaders:    trustProxyHeaders,
		httpClient:           &http.Client{Timeout: 15 * time.Second},
		templates:            tmpl,
		fetchOwnedGames:      steamauth.GetOwnedGames,
		lookupAppSize:        lookupAppSize,
		lookupAppDepots:      lookupAppDepots,
		prefillWake:          make(chan struct{}, 1),
	}
	s.depotResolve.failed = make(map[int]time.Time)
	// Reconcile a legacy pending Pre-Stage that finished while the backend was
	// down and has no queue row to drive it (e.g. a run started by a pre-queue
	// build that completed across the upgrade). The worker handles queue-backed
	// runs; this catches the orphaned-pending case so it isn't left unresolved.
	s.reconcilePrestage()
	return s, nil
}

// SetAPIKey enables the JSON API under /api/v1/ (empty leaves it disabled).
// Static-key auth for scripts and integrations that can't do Steam OpenID.
func (s *Server) SetAPIKey(key string) {
	s.apiKey = key
}

// settingsWebAPIKey and settingsOperatorID64 are the DB keys the first-run
// setup form writes to (internal/catalog's generic settings store).
// Storing the Web API key here is a deliberate, user-approved reversal of
// this project's original plan (env/secret-file only, never the catalog
// DB) -- chosen so the webui can be fully configured with no .env editing.
// It must never be logged.
const (
	settingsWebAPIKey    = "steam_web_api_key"
	settingsOperatorID64 = "steam_operator_id64"
	// settingMinGameSize holds the operator-editable floor that decides what
	// counts as a real game, stored as an nginx-style size string (e.g.
	// "100m"). Editable in Settings so changing it takes effect immediately,
	// with no redeploy (and no operator logout). See minGameSizeBytes.
	settingMinGameSize = "min_game_size"
	// settingDashboardBg toggles the random hero-art dashboard backdrop.
	// Stored as "off" to disable; unset/anything-else means on (default on).
	settingDashboardBg = "dashboard_bg"
	// settingMaxCacheSize is the operator-set cap (nginx-style size string) the
	// backend enforces by auto-evicting the least-recently-used UNLOCKED games
	// once the cache exceeds it. Unset/unparseable => no Buffarr-managed cap
	// (nginx still enforces its own max_size). Locked games are never evicted.
	settingMaxCacheSize = "max_cache_size"
	// settingEvictionPaused suspends the LRU cache evictor so nothing is
	// auto-evicted (e.g. during a LAN event). Value is an RFC3339 resume-at
	// timestamp for a timed pause, the legacy literal "on" for an indefinite
	// pause, or absent/"off"/a past timestamp for eviction-runs (default). See
	// evictionPauseUntil. nginx's own max_size still applies regardless.
	settingEvictionPaused = "eviction_paused"
)

// defaultMinGameSizeBytes is the floor used when the min_game_size setting is
// unset or unparseable -- the historical hard-coded Library floor, so a fresh
// deployment (and the test suite, which seeds no setting) behaves exactly as
// before until the operator dials it in through Settings.
const defaultMinGameSizeBytes = catalog.MinLibrarySizeBytes

// minGameSizeBytes resolves the current min-game-size floor live from the DB on
// every call, so a change made in Settings is reflected on the next request
// with no restart. It is the single cutoff that defines a "real game": passed
// to List/CachedGameCount/GuestGames so those views stay a clean partition, and
// used as the orphan threshold for "Purge orphans". Falls back to
// defaultMinGameSizeBytes when unset/unparseable.
//
// It issues its own (self-contained) query, so callers must evaluate it as a
// plain value BEFORE opening a catalog result set -- never mid-iteration of an
// open rows cursor (the catalog runs on a single connection).
// dashboardBgEnabled reports whether the random hero-art dashboard backdrop is
// on (the default). Read live from the DB so the Settings toggle takes effect
// on the next dashboard load with no restart.
func (s *Server) dashboardBgEnabled() bool {
	if v, found, err := s.cat.GetSetting(settingDashboardBg); err == nil && found {
		return v != "off"
	}
	return true
}

// evictionPauseUntil reports whether LRU eviction is currently paused and, for a
// timed pause, when it auto-resumes. The setting_eviction_paused value is either
// an RFC3339 resume-at timestamp (timed pause -- the normal case), the legacy
// literal "on" (indefinite pause, zero until), or absent/"off"/past-timestamp
// (not paused). A past timestamp auto-resumes with no cleanup job: it simply
// reads as not-paused. An unparseable value fails safe to eviction-runs.
func (s *Server) evictionPauseUntil() (until time.Time, paused bool) {
	v, found, err := s.cat.GetSetting(settingEvictionPaused)
	if err != nil || !found || v == "" || v == "off" {
		return time.Time{}, false
	}
	if v == "on" { // legacy indefinite pause
		return time.Time{}, true
	}
	t, perr := time.Parse(time.RFC3339, v)
	if perr != nil {
		log.Printf("web: unparseable %s=%q, treating as not paused", settingEvictionPaused, v)
		return time.Time{}, false
	}
	if time.Now().Before(t) {
		return t, true
	}
	return time.Time{}, false // expired -> auto-resumed
}

// evictionPaused reports whether the operator has paused LRU cache eviction.
// Read live from the DB so the change takes effect on the next enforcer tick
// with no restart. Default false (eviction runs).
func (s *Server) evictionPaused() bool {
	_, paused := s.evictionPauseUntil()
	return paused
}

// evictionResumeText renders the remaining time on a timed pause for the banner
// and Settings badge ("in 23h", "in 45m"); empty for an indefinite or inactive
// pause.
func (s *Server) evictionResumeText() string {
	until, paused := s.evictionPauseUntil()
	if !paused || until.IsZero() {
		return ""
	}
	d := time.Until(until)
	if d <= 0 {
		return ""
	}
	if h := int(d.Hours()); h >= 1 {
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("in %dh %dm", h, m)
		}
		return fmt.Sprintf("in %dh", h)
	}
	return fmt.Sprintf("in %dm", int(d.Minutes())+1)
}

// maxCacheSizeBytes is the operator-set cache cap in bytes, read live from the
// DB. 0 means "no Buffarr-managed cap" (unset or unparseable) -- eviction is
// then a no-op and only nginx's own max_size applies.
func (s *Server) maxCacheSizeBytes() int64 {
	if v, found, err := s.cat.GetSetting(settingMaxCacheSize); err == nil && found {
		return config.ParseCacheSize(v)
	}
	return 0
}

// cacheUsedBytes returns bytes used on the cache volume (Statfs), matching what
// the disk meter and nginx's own max_size measure.
//
// This is whole-filesystem usage, so the cap assumes a dedicated cache volume.
// If the volume holds other sizeable data, that space counts against the cap
// but can't be reclaimed by evicting games -- the enforcer would keep evicting
// every unlocked game each pass. Summing the catalog's bytes instead would fix
// that but drift between reconciles; a dedicated volume is the supported setup.
func (s *Server) cacheUsedBytes() (int64, bool) {
	if s.cacheRoot == "" {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cacheRoot, &st); err != nil {
		return 0, false
	}
	total := int64(st.Blocks) * st.Bsize
	if total <= 0 {
		return 0, false
	}
	return total - int64(st.Bavail)*st.Bsize, true
}

// enforceCacheMax evicts least-recently-used unlocked games until the cache is
// back under the operator's configured max. No-op when the cap is unset, the
// cache is already under it, or a preload is running (don't evict in-flight
// content). Called periodically by RunCacheEnforcer.
func (s *Server) enforceCacheMax() {
	if s.evictionPaused() {
		return
	}
	max := s.maxCacheSizeBytes()
	if max <= 0 {
		return
	}
	// Skip while a purge is still draining. evictDownTo projects used-bytes
	// down within a single tick, but a purge frees disk asynchronously over
	// minutes (whole-zone walk). If the previous tick's eviction hasn't freed
	// disk yet, cacheUsedBytes() (a live Statfs) still reads high, so without
	// this guard the enforcer would evict another LRU round every tick until
	// the walk finally catches up -- over-evicting games it didn't need to.
	if !s.purgeQueue.Idle() {
		return
	}
	if running, err := s.sidecar.IsRunning(); err == nil && running {
		return
	}
	used, ok := s.cacheUsedBytes()
	if !ok {
		return
	}
	if n := s.evictDownTo(used, max); n > 0 {
		log.Printf("cache-max: evicted %d unlocked game(s) to enforce a %s cap", n, humanBytes(max))
	}
}

// evictDownTo is the testable core of enforceCacheMax: given the current used
// bytes and the cap, it purges LRU unlocked games until the PROJECTED used
// (decremented by each evicted game's catalog size) drops under the cap. It
// projects rather than re-Statfs-ing because nginx frees disk asynchronously
// after a PURGE, so a fresh Statfs would lag and cause over-eviction. Returns
// the number of games evicted.
func (s *Server) evictDownTo(used, max int64) int {
	if max <= 0 || used <= max {
		return 0
	}
	candidates, err := s.cat.EvictionCandidates()
	if err != nil {
		log.Printf("cache-max: listing eviction candidates: %v", err)
		return 0
	}
	projected, evicted := used, 0
	for _, c := range candidates {
		if projected <= max {
			break
		}
		prefixes, err := lifecycle.Delete(s.cat, c.Platform, c.GameKey, s.lookupAppDepots)
		if err != nil {
			// ErrMonitored can't happen (candidates are unlocked); a not-found
			// race just means it's already gone. Skip either way.
			continue
		}
		s.purgeQueue.Enqueue(prefixes)
		projected -= c.SizeBytes
		evicted++
		log.Printf("cache-max: evicted %s/%s (~%s)", c.Platform, c.GameKey, humanBytes(c.SizeBytes))
	}
	return evicted
}

// RunCacheEnforcer runs enforceCacheMax on a ticker until ctx is cancelled.
func (s *Server) RunCacheEnforcer(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.enforceCacheMax()
		}
	}
}

func (s *Server) minGameSizeBytes() int64 {
	if v, found, err := s.cat.GetSetting(settingMinGameSize); err == nil && found {
		if n := config.ParseCacheSize(v); n > 0 {
			return n
		}
	}
	return defaultMinGameSizeBytes
}

// steamConfig resolves the Web API key and operator SteamID64 from
// whichever source has them: env vars first (an operator who configured
// .env at deploy time keeps working exactly as before), the DB-stored
// setup-form values otherwise. configured is true only when both values
// are present, from either source (env for one and DB for the other is
// fine).
func (s *Server) steamConfig() (webAPIKey, operatorSteamID64 string, configured bool) {
	webAPIKey = s.envSteamWebAPIKey
	operatorSteamID64 = s.envOperatorSteamID64

	if webAPIKey == "" {
		if v, found, err := s.cat.GetSetting(settingsWebAPIKey); err == nil && found {
			webAPIKey = v
		}
	}
	if operatorSteamID64 == "" {
		if v, found, err := s.cat.GetSetting(settingsOperatorID64); err == nil && found {
			operatorSteamID64 = v
		}
	}

	return webAPIKey, operatorSteamID64, webAPIKey != "" && operatorSteamID64 != ""
}

// Handler builds the full route table wrapped in the auth gate (KTD16).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /auth/steam/login", s.handleSteamLoginRedirect)
	mux.HandleFunc("GET /auth/steam/callback", s.handleSteamCallback)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /library", s.handleLibrary)
	mux.HandleFunc("GET /library/{platform}/{gameKey}", s.handleGameDetail)
	mux.HandleFunc("POST /library/{platform}/{gameKey}/monitor", s.csrf(s.handleMonitor(true)))
	mux.HandleFunc("POST /library/{platform}/{gameKey}/unmonitor", s.csrf(s.handleMonitor(false)))
	mux.HandleFunc("POST /library/{platform}/{gameKey}/delete", s.csrf(s.handleDelete))
	mux.HandleFunc("POST /library/{platform}/{gameKey}/update", s.csrf(s.handleLibraryUpdate))
	mux.HandleFunc("POST /library/bulk/lock", s.csrf(s.handleBulkLock))
	mux.HandleFunc("POST /library/bulk/unlock", s.csrf(s.handleBulkUnlock))
	mux.HandleFunc("POST /library/bulk/delete", s.csrf(s.handleBulkDelete))
	mux.HandleFunc("POST /library/bulk/update", s.csrf(s.handleBulkUpdate))
	mux.HandleFunc("GET /posters/{platform}/{gameKey}", s.handlePoster)

	mux.HandleFunc("GET /cdn", s.handleCDNPage)
	mux.HandleFunc("POST /cdn/{platform}/enable", s.csrf(s.handleCDNToggle(true)))
	mux.HandleFunc("POST /cdn/{platform}/disable", s.csrf(s.handleCDNToggle(false)))

	// The UI labels this page "Preload"; /prefill is the canonical route (the
	// sidecar is SteamPrefill). Alias /preload so the name in the UI is also a
	// working URL for bookmarks and deep links.
	mux.HandleFunc("GET /preload", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/prefill", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /prefill", s.handlePrefillPage)
	mux.HandleFunc("POST /prefill/trigger", s.csrf(s.handlePrefillTrigger))
	mux.HandleFunc("GET /prefill/status", s.handlePrefillStatus)
	mux.HandleFunc("POST /prefill/cancel", s.csrf(s.handlePrefillCancel))
	mux.HandleFunc("POST /prefill/queue/{id}/remove", s.csrf(s.handleQueueRemove))
	mux.HandleFunc("POST /prefill/queue/{id}/up", s.csrf(s.handleQueueMove(true)))
	mux.HandleFunc("POST /prefill/queue/{id}/down", s.csrf(s.handleQueueMove(false)))
	mux.HandleFunc("POST /prefill/queue/{id}/retry", s.csrf(s.handleQueueRetry))

	mux.HandleFunc("GET /settings", s.handleSettingsPage)
	mux.HandleFunc("GET /about", s.handleAbout)
	mux.HandleFunc("GET /join", s.handleJoin)
	mux.HandleFunc("POST /settings/min-game-size", s.csrf(s.handleSettingsMinGameSize))
	mux.HandleFunc("POST /settings/dashboard-bg", s.csrf(s.handleSettingsDashboardBg))
	mux.HandleFunc("POST /settings/max-cache-size", s.csrf(s.handleSettingsMaxCacheSize))
	mux.HandleFunc("POST /settings/pause-eviction", s.csrf(s.handleSettingsPauseEviction))
	mux.HandleFunc("POST /settings/reset-bandwidth", s.csrf(s.handleSettingsResetBandwidth))

	mux.HandleFunc("GET /guest", s.handleGuestPage)
	mux.HandleFunc("GET /guest/rows", s.handleGuestRows)
	mux.HandleFunc("POST /guest/purge-orphans", s.csrf(s.handleGuestPurgeOrphans))
	mux.HandleFunc("POST /guest/{platform}/{gameKey}/purge", s.csrf(s.handleGuestPurge))

	// JSON API (key-authenticated in authGate; no session, no CSRF).
	mux.HandleFunc("GET /api/v1/games", s.apiListGames)
	mux.HandleFunc("GET /api/v1/games/{platform}/{gameKey}", s.apiGetGame)
	mux.HandleFunc("POST /api/v1/games/{platform}/{gameKey}/lock", s.apiSetLock(true))
	mux.HandleFunc("POST /api/v1/games/{platform}/{gameKey}/unlock", s.apiSetLock(false))
	mux.HandleFunc("DELETE /api/v1/games/{platform}/{gameKey}", s.apiDeleteGame)
	mux.HandleFunc("GET /api/v1/stats", s.apiStats)
	mux.HandleFunc("GET /api/v1/prefill", s.apiPrefillStatus)
	mux.HandleFunc("POST /api/v1/prefill", s.apiPrefillTrigger)

	mux.Handle("GET "+publicStaticPrefix, noCacheStatic(http.StripPrefix("/static/", http.FileServerFS(mustSub(StaticFS, "static")))))
	// Browsers request /favicon.ico at the site root; point it at the embedded logo.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/logo.png", http.StatusMovedPermanently)
	})

	return s.setupGate(s.authGate(mux))
}

// setupGate runs outermost, before authGate: until both Steam settings are
// configured (env or DB, see steamConfig), every route except /setup and
// static assets redirects to /setup -- there is no usable login without
// an allowlisted operator SteamID64 to check against, so sending an
// unconfigured deployment to /login first would just be a dead end.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/setup" || r.URL.Path == "/favicon.ico" || r.URL.Path == "/join" || strings.HasPrefix(r.URL.Path, publicStaticPrefix) || strings.HasPrefix(r.URL.Path, publicPosterPrefix) || strings.HasPrefix(r.URL.Path, apiPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if _, _, configured := s.steamConfig(); !configured {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noCacheStatic forces the browser to revalidate embedded static assets on
// every load. The assets are embedded (zero modtime, so http.FileServerFS
// emits no Last-Modified/ETag), and a redeploy rewrites app.css in place at the
// same URL -- without this a browser keeps serving a stale stylesheet after a
// deploy (e.g. new poster-grid markup styled by the previous CSS, which has no
// grid rule and stacks the cards full-width). The assets are tiny and
// LAN-served, so always-revalidate is free.
func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// sessionContextKey carries the validated session through to handlers that
// need it (e.g. to render the CSRF token into a page).
type contextKey int

const sessionContextKeyVal contextKey = 0

func withSession(ctx context.Context, session steamauth.Session) context.Context {
	return context.WithValue(ctx, sessionContextKeyVal, session)
}

// sessionFrom panics if called on a request that didn't pass through
// authGate or csrf -- both always set it, so any handler reachable via
// Handler()'s mux can rely on it being present. This is deliberate: a
// handler that needs the session but somehow bypassed the gate should
// fail loudly, not silently render with a zero-value session.
func sessionFrom(ctx context.Context) steamauth.Session {
	return ctx.Value(sessionContextKeyVal).(steamauth.Session)
}

func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// authGate is the single point every request not on the public allowlist
// passes through (KTD16) -- a route is protected unless it is explicitly
// listed as public, not the other way around.
func (s *Server) authGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] || strings.HasPrefix(r.URL.Path, publicStaticPrefix) || strings.HasPrefix(r.URL.Path, publicPosterPrefix) {
			next.ServeHTTP(w, r)
			return
		}

		// The JSON API authenticates with a static key instead of a browser
		// session (scripts can't complete Steam OpenID). Checked here so /api/
		// routes stay protected by the same single gate as everything else.
		// CSRF does not apply: a browser never attaches the key header
		// cross-site, and API handlers never read the session cookie.
		if strings.HasPrefix(r.URL.Path, apiPrefix) {
			if code, msg := s.checkAPIKey(r); code != 0 {
				apiError(w, code, msg)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		session, err := s.sessions.RequireSession(r)
		if err != nil {
			if isHTMXRequest(r) {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		r = r.WithContext(withSession(r.Context(), session))
		next.ServeHTTP(w, r)
	})
}

// csrf wraps a state-changing handler, additionally requiring a valid
// per-session CSRF token (KTD15). Applied explicitly to every POST route
// above rather than folded into authGate, since CSRF only applies to
// state-changing requests, not every authenticated GET.
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.sessions.RequireCSRF(r)
		if err != nil {
			// authGate has already validated the session before this
			// middleware runs, so a failure here is specifically a stale or
			// missing CSRF token -- in practice an open tab whose token
			// predates a backend restart (sessions are in-memory and do not
			// survive one). A plain 403 is the wrong response: htmx does not
			// swap on it, so the operator's click appears to do nothing. For
			// an htmx request, ask the browser to reload instead -- the fresh
			// page carries a token matching the current session, so the next
			// click just works. No mutation ran, so a reload is also the
			// correct, harmless outcome for a genuinely forged token.
			if isHTMXRequest(r) {
				w.Header().Set("HX-Refresh", "true")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			s.writeToast(w, "error", "Request rejected: missing or invalid CSRF token.")
			return
		}
		r = r.WithContext(withSession(r.Context(), session))
		next(w, r)
	}
}

// handleIndex serves the authed home: the dashboard (status overview plus the
// largest-cached poster strip). authGate already bounces unauthenticated
// requests to /login, so anyone reaching here has a session. The other pages
// (Library/Preload/Settings) are reached from the top nav.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// "GET /" is a catch-all in net/http's mux: it matches every GET path not
	// claimed by a more specific route. Without this guard a typo'd or stale URL
	// (e.g. /preload, which is only aliased below) would silently render the
	// Dashboard with a 200 instead of a 404.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{}
	s.mergeCacheStats(data)     // CacheTotalBytes/Free/Used/UsedPct, CachedGameCount
	s.mergeBandwidthStats(data) // BandwidthSaved/FromUpstream, HitRatePct, since
	s.mergeHero(data)           // faded backdrop (fades out above the poster strip)

	// Library status: locked count/size and the largest cached games, from the
	// one List() the Library page already uses.
	if entries, err := s.cat.List(s.minGameSizeBytes()); err == nil {
		var lockedCount int
		var lockedSize int64
		for _, e := range entries {
			if e.Monitored {
				lockedCount++
				lockedSize += e.SizeBytes
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].SizeBytes > entries[j].SizeBytes })
		if len(entries) > 7 {
			entries = entries[:7]
		}
		data["LockedCount"] = lockedCount
		data["LockedSize"] = lockedSize
		data["TopGames"] = entries
	}

	// Unmanaged ("guest") cache size + count.
	if guests, err := s.cat.GuestGames(s.minGameSizeBytes()); err == nil {
		var unmanagedSize int64
		for _, g := range guests {
			unmanagedSize += g.SizeBytes
		}
		data["UnmanagedSize"] = unmanagedSize
		data["UnmanagedCount"] = len(guests)
	}

	// Live preload status (Running/RunningGameName/Progress/LastStatus/...).
	// CacheCapBytes and the gauge segments are set by mergeCacheStats above.
	s.mergePrefillStatus(data)

	s.render(w, "dashboard.html", data)
}

// mergeHero picks a random game from the operator's owned Steam library (the
// Preload list) for a page's faded hero backdrop (HeroAppID/HeroName), shared
// by the Dashboard, Library, and Preload pages. Re-rolls each load. No-op when
// the artwork background is off in Settings or the owned list isn't loaded yet.
func (s *Server) mergeHero(data map[string]any) {
	if !s.dashboardBgEnabled() {
		return
	}
	var appID, name string
	if games := s.ownedHeroGames(); len(games) > 0 {
		pick := games[rand.Intn(len(games))]
		appID, name = strconv.Itoa(pick.AppID), pick.Name
	} else if entries, err := s.cat.List(s.minGameSizeBytes()); err == nil {
		// Owned list not loaded yet (cold cache) or Steam unreachable -- fall
		// back to the cached library so a backdrop still shows immediately.
		var heroes []catalog.LibraryEntry
		for _, e := range entries {
			if e.Platform == "steam" && posterAppID.MatchString(e.GameKey) {
				heroes = append(heroes, e)
			}
		}
		if len(heroes) > 0 {
			pick := heroes[rand.Intn(len(heroes))]
			appID, name = pick.GameKey, pick.Name
		}
	}
	if appID != "" {
		data["HeroAppID"] = appID
		data["HeroName"] = name
	}
}

// ownedHeroGames returns the cached owned-Steam-games list for the hero pool. On
// a cold/stale cache it kicks off a single background refresh and returns
// whatever is cached now (possibly empty on the very first call), so a page
// render never blocks on the Steam Web API. The backdrop simply appears once
// the refresh lands.
func (s *Server) ownedHeroGames() []steamauth.OwnedGame {
	const ttl = time.Hour
	s.owned.mu.Lock()
	cached, fetchedAt := s.owned.games, s.owned.fetchedAt
	s.owned.mu.Unlock()
	if cached != nil && time.Since(fetchedAt) < ttl {
		return cached
	}
	if s.owned.refreshing.CompareAndSwap(false, true) {
		go func() {
			defer s.owned.refreshing.Store(false)
			webAPIKey, operatorSteamID64, configured := s.steamConfig()
			if !configured {
				return
			}
			games, err := s.fetchOwnedGames(webAPIKey, operatorSteamID64)
			if err != nil {
				log.Printf("hero: refreshing owned-games pool failed: %v", err)
				return
			}
			if len(games) == 0 {
				return
			}
			s.owned.mu.Lock()
			s.owned.games, s.owned.fetchedAt = games, time.Now()
			s.owned.mu.Unlock()
			log.Printf("hero: owned-games pool refreshed (%d games)", len(games))
		}()
	}
	return cached
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.sessions.RequireSession(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", map[string]any{})
}

// handleSetupPage and handleSetupSubmit are intentionally unauthenticated
// (KTD13 has nothing to check against until an operator SteamID64 exists)
// but must close once configured -- otherwise any LAN device could
// overwrite the allowlisted operator and seize control. The window this
// leaves is "first boot until first submission, whoever submits first
// becomes the operator," the same trust model routers and Pi-hole use for
// first-run setup on a LAN. An operator who wants zero such window should
// configure STEAM_WEB_API_KEY/STEAM_OPERATOR_ID64 via env instead.
func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if _, _, configured := s.steamConfig(); configured {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, "setup.html", map[string]any{})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	if _, _, configured := s.steamConfig(); configured {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		s.render(w, "setup.html", map[string]any{"Error": "Malformed request."})
		return
	}
	webAPIKey := strings.TrimSpace(r.FormValue("steam_web_api_key"))
	operatorSteamID64 := strings.TrimSpace(r.FormValue("steam_operator_id64"))

	if webAPIKey == "" || operatorSteamID64 == "" {
		s.render(w, "setup.html", map[string]any{"Error": "Both fields are required."})
		return
	}
	if !steamID64RE.MatchString(operatorSteamID64) {
		s.render(w, "setup.html", map[string]any{"Error": "That doesn't look like a SteamID64 (17 digits)."})
		return
	}

	// Never log webAPIKey -- it's a revocable credential, same handling
	// rule as if it had stayed env-only.
	if err := s.cat.SetSetting(settingsWebAPIKey, webAPIKey); err != nil {
		log.Printf("web: saving setup settings: %v", err)
		s.render(w, "setup.html", map[string]any{"Error": "Could not save settings."})
		return
	}
	if err := s.cat.SetSetting(settingsOperatorID64, operatorSteamID64); err != nil {
		log.Printf("web: saving setup settings: %v", err)
		s.render(w, "setup.html", map[string]any{"Error": "Could not save settings."})
		return
	}

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// requestBaseURL derives the OpenID return_to/realm from however the
// operator actually reached the backend (HTTP:9080 or HTTPS:9443, any LAN
// hostname/IP) rather than a fixed configured URL, since this stack has
// no single canonical address.
//
// When trustProxyHeaders is set, X-Forwarded-Proto/Host take precedence so a
// login behind a TLS-terminating reverse proxy redirects back to the public
// address instead of the backend-local one. These headers are client-supplied,
// so they are only honored under the explicit opt-in (see cfg.TrustProxyHeaders);
// otherwise a caller could spoof them to steer the OpenID return_to.
func (s *Server) requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if s.trustProxyHeaders {
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			scheme = strings.TrimSpace(strings.Split(p, ",")[0]) // may be a proxy chain "https, http"
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return scheme + "://" + host
}

func (s *Server) handleSteamLoginRedirect(w http.ResponseWriter, r *http.Request) {
	base := s.requestBaseURL(r)
	// Must stay in sync with the returnTo handleSteamCallback verifies against.
	redirectURL := steamauth.LoginRedirectURL(base+"/auth/steam/callback", base)
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) handleSteamCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "login.html", map[string]any{"Error": "Malformed callback request."})
		return
	}

	_, operatorSteamID64, _ := s.steamConfig()
	returnTo := s.requestBaseURL(r) + "/auth/steam/callback"
	sessionID, _, err := steamauth.CompleteLogin(s.httpClient, r.URL.Query(), returnTo, operatorSteamID64, s.sessions)
	if err != nil {
		log.Printf("web: steam login failed: %v", err)
		msg := "Login failed."
		if errors.Is(err, steamauth.ErrNotAllowlisted) {
			msg = "This Steam account is not the allowlisted operator account."
		}
		s.render(w, "login.html", map[string]any{"Error": msg})
		return
	}

	steamauth.SetSessionCookie(w, r, sessionID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(steamauth.SessionCookieName); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	steamauth.ClearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type libraryRowView struct {
	catalog.LibraryEntry
	CSRFToken string
}

// guestRowView is the guest-page analogue of libraryRowView: one guest cache
// entry plus the CSRF token its Purge button needs. Carrying the token per
// row (rather than only at page scope) lets handleGuestPurge re-render a
// single row on its failure path, exactly as renderLibraryRow does.
type guestRowView struct {
	catalog.LibraryEntry
	CSRFToken string
}

func sortLibraryEntries(entries []catalog.LibraryEntry, sortBy string) {
	switch sortBy {
	case "name":
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	case "age":
		sort.Slice(entries, func(i, j int) bool { return entries[i].LastSeen.After(entries[j].LastSeen) })
	default:
		sort.Slice(entries, func(i, j int) bool { return entries[i].SizeBytes > entries[j].SizeBytes })
	}
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	entries, err := s.cat.List(s.minGameSizeBytes())
	if err != nil {
		log.Printf("web: listing library: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "size"
	}
	sortLibraryEntries(entries, sortBy)

	rows := make([]libraryRowView, len(entries))
	for i, e := range entries {
		rows[i] = libraryRowView{LibraryEntry: e, CSRFToken: session.CSRFToken}
	}

	viewMode := r.URL.Query().Get("view")
	if viewMode != "list" {
		viewMode = "grid"
	}

	data := map[string]any{
		"CSRFToken": session.CSRFToken,
		"Entries":   rows,
		"Sort":      sortBy,
		"View":      viewMode,
	}
	s.mergeHero(data)
	s.render(w, "library.html", data)
}

// gameDetailView backs the per-game detail page (game.html). It is the catalog
// row plus the CSRF token its action buttons need, an optional download-size
// estimate, and optional Storefront metadata (nil when unavailable).
type gameDetailView struct {
	catalog.LibraryEntry
	CSRFToken     string
	DownloadBytes int64
	Store         *steamStoreView
}

// handleGameDetail renders the Radarr-style detail page for one cached game:
// poster, hero backdrop, Steam Storefront info, and the Lock/Preload/Delete
// actions. The game must exist in the catalog (reached by clicking a Library
// card); a direct URL to an unknown game 404s.
func (s *Server) handleGameDetail(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	platform := r.PathValue("platform")
	gameKey := r.PathValue("gameKey")

	entry, found, err := s.cat.GetGame(platform, gameKey)
	if err != nil {
		log.Printf("web: game detail (%s, %s): %v", platform, gameKey, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		// Not in the catalog -- e.g. an owned-but-uncached game reached from
		// the Preload grid. Still render a detail page for a valid Steam app
		// (Storefront info + a Preload action), but 404 anything else. The
		// synthetic entry has CacheKeyCount 0, so the template shows Preload
		// (and hides the Lock/Delete actions that only apply to cached games).
		if platform != "steam" || !posterAppID.MatchString(gameKey) {
			http.NotFound(w, r)
			return
		}
		entry = catalog.LibraryEntry{Platform: platform, GameKey: gameKey}
	}

	view := gameDetailView{LibraryEntry: entry, CSRFToken: session.CSRFToken}
	heroAppID := ""
	if platform == "steam" && posterAppID.MatchString(gameKey) {
		if id, convErr := strconv.Atoi(gameKey); convErr == nil {
			view.DownloadBytes = s.lookupAppSize(id)
		}
		if d, ok := s.gameStoreDetails(gameKey); ok {
			view.Store = &d
			if view.Name == "" {
				view.Name = d.Name
			}
		}
		if view.Name == "" {
			view.Name = "App " + gameKey
		}
		heroAppID = gameKey
	}

	data := map[string]any{
		"CSRFToken": session.CSRFToken,
		"Game":      view,
		"HeroAppID": heroAppID,
	}
	s.render(w, "game.html", data)
}

// renderGameActions re-renders the detail page's action cluster (game_actions
// fragment) after a Lock/Unlock from the detail view, mirroring how
// renderLibraryRow re-renders a grid card.
func (s *Server) renderGameActions(w http.ResponseWriter, session steamauth.Session, platform, gameKey string) {
	entry, found, err := s.cat.GetGame(platform, gameKey)
	if err != nil || !found {
		log.Printf("web: refreshing game actions (%s, %s): found=%v err=%v", platform, gameKey, found, err)
		s.writeToast(w, "error", "Could not refresh actions.")
		return
	}
	view := gameDetailView{LibraryEntry: entry, CSRFToken: session.CSRFToken}
	if err := s.templates.ExecuteTemplate(w, "game_actions", view); err != nil {
		log.Printf("web: rendering game_actions: %v", err)
	}
}

// handleMonitor returns a handler for either direction (monitor=true/
// false) -- both render the same updated row partial on success.
func (s *Server) handleMonitor(monitor bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		platform := r.PathValue("platform")
		gameKey := r.PathValue("gameKey")

		var err error
		if monitor {
			err = lifecycle.Monitor(s.cat, platform, gameKey)
		} else {
			err = lifecycle.Unmonitor(s.cat, platform, gameKey)
		}
		if err != nil {
			log.Printf("web: monitor=%v for (%s, %s): %v", monitor, platform, gameKey, err)
			s.writeToast(w, "error", "Could not update lock status.")
		}

		// Always falls through to render the actual current state --
		// unchanged if the mutation above failed, updated if it succeeded --
		// alongside any toast just written, so the target is never blanked by
		// a toast-only response (htmx OOB swaps only extract the OOB
		// element; the rest of the body is what fills hx-target).
		// view=detail comes from the game detail page, which swaps its action
		// cluster rather than a grid row.
		if r.URL.Query().Get("view") == "detail" {
			s.renderGameActions(w, session, platform, gameKey)
			return
		}
		s.renderLibraryRow(w, session, platform, gameKey)
	}
}

// handleDelete covers R6, R7, KTD12 (monitored entries are hard-rejected),
// and the plan's idempotent-stale-tab edge case: a not-found game (already
// deleted by another tab) renders as a no-op success, not an error.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	platform := r.PathValue("platform")
	gameKey := r.PathValue("gameKey")

	prefixes, err := lifecycle.Delete(s.cat, platform, gameKey, s.lookupAppDepots)
	switch {
	case err == nil, errors.Is(err, lifecycle.ErrGameNotFound):
		// ErrGameNotFound means a stale tab raced another delete (or
		// reconciliation already zeroed it) -- the desired postcondition
		// ("this game's files are gone") already holds, so render it the
		// same as success rather than an error. The catalog is already
		// cleared (optimistic delete); hand the actual eviction to the
		// background queue so the slow whole-zone PURGE never blocks this
		// response. prefixes is empty on the not-found/zero-key paths.
		s.purgeQueue.Enqueue(prefixes)
	case errors.Is(err, lifecycle.ErrMonitored):
		// The UI already disables the Delete button while monitored
		// (library_card), so reaching this means a stale tab raced an
		// Unmonitor->Monitor by someone else -- explain it, don't just fail.
		s.writeToast(w, "error", "This game is locked; unlock it before deleting.")
	default:
		log.Printf("web: deleting (%s, %s): %v", platform, gameKey, err)
		s.writeToast(w, "error", "Delete failed; no files were removed.")
	}
	// The detail page has no grid row to swap: on a successful delete the game
	// is gone, so send the browser back to the Library; on a failure the toast
	// is already written and we re-render the (unchanged) action cluster.
	if r.URL.Query().Get("view") == "detail" {
		if err == nil || errors.Is(err, lifecycle.ErrGameNotFound) {
			w.Header().Set("HX-Redirect", "/library")
			return
		}
		s.renderGameActions(w, session, platform, gameKey)
		return
	}
	s.renderLibraryRow(w, session, platform, gameKey)
}

// handleLibraryUpdate enqueues a cached game for prefill so updated chunks
// are fetched and cached without deleting and redownloading the whole game.
func (s *Server) handleLibraryUpdate(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	gameKey := r.PathValue("gameKey")

	if platform != "steam" {
		s.writeToast(w, "error", "Update is only supported for Steam games.")
		return
	}

	entry, found, err := s.cat.GetGame(platform, gameKey)
	if err != nil {
		log.Printf("web: looking up game for update (%s, %s): %v", platform, gameKey, err)
		s.writeToast(w, "error", "Could not queue update.")
		return
	}
	if !found {
		s.writeToast(w, "error", "Game not found.")
		return
	}

	added, err := s.cat.EnqueuePrefill(platform, gameKey, entry.Name)
	if err != nil {
		log.Printf("web: enqueueing update for (%s, %s): %v", platform, gameKey, err)
		s.writeToast(w, "error", "Could not queue update.")
		return
	}
	if !added {
		s.writeToast(w, "success", entry.Name+" is already in the queue.")
		return
	}
	s.wakePrefillWorker()
	s.writeToast(w, "success", "Queued "+entry.Name+" for update.")
}

// parseBulkGames decodes "game" form values of the shape "platform:gameKey"
// sent by the library list view's bulk action form.
func parseBulkGames(form map[string][]string) [][2]string {
	var out [][2]string
	for _, val := range form["game"] {
		if i := strings.IndexByte(val, ':'); i > 0 && i < len(val)-1 {
			out = append(out, [2]string{val[:i], val[i+1:]})
		}
	}
	return out
}

// renderListTbody re-renders just the <tr> rows for the library list view,
// used as the main swap target after bulk lock/unlock/delete. The OOB toast
// written before this call handles the user-visible result message.
func (s *Server) renderListTbody(w http.ResponseWriter, session steamauth.Session, sortBy string) {
	entries, err := s.cat.List(s.minGameSizeBytes())
	if err != nil {
		log.Printf("web: listing library for list tbody: %v", err)
		fmt.Fprintf(w, `<tr><td colspan="5">Error loading — reload the page.</td></tr>`)
		return
	}
	sortLibraryEntries(entries, sortBy)
	rows := make([]libraryRowView, len(entries))
	for i, e := range entries {
		rows[i] = libraryRowView{LibraryEntry: e, CSRFToken: session.CSRFToken}
	}
	if err := s.templates.ExecuteTemplate(w, "library_list_rows", rows); err != nil {
		log.Printf("web: rendering library_list_rows: %v", err)
	}
}

func (s *Server) handleBulkLock(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		return
	}
	games := parseBulkGames(r.Form)
	if len(games) == 0 {
		s.writeToast(w, "error", "No games selected.")
		return
	}
	var locked int
	for _, g := range games {
		if err := lifecycle.Monitor(s.cat, g[0], g[1]); err != nil {
			log.Printf("web: bulk lock (%s, %s): %v", g[0], g[1], err)
		} else {
			locked++
		}
	}
	if locked == 0 {
		s.writeToast(w, "error", "Could not lock any games.")
		s.renderListTbody(w, session, r.URL.Query().Get("sort"))
		return
	}
	msg := fmt.Sprintf("Locked %d game", locked)
	if locked != 1 {
		msg += "s"
	}
	s.writeToast(w, "success", msg+".")
	s.renderListTbody(w, session, r.URL.Query().Get("sort"))
}

func (s *Server) handleBulkUnlock(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		return
	}
	games := parseBulkGames(r.Form)
	if len(games) == 0 {
		s.writeToast(w, "error", "No games selected.")
		return
	}
	var unlocked int
	for _, g := range games {
		if err := lifecycle.Unmonitor(s.cat, g[0], g[1]); err != nil {
			log.Printf("web: bulk unlock (%s, %s): %v", g[0], g[1], err)
		} else {
			unlocked++
		}
	}
	if unlocked == 0 {
		s.writeToast(w, "error", "Could not unlock any games.")
		s.renderListTbody(w, session, r.URL.Query().Get("sort"))
		return
	}
	msg := fmt.Sprintf("Unlocked %d game", unlocked)
	if unlocked != 1 {
		msg += "s"
	}
	s.writeToast(w, "success", msg+".")
	s.renderListTbody(w, session, r.URL.Query().Get("sort"))
}

func (s *Server) handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		return
	}
	games := parseBulkGames(r.Form)
	if len(games) == 0 {
		s.writeToast(w, "error", "No games selected.")
		return
	}
	var deleted, skipped int
	for _, g := range games {
		prefixes, err := lifecycle.Delete(s.cat, g[0], g[1], s.lookupAppDepots)
		switch {
		case err == nil:
			s.purgeQueue.Enqueue(prefixes)
			deleted++
		case errors.Is(err, lifecycle.ErrGameNotFound):
			// already gone — silent no-op
		case errors.Is(err, lifecycle.ErrMonitored):
			skipped++
		default:
			log.Printf("web: bulk delete (%s, %s): %v", g[0], g[1], err)
		}
	}
	msg := fmt.Sprintf("Deleted %d game", deleted)
	if deleted != 1 {
		msg += "s"
	}
	if skipped > 0 {
		msg += fmt.Sprintf("; skipped %d locked", skipped)
	}
	s.writeToast(w, "success", msg+".")
	s.renderListTbody(w, session, r.URL.Query().Get("sort"))
}

func (s *Server) handleBulkUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		return
	}
	games := parseBulkGames(r.Form)
	if len(games) == 0 {
		s.writeToast(w, "error", "No games selected.")
		return
	}
	var queued, already int
	for _, g := range games {
		if g[0] != "steam" {
			continue
		}
		entry, found, err := s.cat.GetGame(g[0], g[1])
		if err != nil || !found {
			continue
		}
		added, err := s.cat.EnqueuePrefill(g[0], g[1], entry.Name)
		if err != nil {
			log.Printf("web: bulk update enqueue (%s, %s): %v", g[0], g[1], err)
			continue
		}
		if added {
			queued++
		} else {
			already++
		}
	}
	s.wakePrefillWorker()
	switch {
	case queued == 0 && already == 0:
		s.writeToast(w, "error", "No Steam games in selection.")
	case queued == 0:
		s.writeToast(w, "success", "Already in the queue.")
	default:
		msg := fmt.Sprintf("Queued %d game", queued)
		if queued != 1 {
			msg += "s"
		}
		msg += " for update."
		s.writeToast(w, "success", msg)
	}
}

func (s *Server) renderLibraryRow(w http.ResponseWriter, session steamauth.Session, platform, gameKey string) {
	entry, found, err := s.cat.GetGame(platform, gameKey)
	if err != nil {
		log.Printf("web: getting game (%s, %s): %v", platform, gameKey, err)
		s.writeToast(w, "error", "Could not refresh row.")
		s.writeFallbackRow(w, "row-"+platform+"-"+gameKey)
		return
	}
	// !found covers a game row that never existed; CacheKeyCount == 0
	// covers the normal post-delete case (DeleteCacheKeysForGame removes
	// the cache_keys rows but the games row persists for R16's "revives
	// the same logical entry" property). List() (the full-page view) hides
	// both shapes via its INNER JOIN, so a partial swap that left a
	// visible 0-byte row here would show something a reload immediately
	// makes disappear -- treat them the same way for consistency. (No size
	// floor here: a row only renders if it was visible enough to click,
	// which means it already cleared List()'s HAVING threshold.)
	if !found || entry.CacheKeyCount == 0 {
		// Deleted/gone -- remove the card from the DOM.
		w.Write([]byte(`<div id="row-` + html.EscapeString(platform) + "-" + html.EscapeString(gameKey) + `" style="display:none"></div>`))
		return
	}
	row := libraryRowView{LibraryEntry: entry, CSRFToken: session.CSRFToken}
	if err := s.templates.ExecuteTemplate(w, "library_card", row); err != nil {
		log.Printf("web: rendering library_card: %v", err)
	}
}

// posterAppID is the strict allowlist for the poster path's gameKey: a bare
// Steam appID (digits only, bounded length). It is the ONLY thing
// interpolated into the Steam CDN URL, so it rules out SSRF and path
// traversal -- a non-matching key just 404s and the card falls back to a
// placeholder tile.
var posterAppID = regexp.MustCompile(`^[0-9]{1,8}$`)

// errNoPosterArt means the CDN definitively has no cover art for the app
// (every candidate file returned 4xx or a non-image response). Only this
// warrants caching a .miss; transient failures are retried. Mirrors
// errNoStoreData in steamstore.go.
var errNoPosterArt = errors.New("poster: no art published for app")

// handlePoster serves cached Steam cover art for a library card, lazily
// fetching it from Steam's public CDN on first request and storing it under
// PosterDir/steam/<appid>.jpg. Only platform=steam with a numeric appID is
// served; everything else 404s (the card's <img onerror> then shows the
// name-tile fallback). A fetch miss is remembered with a sibling .miss marker
// so an art-less game isn't re-fetched on every page load.
func (s *Server) handlePoster(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	gameKey := r.PathValue("gameKey")
	if s.posterDir == "" || platform != "steam" || !posterAppID.MatchString(gameKey) {
		http.NotFound(w, r)
		return
	}

	// kind=hero serves the wide cinematic library_hero art (the dashboard
	// backdrop); the default is the 2:3 portrait poster (library grid cards).
	variant, cdnFiles := ".jpg", []string{"library_600x900.jpg", "header.jpg"}
	if r.URL.Query().Get("kind") == "hero" {
		variant, cdnFiles = ".hero.jpg", []string{"library_hero.jpg", "header.jpg"}
	}

	dir := filepath.Join(s.posterDir, "steam")
	jpg := filepath.Join(dir, gameKey+variant)
	miss := filepath.Join(dir, gameKey+variant+".miss")

	if _, err := os.Stat(jpg); err == nil {
		w.Header().Set("Cache-Control", "public, max-age=604800") // a week; cover art is effectively static
		http.ServeFile(w, r, jpg)
		return
	}
	if _, err := os.Stat(miss); err == nil {
		http.NotFound(w, r) // known art-less -- don't re-hit the CDN
		return
	}

	// Auth-gate the CDN fetch. Cached art (above) is served publicly so the
	// kiosk/join page works, but only an authenticated operator can trigger a
	// fresh fetch and .miss write. Otherwise any LAN device could drive fetches
	// for up to 10^8 appID keys -- an unbounded disk-fill vector (SSRF-safe via
	// the strict appID allowlist + 8 MiB cap, but still uncontrolled writes).
	// Guests see whatever the operator has already populated by browsing.
	if _, err := s.sessions.RequireSession(r); err != nil {
		http.NotFound(w, r)
		return
	}

	if err := s.fetchPoster(gameKey, dir, jpg, cdnFiles); err != nil {
		// Only remember a .miss when the CDN definitively has no art for this
		// app (all candidates 4xx / non-image). A transient failure -- DNS
		// hiccup, timeout, 5xx -- must NOT be cached, or one blip permanently
		// blanks the poster (the .miss short-circuits every future load).
		// Mirrors the errNoStoreData handling in steamstore.go.
		if errors.Is(err, errNoPosterArt) {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(miss, nil, 0o644)
		} else {
			log.Printf("web: poster fetch for steam/%s%s failed transiently (not cached): %v", gameKey, variant, err)
		}
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=604800")
	http.ServeFile(w, r, jpg)
}

// fetchPoster downloads the portrait library art for a Steam appID (falling
// back to the wide header image when the portrait isn't published) and writes
// it to jpg via a temp-file rename so a serve never sees a half-written file.
func (s *Server) fetchPoster(appID, dir, jpg string, cdnFiles []string) error {
	const cdn = "https://cdn.cloudflare.steamstatic.com/steam/apps/"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var lastErr error
	// allDefinitive stays true only while every failure is a definitive "no
	// such art" (4xx or a non-image 200). Any network error, 5xx, or mid-
	// download I/O failure flips it: those are transient and must not be
	// cached as a permanent miss (see handlePoster / errNoPosterArt).
	allDefinitive := true
	for _, name := range cdnFiles {
		url := cdn + appID + "/" + name
		resp, err := s.httpClient.Get(url)
		if err != nil {
			lastErr = err
			allDefinitive = false
			continue
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: status %d", url, resp.StatusCode)
			if resp.StatusCode >= 500 {
				allDefinitive = false // server-side blip, may recover
			}
			continue
		}
		tmp, err := os.CreateTemp(dir, appID+".*.tmp")
		if err != nil {
			resp.Body.Close()
			return err
		}
		// Cap the copy so a misbehaving/oversized response can't fill the volume.
		// Read one byte past the cap: hitting it means the art was cut short,
		// and a truncated image must not be cached as the poster for good.
		const maxPoster = 8 << 20
		var n int64
		n, err = io.Copy(tmp, io.LimitReader(resp.Body, maxPoster+1))
		if err == nil && n > maxPoster {
			err = fmt.Errorf("%s: exceeds %d bytes", url, maxPoster)
		}
		resp.Body.Close()
		tmp.Close()
		if err != nil {
			os.Remove(tmp.Name())
			lastErr = err
			allDefinitive = false
			continue
		}
		return os.Rename(tmp.Name(), jpg)
	}
	if allDefinitive {
		return errNoPosterArt
	}
	return lastErr
}

type cdnRowView struct {
	cdn.PlatformStatus
	CSRFToken string
}

// handleCDNPage formerly rendered a standalone Platforms page; that control
// now lives on the Settings page. The route is kept as a redirect so existing
// bookmarks (and the auth/setup gates that enumerate it) keep working.
func (s *Server) handleCDNPage(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleCDNToggle covers U6's integration requirement: a toggle made via
// the webui round-trips through the real Toggler (which itself reverts
// both layers on partial failure) and the row reflects the resulting
// state -- including the partial-failure case, which renders as an error
// toast plus the row's actual (reverted) state, never a false "enabled."
func (s *Server) handleCDNToggle(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		platform := r.PathValue("platform")

		if err := s.toggler.Toggle(platform, enabled); err != nil {
			log.Printf("web: toggling %s=%v: %v", platform, enabled, err)
			s.writeToast(w, "error", "Toggle failed; platform state unchanged.")
		}

		statuses, err := s.toggler.ListPlatforms()
		if err != nil {
			log.Printf("web: re-listing CDN platforms after toggle: %v", err)
			s.writeToast(w, "error", "Could not refresh platform state.")
			s.writeFallbackRow(w, "cdn-row-"+platform)
			return
		}
		for _, st := range statuses {
			if st.Name == platform {
				row := cdnRowView{PlatformStatus: st, CSRFToken: session.CSRFToken}
				if err := s.templates.ExecuteTemplate(w, "cdn_row", row); err != nil {
					log.Printf("web: rendering cdn_row: %v", err)
				}
				return
			}
		}
		s.writeFallbackRow(w, "cdn-row-"+platform)
	}
}

// prestageGame is one row in a Pre-Stage platform section.
type prestageGame struct {
	AppID       int
	Name        string
	CachedBytes int64 // 0 if not yet in the catalog
	TotalBytes  int64 // estimated full download size (sum of depot maxsizes); 0 if unknown
}

// prestageSection is one collapsible platform block on the Pre-Stage page.
type prestageSection struct {
	Platform string
	Games    []prestageGame
	Error    string // non-empty when fetching the game list failed
}

func (s *Server) handlePrefillPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	data := map[string]any{
		"CSRFToken": session.CSRFToken,
	}

	// Build a cached-size lookup from the catalog (steam platform only for now).
	cachedBytes := map[string]int64{}
	if entries, err := s.cat.List(s.minGameSizeBytes()); err == nil {
		for _, e := range entries {
			if e.Platform == "steam" {
				cachedBytes[e.GameKey] = e.SizeBytes
			}
		}
	}

	// Only show platforms that are enabled AND have Pre-Stage support.
	// Currently only Steam has a sidecar; others are placeholders for later.
	platforms, _ := s.toggler.ListPlatforms()
	enabledPlatforms := map[string]bool{}
	for _, p := range platforms {
		if p.Enabled {
			enabledPlatforms[p.Name] = true
		}
	}

	var sections []prestageSection
	if enabledPlatforms["steam"] {
		webAPIKey, operatorSteamID64, _ := s.steamConfig()
		games, err := s.fetchOwnedGames(webAPIKey, operatorSteamID64)
		sec := prestageSection{Platform: "Steam"}
		if err != nil {
			log.Printf("web: fetching owned games: %v", err)
			sec.Error = "Steam's Web API is currently unreachable."
		} else {
			sort.Slice(games, func(i, j int) bool { return games[i].Name < games[j].Name })
			sec.Games = make([]prestageGame, len(games))
			for i, g := range games {
				sec.Games[i] = prestageGame{
					AppID:       g.AppID,
					Name:        g.Name,
					CachedBytes: cachedBytes[strconv.Itoa(g.AppID)],
					TotalBytes:  s.lookupAppSize(g.AppID),
				}
			}
		}
		sections = append(sections, sec)
	}
	data["Sections"] = sections

	s.mergePrefillStatus(data)
	s.mergeHero(data)
	s.render(w, "prefill.html", data)
}

// handleAbout serves the static About page (project blurb + FAQ). render()
// injects the topbar disk readout the shared nav needs.
func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	s.render(w, "about.html", map[string]any{})
}

// handleJoin serves the public attendee how-to (FEATURES #8): point your
// device's DNS at the cache. Unauthenticated by design -- attendees reach it
// before they're configured, so it's allowlisted in setupGate + authGate. The
// only dynamic value is the cache server's LAN IP (BUFFARR_IP).
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"CacheIP": s.cacheIP}
	// Show the cached library so attendees can see what's ready to install fast.
	if games, err := s.cat.List(s.minGameSizeBytes()); err != nil {
		log.Printf("web: listing cached games for kiosk: %v", err)
	} else {
		data["Games"] = games
	}
	s.render(w, "join.html", data)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	// Platform enable/disable lives on Settings (the standalone Platforms page
	// was folded in here). The toggle buttons still POST to /cdn/{platform}/...
	// and swap the cdn_row partial in place.
	statuses, err := s.toggler.ListPlatforms()
	if err != nil {
		log.Printf("web: listing platforms for settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]cdnRowView, len(statuses))
	for i, st := range statuses {
		rows[i] = cdnRowView{PlatformStatus: st, CSRFToken: session.CSRFToken}
	}

	// MinGameSizeRaw is the operator's own stored string (e.g. "100m"), so the
	// input round-trips exactly what they typed; empty when unset (the field
	// then shows a placeholder and the default applies). MinGameSizeHuman is the
	// effective floor in human units for the "Current:" readout.
	rawMin, _, _ := s.cat.GetSetting(settingMinGameSize)
	rawMax, _, _ := s.cat.GetSetting(settingMaxCacheSize)

	usedStr := "unknown"
	if u, ok := s.cacheUsedBytes(); ok {
		usedStr = humanBytes(u)
	}
	capBytes := s.maxCacheSizeBytes()
	capStr := "no cap"
	if capBytes > 0 {
		capStr = humanBytes(capBytes)
	}
	// A cap larger than the physical disk can never be reached, so Buffarr would
	// never evict on its own -- warn the operator that the number is a no-op.
	capExceedsDisk := false
	diskTotalHuman := ""
	if total, ok := s.cacheDiskTotalBytes(); ok && capBytes > total {
		capExceedsDisk = true
		diskTotalHuman = humanBytes(total)
	}

	s.render(w, "settings.html", map[string]any{
		"CSRFToken":          session.CSRFToken,
		"SteamAuthenticated": s.sidecar.SteamAuthenticated(),
		"Platforms":          rows,
		"MinGameSizeRaw":     rawMin,
		"MinGameSizeHuman":   humanBytes(s.minGameSizeBytes()),
		"DashboardBgEnabled": s.dashboardBgEnabled(),
		"MaxCacheSizeRaw":    rawMax,
		"MaxCacheSizeStatus": "Using " + usedStr + " of " + capStr,
		"CapExceedsDisk":     capExceedsDisk,
		"DiskTotalHuman":     diskTotalHuman,
		"EvictionPaused":     s.evictionPaused(),
		"EvictionResumeText": s.evictionResumeText(),
	})
}

// handleSettingsPauseEviction sets a timed eviction pause (for=24h|72h) or
// resumes now (for=resume) and re-renders the control in place. While paused,
// RunCacheEnforcer (enforceCacheMax) skips eviction entirely; a timed pause
// auto-resumes when its window elapses (see evictionPauseUntil).
func (s *Server) handleSettingsPauseEviction(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var value string
	switch r.FormValue("for") {
	case "resume":
		value = "off"
	case "24h":
		value = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	case "72h":
		value = time.Now().Add(72 * time.Hour).Format(time.RFC3339)
	default:
		s.writeToast(w, "error", "Unknown pause duration.")
		return
	}
	if err := s.cat.SetSetting(settingEvictionPaused, value); err != nil {
		log.Printf("web: setting %s=%s: %v", settingEvictionPaused, value, err)
		s.writeToast(w, "error", "Could not update the eviction setting.")
	}
	if err := s.templates.ExecuteTemplate(w, "pause_eviction_control", map[string]any{
		"CSRFToken":          session.CSRFToken,
		"EvictionPaused":     s.evictionPaused(),
		"EvictionResumeText": s.evictionResumeText(),
	}); err != nil {
		log.Printf("web: rendering pause_eviction_control: %v", err)
	}
}

// handleSettingsResetBandwidth zeroes the cache-performance tally (FEATURES #1)
// and restarts the "since" clock. The dashboard reflects it on next load; here
// we just confirm with a toast.
func (s *Server) handleSettingsResetBandwidth(w http.ResponseWriter, r *http.Request) {
	if err := s.cat.ResetBandwidth(); err != nil {
		log.Printf("web: reset bandwidth: %v", err)
		s.writeToast(w, "error", "Could not reset the stats.")
		return
	}
	s.writeToast(w, "success", "Cache performance stats reset.")
}

// handleSettingsDashboardBg flips the dashboard-backdrop setting and re-renders
// the toggle control in place.
func (s *Server) handleSettingsDashboardBg(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	next := "off"
	if !s.dashboardBgEnabled() {
		next = "on"
	}
	if err := s.cat.SetSetting(settingDashboardBg, next); err != nil {
		log.Printf("web: setting %s=%s: %v", settingDashboardBg, next, err)
		s.writeToast(w, "error", "Could not update the dashboard setting.")
	}
	if err := s.templates.ExecuteTemplate(w, "dashboard_bg_control", map[string]any{
		"CSRFToken":          session.CSRFToken,
		"DashboardBgEnabled": s.dashboardBgEnabled(),
	}); err != nil {
		log.Printf("web: rendering dashboard_bg_control: %v", err)
	}
}

// handleSettingsMinGameSize persists the operator-editable min-game-size floor
// (the single cutoff used by the Library, CachedGameCount, GuestGames, and
// "Purge orphans"). It is htmx-driven like the platform toggles: a success/
// error toast plus a re-rendered "Current:" status swapped into the form, no
// full page reload and -- unlike the old MIN_GAME_SIZE env var -- no redeploy.
func (s *Server) handleSettingsMinGameSize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		s.writeMinGameSizeStatus(w)
		return
	}
	raw := strings.TrimSpace(r.FormValue("min_game_size"))
	if raw == "" || config.ParseCacheSize(raw) <= 0 {
		s.writeToast(w, "error", "Enter a size like 100m, 2g, or 500k.")
		s.writeMinGameSizeStatus(w)
		return
	}
	if err := s.cat.SetSetting(settingMinGameSize, raw); err != nil {
		log.Printf("web: saving min game size: %v", err)
		s.writeToast(w, "error", "Could not save setting.")
		s.writeMinGameSizeStatus(w)
		return
	}
	s.writeToast(w, "success", "Minimum game size updated.")
	s.writeMinGameSizeStatus(w)
}

// handleSettingsMaxCacheSize saves the cache cap (empty value clears it, i.e.
// "no Buffarr cap"). Mirrors handleSettingsMinGameSize. The cap takes effect on
// the next enforcer tick.
func (s *Server) handleSettingsMaxCacheSize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		s.writeMaxCacheSizeStatus(w)
		return
	}
	raw := strings.TrimSpace(r.FormValue("max_cache_size"))
	// An empty value clears the cap (disables Buffarr eviction); a non-empty
	// value must parse to a positive size.
	if raw != "" && config.ParseCacheSize(raw) <= 0 {
		s.writeToast(w, "error", "Enter a size like 200g, 1t, or leave blank for no cap.")
		s.writeMaxCacheSizeStatus(w)
		return
	}
	if err := s.cat.SetSetting(settingMaxCacheSize, raw); err != nil {
		log.Printf("web: saving max cache size: %v", err)
		s.writeToast(w, "error", "Could not save setting.")
		s.writeMaxCacheSizeStatus(w)
		return
	}
	if raw == "" {
		s.writeToast(w, "success", "Cache cap cleared (no Buffarr eviction).")
	} else {
		s.writeToast(w, "success", "Cache cap updated.")
	}
	s.writeMaxCacheSizeStatus(w)
}

// writeMaxCacheSizeStatus renders the "Current:" readout for the cache-cap form.
func (s *Server) writeMaxCacheSizeStatus(w http.ResponseWriter) {
	used := "unknown"
	if u, ok := s.cacheUsedBytes(); ok {
		used = humanBytes(u)
	}
	cap := "no cap"
	if m := s.maxCacheSizeBytes(); m > 0 {
		cap = humanBytes(m)
	}
	w.Write([]byte("Using " + used + " of " + cap))
}

// writeMinGameSizeStatus writes the "Current:" readout fragment that
// handleSettingsMinGameSize swaps into #min-game-size-status, reflecting the
// now-effective floor. humanBytes output is plain ASCII (no HTML-significant
// characters), so it needs no escaping.
func (s *Server) writeMinGameSizeStatus(w http.ResponseWriter) {
	w.Write([]byte("Current: " + humanBytes(s.minGameSizeBytes())))
}

func (s *Server) handleGuestPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	rows := s.guestRowViews(session)
	s.render(w, "guest.html", map[string]any{
		"CSRFToken": session.CSRFToken,
		"Entries":   rows,
	})
}

// handleGuestRows serves just the Unmanaged cache section ("buffer_section")
// as a fragment. The Library page (handleLibrary) lazy-loads this with
// hx-trigger="load" so the per-depot Steam Store name resolution in
// guestRowViews never blocks that landing page's render. Hitting GET /guest
// directly still renders the same section inside a full frame.
func (s *Server) handleGuestRows(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	rows := s.guestRowViews(session)
	if err := s.templates.ExecuteTemplate(w, "buffer_section", map[string]any{
		"CSRFToken": session.CSRFToken,
		"Entries":   rows,
	}); err != nil {
		log.Printf("web: rendering buffer_section: %v", err)
	}
}

// guestRowViews fetches the guest cache entries and lazily resolves any raw
// depot IDs to game names via the Steam Store (persisting each resolution so
// later loads skip the round-trip). Shared by the full /guest page and the
// Library page's lazy unmanaged-cache fragment so both resolve names identically.
func (s *Server) guestRowViews(session steamauth.Session) []guestRowView {
	entries, err := s.cat.GuestGames(s.minGameSizeBytes())
	if err != nil {
		log.Printf("web: guest games: %v", err)
	}
	for i, e := range entries {
		if !strings.HasPrefix(e.GameKey, "depot:") {
			continue
		}
		if !strings.HasPrefix(e.Name, "Steam Depot ") {
			continue // already resolved
		}
		depotID, err := strconv.Atoi(strings.TrimPrefix(e.GameKey, "depot:"))
		if err != nil {
			continue
		}
		// Skip depots whose Store lookup recently failed: retrying every
		// render would serialize multiple 15s Store calls into the request
		// and can 429 the rate-limited endpoint. The raw "Steam Depot N"
		// name renders meanwhile (finding #7).
		if s.depotResolveRecentlyFailed(depotID) {
			continue
		}
		if name := s.resolveDepotAppName(depotID); name != "" {
			entries[i].Name = name
			if err := s.cat.UpdateGameName(e.Platform, e.GameKey, name); err != nil {
				log.Printf("web: updating guest game name: %v", err)
			}
		} else {
			s.markDepotResolveFailed(depotID)
		}
	}
	rows := make([]guestRowView, len(entries))
	for i, e := range entries {
		rows[i] = guestRowView{LibraryEntry: e, CSRFToken: session.CSRFToken}
	}
	return rows
}

// depotResolveRecentlyFailed reports whether depotID's Store lookup failed
// within the last depotResolveTTL, so the guest page can skip re-hitting the
// rate-limited Store API for it (finding #7).
func (s *Server) depotResolveRecentlyFailed(depotID int) bool {
	s.depotResolve.mu.Lock()
	defer s.depotResolve.mu.Unlock()
	at, ok := s.depotResolve.failed[depotID]
	return ok && time.Since(at) < depotResolveTTL
}

// markDepotResolveFailed records that depotID's Store lookup just failed.
func (s *Server) markDepotResolveFailed(depotID int) {
	s.depotResolve.mu.Lock()
	defer s.depotResolve.mu.Unlock()
	s.depotResolve.failed[depotID] = time.Now()
}

// resolveDepotAppName tries to map a Steam depot ID to a game title via the
// Steam Store API. Two patterns are attempted:
//   - depotID == appID (some titles, e.g. older games or primary depots numbered to match)
//   - depotID == appID+1 (the most common Steam pattern: first content depot is appID+1)
//
// Both return "" on failure; the caller skips the update in that case.
func (s *Server) resolveDepotAppName(depotID int) string {
	if name := s.steamStoreAppName(depotID); name != "" {
		return name
	}
	if depotID > 1 {
		if name := s.steamStoreAppName(depotID - 1); name != "" {
			return name
		}
	}
	return ""
}

func (s *Server) steamStoreAppName(appID int) string {
	url := "https://store.steampowered.com/api/appdetails?appids=" + strconv.Itoa(appID)
	resp, err := s.httpClient.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var result map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	entry, ok := result[strconv.Itoa(appID)]
	if !ok || !entry.Success {
		return ""
	}
	return entry.Data.Name
}

func (s *Server) handleGuestPurge(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	platform := r.PathValue("platform")
	gameKey := r.PathValue("gameKey")

	prefixes, err := lifecycle.Delete(s.cat, platform, gameKey, s.lookupAppDepots)
	switch {
	case err == nil, errors.Is(err, lifecycle.ErrGameNotFound):
		// Optimistic + async, same as handleDelete: the catalog row is
		// already cleared, so hand the slow whole-zone PURGE to the queue
		// and remove the row immediately. ErrGameNotFound (a stale tab
		// raced another purge) reaches the same gone-postcondition, so it
		// renders identically. The button's hx-swap="outerHTML" replaces
		// the row with this hidden tr, dropping it from the table.
		s.purgeQueue.Enqueue(prefixes)
		s.writeToast(w, "success", "Cache entry purged.")
		w.Write([]byte(`<tr id="guest-` + html.EscapeString(platform) + "-" + html.EscapeString(gameKey) + `" style="display:none"></tr>`))
	default:
		// A real (DB-layer) failure: nothing was cleared. Re-render the row
		// so the operator doesn't see it vanish on an error -- an empty body
		// under outerHTML would otherwise falsely signal success.
		log.Printf("web: guest purge (%s, %s): %v", platform, gameKey, err)
		s.writeToast(w, "error", "Purge failed.")
		s.renderGuestRow(w, session, platform, gameKey)
	}
}

// renderGuestRow re-emits a single guest row from the catalog, used by
// handleGuestPurge's failure path (the success path removes the row instead).
func (s *Server) renderGuestRow(w http.ResponseWriter, session steamauth.Session, platform, gameKey string) {
	entry, found, err := s.cat.GetGame(platform, gameKey)
	if err != nil || !found {
		if err != nil {
			log.Printf("web: getting guest game (%s, %s): %v", platform, gameKey, err)
		}
		s.writeFallbackRow(w, "guest-"+platform+"-"+gameKey)
		return
	}
	row := guestRowView{LibraryEntry: entry, CSRFToken: session.CSRFToken}
	if err := s.templates.ExecuteTemplate(w, "guest_row", row); err != nil {
		log.Printf("web: rendering guest_row: %v", err)
	}
}

// handleGuestPurgeOrphans sweeps every orphan fragment -- entries below the
// configured min game size that the operator never deliberately kept (not
// Pre-Staged, not monitored) -- in one click. The catalog rows are cleared
// immediately so the whole Guest list declutters at once; only orphans big
// enough to be worth a full-zone scan (>= MinGuestSizeBytes) are enqueued for
// nginx eviction, while sub-threshold slivers are list-cleared only and left
// for nginx's LRU to reclaim (see lifecycle.PurgeOrphans). The button swaps
// innerHTML of #guest-rows, so we re-render the remaining rows.
func (s *Server) handleGuestPurgeOrphans(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	prefixes, err := lifecycle.PurgeOrphans(s.cat, s.minGameSizeBytes(), catalog.MinGuestSizeBytes)
	if err != nil {
		// A real DB failure: nothing (or only part) was cleared. Re-render the
		// current rows so the list reflects actual state, not a false sweep.
		log.Printf("web: purge orphans: %v", err)
		s.writeToast(w, "error", "Could not purge orphans.")
		s.renderGuestRows(w, session)
		return
	}
	s.purgeQueue.Enqueue(prefixes)
	s.writeToast(w, "success", "Orphans purged.")
	s.renderGuestRows(w, session)
}

// renderGuestRows re-emits the remaining guest rows (the #guest-rows tbody
// contents) from the catalog, used by handleGuestPurgeOrphans. Unlike
// handleGuestPage it deliberately skips the lazy Steam name resolution: a bulk
// purge is about removing fragments, and the per-depot Store API round-trips
// would stall the response. Surviving entries keep whatever names the page
// load already resolved.
func (s *Server) renderGuestRows(w http.ResponseWriter, session steamauth.Session) {
	entries, err := s.cat.GuestGames(s.minGameSizeBytes())
	if err != nil {
		log.Printf("web: guest games: %v", err)
	}
	rows := make([]guestRowView, len(entries))
	for i, e := range entries {
		rows[i] = guestRowView{LibraryEntry: e, CSRFToken: session.CSRFToken}
	}
	if err := s.templates.ExecuteTemplate(w, "guest_rows", map[string]any{"Entries": rows}); err != nil {
		log.Printf("web: rendering guest_rows: %v", err)
	}
}

func (s *Server) handlePrefillStatus(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.renderPrefillStatus(w, session.CSRFToken)
}

// mergeCacheStats adds the cache-volume totals, used figures, and cached-game
// count to data from a single Statfs. Shared by the prefill status fragment and
// the dashboard hero meter so both read the same numbers. CacheUsedBytes/
// CacheUsedPct are precomputed here so templates stay arithmetic-free.
func (s *Server) mergeCacheStats(data map[string]any) {
	if s.cacheRoot != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(s.cacheRoot, &st); err == nil {
			total := int64(st.Blocks) * st.Bsize
			free := int64(st.Bavail) * st.Bsize
			data["CacheTotalBytes"] = total
			data["CacheFreeBytes"] = free
			if total > 0 {
				used := total - free
				data["CacheUsedBytes"] = used
				data["CacheUsedPct"] = int(used * 100 / total)
				s.mergeGaugeSegments(data, total, free, used)
			}
		}
	}
	if n, err := s.cat.CachedGameCount(s.minGameSizeBytes()); err == nil {
		data["CachedGameCount"] = n
	}
}

// mergeGaugeSegments precomputes the capacity gauge's band widths and cap marker
// as whole-percent-of-disk values so the template stays arithmetic-free. The
// gauge splits used space into two bands: Locked (monitored games, never
// evicted) and Evictable (everything else on the volume that eviction could
// reclaim -- unlocked cached games, unmanaged/guest content, and any overhead).
// Evictable is derived as used-locked rather than measured directly, so it
// absorbs whatever isn't locked (unlocked games, unmanaged content, overhead).
func (s *Server) mergeGaugeSegments(data map[string]any, total, free, used int64) {
	locked, err := s.cat.LockedBytes(s.minGameSizeBytes())
	if err != nil {
		log.Printf("web: locked bytes: %v", err)
		locked = 0
	}
	if locked > used { // compressed-vs-reported skew: never draw locked past used
		locked = used
	}
	evictable := used - locked
	data["CacheLockedBytes"] = locked
	data["CacheEvictableBytes"] = evictable

	// Band widths as whole percent of the disk. The free remainder isn't drawn
	// as its own element -- it's the uncolored track behind these two segments --
	// so locked+evictable naturally leave the right gap without a third width.
	data["CacheLockedPct"] = int(locked * 100 / total)
	data["CacheEvictablePct"] = int(evictable * 100 / total)

	// Cap marker: where the configured cache cap sits on the disk track. Absent
	// key => no cap set => the template draws no marker.
	if cap := s.maxCacheSizeBytes(); cap > 0 {
		data["CacheCapBytes"] = cap
		mark := int(cap * 100 / total)
		if mark > 100 {
			mark = 100 // a cap larger than the disk pins to the far end
		}
		data["CacheCapMarkPct"] = mark
	}
}

// mergeBandwidthStats adds the dashboard's cache-performance figures (FEATURES
// #1): bytes served from cache ("saved"), bytes pulled from upstream, and the
// request hit-rate. Lifetime totals, since the counter started or was last
// reset. Forward-only -- the cache_keys overwrite already discarded earlier
// HIT history, so there is nothing to backfill.
func (s *Server) mergeBandwidthStats(data map[string]any) {
	totalBytes, hitBytes, totalReqs, hitReqs, err := s.cat.BandwidthTotals("")
	if err != nil {
		log.Printf("web: bandwidth totals: %v", err)
		return
	}
	if totalReqs == 0 {
		return // nothing recorded yet; the template shows an empty state
	}
	data["BandwidthHasData"] = true
	data["BandwidthSaved"] = hitBytes
	data["BandwidthFromUpstream"] = totalBytes - hitBytes
	data["HitRatePct"] = int(hitReqs * 100 / totalReqs)
	if since, err := s.cat.BandwidthSince(); err == nil {
		data["BandwidthSince"] = since
	}
}

// mergePrefillStatus populates the prefill status panel. It is read-only with
// respect to the queue: the prefill worker (RunPrefillWorker) is the single
// mutator. "Running" is driven off the sidecar's on-disk trigger lock (the
// authoritative signal that survives a backend restart), and the active game's
// name comes from the queue's active row rather than any in-memory state.
func (s *Server) mergePrefillStatus(data map[string]any) {
	s.mergeCacheStats(data)

	queue, err := s.cat.ListPrefillQueue()
	if err != nil {
		log.Printf("web: listing prefill queue: %v", err)
	}
	data["Queue"] = queue

	// HasActive (an "active" queue row exists) drives the stale-lock fallback in
	// the template: Running with no active row means a leftover trigger that no
	// queue item owns, which still needs a Cancel control.
	var hasActive bool
	var activeName, activePlatform, activeAppID string
	for _, q := range queue {
		if q.Status == "active" {
			hasActive, activeName = true, q.Name
			activePlatform, activeAppID = q.Platform, q.AppID
			break
		}
	}
	data["HasActive"] = hasActive

	running, err := s.sidecar.IsRunning()
	if err != nil {
		log.Printf("web: checking prefill running state: %v", err)
	}
	data["Running"] = running
	if !running {
		return
	}

	// Name for the dashboard mini-status and the stale-lock fallback.
	name := activeName
	if name == "" {
		name = "an earlier request"
	}
	data["RunningGameName"] = name

	// RunningIsUpdate is true when the active game is already cached (has chunk
	// data), so the UI can say "Updating" instead of "Preloading".
	if activePlatform != "" && activeAppID != "" {
		if entry, found, gErr := s.cat.GetGame(activePlatform, activeAppID); gErr == nil && found {
			data["RunningIsUpdate"] = entry.CacheKeyCount > 0
		}
	}

	// Best-effort real download percent from the sidecar's --no-ansi run log.
	// Only a genuine >=1% reading (or the "complete" snap to 100%) is surfaced;
	// the pre-download "preparing" phase leaves Progress unset so the template
	// keeps the honest indeterminate bar.
	if pct, phase, total, ok := s.sidecar.Progress(); ok {
		totalBytes := prefill.ParseTotalBytes(total)
		switch {
		case phase == "complete":
			// The run log shows "Prefill complete!" but the entrypoint hasn't
			// removed the trigger / written status.json yet, so IsRunning() is
			// still true. Snap to 100% so the operator never sees a parked 99%;
			// the next poll (trigger gone) renders the result on its own.
			data["Progress"] = 100
			data["ProgressComplete"] = true
			data["ProgressTotal"] = total
			s.speed.mu.Lock()
			s.speed.pct, s.speed.totalBytes = 0, 0
			s.speed.mu.Unlock()
		case pct > 0:
			data["Progress"] = pct
			data["ProgressTotal"] = total
			if totalBytes > 0 {
				s.speed.mu.Lock()
				now := time.Now()
				if s.speed.totalBytes == totalBytes && s.speed.pct > 0 && pct > s.speed.pct {
					if dt := now.Sub(s.speed.at).Seconds(); dt > 0 {
						delta := float64(totalBytes) * float64(pct-s.speed.pct) / 100
						if bps := int64(delta / dt); bps > 0 {
							data["ProgressSpeedBytes"] = bps
						}
					}
				}
				s.speed.pct = pct
				s.speed.at = now
				s.speed.totalBytes = totalBytes
				s.speed.mu.Unlock()
			}
		}
	} else {
		s.speed.mu.Lock()
		s.speed.pct, s.speed.totalBytes = 0, 0
		s.speed.mu.Unlock()
	}
}

// handlePrefillTrigger enqueues one or more owned games for preload (R13: owned
// games only) and wakes the worker. It accepts multiple app_id form values, so
// the per-card "Queue" button (one value) and the grid's "Queue selected" bar
// (many) share this one handler. It no longer triggers the sidecar directly;
// the worker (RunPrefillWorker) is the single driver, so games queue rather than
// a second request being rejected. Every path falls through to the same final
// render of the current prefill status alongside any toast, so #prefill-status
// (hx-swap="innerHTML") is never left blank.
func (s *Server) handlePrefillTrigger(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.writeToast(w, "error", "Malformed request.")
		s.renderPrefillStatus(w, session.CSRFToken)
		return
	}
	appIDs := r.Form["app_id"]
	if len(appIDs) == 0 {
		s.writeToast(w, "error", "No games selected.")
		s.renderPrefillStatus(w, session.CSRFToken)
		return
	}

	// One ownership fetch for the whole batch, for an immediate specific error
	// and to capture each game's name. The worker re-checks at start time and
	// the sidecar gates on ownership regardless.
	webAPIKey, operatorSteamID64, _ := s.steamConfig()
	games, err := s.fetchOwnedGames(webAPIKey, operatorSteamID64)
	if err != nil {
		log.Printf("web: fetching owned games for prefill trigger: %v", err)
		s.writeToast(w, "error", "Could not verify ownership; Steam's Web API is unreachable.")
		s.renderPrefillStatus(w, session.CSRFToken)
		return
	}
	names := make(map[int]string, len(games))
	for _, g := range games {
		names[g.AppID] = g.Name
	}

	var queued, already int
	var lastName string
	for _, str := range appIDs {
		appID, convErr := strconv.Atoi(str)
		if convErr != nil {
			continue
		}
		name, owned := names[appID]
		if !owned {
			continue // silently skip un-owned/invalid; summarised in the toast
		}
		added, eErr := s.cat.EnqueuePrefill("steam", strconv.Itoa(appID), name)
		if eErr != nil {
			log.Printf("web: enqueueing prefill for app %d: %v", appID, eErr)
			continue
		}
		if added {
			queued++
			lastName = name
		} else {
			already++
		}
	}

	switch {
	case queued == 0 && already == 0:
		s.writeToast(w, "error", "None of those are in your owned-games list.")
		s.renderPrefillStatus(w, session.CSRFToken)
		return
	case queued == 0:
		s.writeToast(w, "success", "Already in the queue.")
	case queued == 1 && already == 0:
		s.writeToast(w, "success", "Queued "+lastName+".")
	default:
		s.writeToast(w, "success", fmt.Sprintf("Queued %d games.", queued))
	}
	s.wakePrefillWorker()
	s.renderPrefillStatus(w, session.CSRFToken)
}

// handleQueueRemove drops one queue item by id. If it happens to be the active
// run, the sidecar is cancelled first so the worker isn't left waiting on a row
// that's gone.
func (s *Server) handleQueueRemove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeToast(w, "error", "Invalid queue item.")
		s.renderPrefillStatus(w, session.CSRFToken)
		return
	}
	if act, ok, aErr := s.cat.ActivePrefillItem(); aErr != nil {
		log.Printf("web: reading active item for remove: %v", aErr)
	} else if ok && act.ID == id {
		if cErr := s.sidecar.Cancel(); cErr != nil {
			log.Printf("web: cancelling active run on remove: %v", cErr)
		}
		if pErr := s.cat.ClearPrestagePending(act.RequestID); pErr != nil {
			log.Printf("web: clearing prestage on remove: %v", pErr)
		}
	}
	if err := s.cat.DeletePrefillItem(id); err != nil {
		log.Printf("web: removing queue item %d: %v", id, err)
		s.writeToast(w, "error", "Could not remove the item.")
	}
	s.wakePrefillWorker()
	s.renderPrefillStatus(w, session.CSRFToken)
}

// handleQueueMove reorders a queued item one place earlier (up) or later (down).
// A no-op at the edge; the catalog swaps adjacent positions.
func (s *Server) handleQueueMove(up bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.writeToast(w, "error", "Invalid queue item.")
			s.renderPrefillStatus(w, session.CSRFToken)
			return
		}
		if err := s.cat.MovePrefillItem(id, up); err != nil {
			log.Printf("web: reordering queue item %d: %v", id, err)
			s.writeToast(w, "error", "Could not reorder the queue.")
		}
		s.renderPrefillStatus(w, session.CSRFToken)
	}
}

// handleQueueRetry resets a failed item back to queued and wakes the worker.
func (s *Server) handleQueueRetry(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeToast(w, "error", "Invalid queue item.")
		s.renderPrefillStatus(w, session.CSRFToken)
		return
	}
	if err := s.cat.RequeuePrefill(id); err != nil {
		log.Printf("web: retrying queue item %d: %v", id, err)
		s.writeToast(w, "error", "Could not retry the item.")
	} else {
		s.wakePrefillWorker()
	}
	s.renderPrefillStatus(w, session.CSRFToken)
}

// prefillWorkerTick bounds how long a finished or newly-queued run waits to be
// noticed when no wake signal fires. The sidecar writes status at most once per
// run, so a brisk poll just bounds detection latency; it does not hammer anything.
const prefillWorkerTick = 5 * time.Second

// prefillRetryBackoff is how long the worker pauses *starting new runs* after a
// transient start failure (e.g. the Steam Web API being briefly unreachable for
// the owned-games check), so it doesn't tight-loop and hammer the API each tick.
const prefillRetryBackoff = 60 * time.Second

// RunPrefillWorker is the single driver of the prefill queue. It drains
// prefill_queue one game at a time, advancing on a tick or whenever something is
// enqueued or cancelled (wakePrefillWorker). All queue mutation lives here, so
// there is exactly one thing talking to the sidecar; handlers only enqueue and
// the status renderer only reads.
func (s *Server) RunPrefillWorker(ctx context.Context) {
	ticker := time.NewTicker(prefillWorkerTick)
	defer ticker.Stop()
	for {
		s.prefillStep()
		select {
		case <-ctx.Done():
			return
		case <-s.prefillWake:
		case <-ticker.C:
		}
	}
}

// prefillStep performs one non-blocking unit of work. It is the testable seam:
// drive it directly in tests by writing the sidecar's status file between calls.
// Exactly one runs at a time (single worker goroutine), so prefillRetryAfter
// needs no lock.
func (s *Server) prefillStep() {
	// 1. A run is active (ours -- in flight or just finished): resolve or wait.
	if act, ok, err := s.cat.ActivePrefillItem(); err != nil {
		log.Printf("prefill worker: reading active item: %v", err)
		return
	} else if ok {
		// A terminal status for *this* request means the run finished. The
		// requestID filter in PollStatus is load-bearing: status.json is a
		// single shared file, so without it a later run would be falsely
		// "completed" by an earlier run's leftover status.
		if st, found, err := s.sidecar.PollStatus(act.RequestID); err != nil {
			log.Printf("prefill worker: polling status for %s: %v", act.RequestID, err)
			return
		} else if found {
			s.finishPrefill(act, st)
			return
		}
		if running, err := s.sidecar.IsRunning(); err != nil {
			log.Printf("prefill worker: checking running state: %v", err)
			return
		} else if running {
			return // still downloading; check again next tick
		}
		// Active row, no terminal status for our request, and the trigger lock
		// is gone: the run was interrupted (the backend, and with it the
		// sidecar, restarted mid-run). A run that *finished* while we were down
		// is caught by the PollStatus(found) branch above, so this is genuinely
		// "interrupted", not a missed completion.
		log.Printf("prefill worker: run %s for %s was interrupted; marking failed", act.RequestID, act.AppID)
		if err := s.cat.ClearPrestagePending(act.RequestID); err != nil {
			log.Printf("prefill worker: clearing interrupted prestage: %v", err)
		}
		if err := s.cat.MarkPrefillFailed(act.ID, "interrupted by a restart"); err != nil {
			log.Printf("prefill worker: marking interrupted item failed: %v", err)
		}
		return
	}

	// 2. No active run. Don't start one while the sidecar lock is held by a
	// leftover/legacy run, or while backing off from a transient failure.
	if running, err := s.sidecar.IsRunning(); err != nil {
		log.Printf("prefill worker: checking running state: %v", err)
		return
	} else if running {
		return
	}
	if time.Now().Before(s.prefillRetryAfter) {
		return
	}

	// 3. Start the next queued game.
	item, ok, err := s.cat.NextQueuedPrefill()
	if err != nil {
		log.Printf("prefill worker: reading next queued item: %v", err)
		return
	}
	if !ok {
		return // queue drained
	}
	if err := s.startPrefill(item); err != nil {
		switch {
		case errors.Is(err, prefill.ErrNotOwned):
			if mErr := s.cat.MarkPrefillFailed(item.ID, "not in your owned-games list"); mErr != nil {
				log.Printf("prefill worker: marking unowned item failed: %v", mErr)
			}
		case errors.Is(err, prefill.ErrAlreadyRunning):
			// A trigger lock appeared between our check and Trigger; retry next tick.
		default:
			// Transient (owned-games fetch failed, shared volume unwritable):
			// leave the item queued and pause new starts so we don't spin.
			log.Printf("prefill worker: deferring start of %s: %v", item.AppID, err)
			s.prefillRetryAfter = time.Now().Add(prefillRetryBackoff)
		}
	}
}

// startPrefill triggers the sidecar for one queued item and records it as the
// active run, plus a pending prestage so a successful run marks the game
// pre_staged. The owned set comes from the 1h cache (ownedSet) so draining a
// long queue doesn't re-hit the Steam API per game.
func (s *Server) startPrefill(item catalog.QueueItem) error {
	appID, err := strconv.Atoi(item.AppID)
	if err != nil {
		return fmt.Errorf("%w: bad app id %q", prefill.ErrNotOwned, item.AppID)
	}
	owned, err := s.ownedSet()
	if err != nil {
		return fmt.Errorf("owned-games check: %w", err)
	}
	requestID, err := s.sidecar.Trigger([]int{appID}, owned)
	if err != nil {
		return err
	}
	if err := s.cat.MarkPrefillActive(item.ID, requestID); err != nil {
		return fmt.Errorf("marking item active: %w", err)
	}
	if err := s.cat.MarkPrestagePending(requestID, item.Platform, item.AppID, item.Name); err != nil {
		log.Printf("prefill worker: recording pending prestage for %s: %v", item.AppID, err)
	}
	return nil
}

// finishPrefill applies a terminal sidecar status to the active item: success
// marks the game pre_staged (via resolvePrestage) and removes it from the queue;
// failure retains it with a reason so the operator can see why and retry.
func (s *Server) finishPrefill(item catalog.QueueItem, st prefill.Status) {
	s.resolvePrestage(st)
	if st.State == prefill.StateSuccess {
		if err := s.cat.DeletePrefillItem(item.ID); err != nil {
			log.Printf("prefill worker: removing completed item: %v", err)
		}
		return
	}
	reason := st.Error
	if reason == "" {
		reason = "download failed"
	}
	if err := s.cat.MarkPrefillFailed(item.ID, reason); err != nil {
		log.Printf("prefill worker: marking item failed: %v", err)
	}
}

// ownedSet returns the operator's owned-game app IDs as a set, served from the
// 1h owned-games cache (a cold or stale cache does a single blocking fetch). It
// shares s.owned with the dashboard hero pool so the worker and UI don't
// double-fetch.
func (s *Server) ownedSet() (map[int]bool, error) {
	const ttl = time.Hour
	s.owned.mu.Lock()
	cached, fetchedAt := s.owned.games, s.owned.fetchedAt
	s.owned.mu.Unlock()
	if cached == nil || time.Since(fetchedAt) >= ttl {
		webAPIKey, operatorSteamID64, configured := s.steamConfig()
		if !configured {
			return nil, errors.New("steam is not configured")
		}
		games, err := s.fetchOwnedGames(webAPIKey, operatorSteamID64)
		if err != nil {
			return nil, err
		}
		// Treat an empty result as "can't verify ownership right now" (a private
		// profile, or a transient 200-with-no-games) rather than "you own
		// nothing". Caching {} would fail every queued game with ErrNotOwned for
		// a full hour; instead return an error so the worker backs off and
		// retries, exactly like a fetch failure. (ownedHeroGames guards this the
		// same way for the hero pool.)
		if len(games) == 0 {
			return nil, errors.New("owned-games list came back empty")
		}
		s.owned.mu.Lock()
		s.owned.games, s.owned.fetchedAt = games, time.Now()
		s.owned.mu.Unlock()
		cached = games
	}
	set := make(map[int]bool, len(cached))
	for _, g := range cached {
		set[g.AppID] = true
	}
	return set, nil
}

// resolvePrestage applies one terminal sidecar Status to the pending_prestage
// table: success -> CompletePrestage (set pre_staged), anything else -> drop
// the pending row. Shared by the worker (finishPrefill) and reconcilePrestage
// (legacy restart recovery) so both paths treat a result identically.
func (s *Server) resolvePrestage(st prefill.Status) {
	if st.State == prefill.StateSuccess {
		if _, err := s.cat.CompletePrestage(st.RequestID); err != nil {
			log.Printf("web: completing prestage %s: %v", st.RequestID, err)
		}
		return
	}
	if err := s.cat.ClearPrestagePending(st.RequestID); err != nil {
		log.Printf("web: clearing failed prestage %s: %v", st.RequestID, err)
	}
}

// reconcilePrestage resolves a legacy pending Pre-Stage that has no queue row to
// drive it: a run started by a pre-queue build that finished across an upgrade,
// leaving a pending_prestage row the worker won't pick up (the worker only
// reconciles items it finds in prefill_queue). It reads the sidecar's most
// recent status and, if that request still has a pending row, applies it. Called
// once at startup (NewServer).
//
// CompletePrestage/ClearPrestagePending are no-ops when the request has no
// pending row, so it is safe even when there is nothing to reconcile.
func (s *Server) reconcilePrestage() {
	st, found, err := s.sidecar.LatestStatus()
	if err != nil {
		log.Printf("web: reading latest prefill status for reconcile: %v", err)
		return
	}
	if !found || st.RequestID == "" {
		return
	}
	s.resolvePrestage(st)
}

func (s *Server) handlePrefillCancel(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := s.sidecar.Cancel(); err != nil {
		log.Printf("web: cancelling prefill: %v", err)
		s.writeToast(w, "error", "Could not cancel the preload.")
	}
	// Drop the active item so the worker advances to the next queued game
	// instead of recording the cancelled run as a failure to retain.
	if act, ok, err := s.cat.ActivePrefillItem(); err != nil {
		log.Printf("web: reading active prefill item for cancel: %v", err)
	} else if ok {
		if err := s.cat.DeletePrefillItem(act.ID); err != nil {
			log.Printf("web: removing cancelled prefill item: %v", err)
		}
		if err := s.cat.ClearPrestagePending(act.RequestID); err != nil {
			log.Printf("web: clearing cancelled prestage: %v", err)
		}
	}
	s.wakePrefillWorker()
	s.renderPrefillStatus(w, session.CSRFToken)
}

func (s *Server) renderPrefillStatus(w http.ResponseWriter, csrfToken string) {
	data := map[string]any{"CSRFToken": csrfToken}
	s.mergePrefillStatus(data)
	if err := s.templates.ExecuteTemplate(w, "prefill_status", data); err != nil {
		log.Printf("web: rendering prefill_status: %v", err)
	}
}

// diskUsedPct returns the percentage of cache-volume space in use (0-100),
// for the disk-usage readout in the topbar. It uses the same int(used*100/total)
// expression as mergeCacheStats' CacheUsedPct so the topbar chip and the
// Dashboard gauge can never disagree by a rounding step (they previously did:
// the chip computed 100 - free% and floored in the opposite direction). ok is
// false when the cache root isn't configured or Statfs fails, so the UI can show
// "N/A" rather than a misleading 0% (which is a real, distinct state: a full disk).
func (s *Server) diskUsedPct() (pct int, ok bool) {
	if s.cacheRoot == "" {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cacheRoot, &st); err != nil {
		return 0, false
	}
	total := int64(st.Blocks) * st.Bsize
	if total <= 0 {
		return 0, false
	}
	free := int64(st.Bavail) * st.Bsize
	used := total - free
	return int(used * 100 / total), true
}

// cacheDiskTotalBytes returns the cache volume's total size, for the Settings
// "cap larger than disk" sanity warning. ok is false when the cache root isn't
// configured or Statfs fails.
func (s *Server) cacheDiskTotalBytes() (int64, bool) {
	if s.cacheRoot == "" {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cacheRoot, &st); err != nil {
		return 0, false
	}
	total := int64(st.Blocks) * st.Bsize
	if total <= 0 {
		return 0, false
	}
	return total, true
}

// render executes a full-page template. Errors here are a template bug,
// not a user-facing condition, so they're logged and surfaced generically.
func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	// Inject the disk-usage readout for the topbar chip (nav.html reads
	// DiskKnown/DiskUsedPct). Harmless on the few pages that don't include nav
	// (login/setup). DiskKnown distinguishes "unknown" from a genuine 0% so the
	// template can show N/A instead.
	pct, known := s.diskUsedPct()
	data["DiskKnown"] = known
	data["DiskUsedPct"] = pct // disk-used % for the topbar chip; valid only when DiskKnown
	// Persistent "eviction paused" banner (nav.html) on every page, so it's
	// obvious nothing is being auto-deleted while a pause is in effect.
	if _, ok := data["EvictionPaused"]; !ok {
		data["EvictionPaused"] = s.evictionPaused()
		data["EvictionResumeText"] = s.evictionResumeText()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("web: rendering %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// writeToast appends an out-of-band toast fragment to the response. It
// deliberately does not call WriteHeader or return -- callers must follow
// it by writing the actual current state of whatever the request's
// hx-target points at (the row, the platform row, the prefill status).
// htmx only extracts and swaps OOB-marked elements out of a response
// independently of the declared hx-target; the *rest* of the response
// body is what gets swapped into hx-target. A toast-only response (no
// target content) means htmx swaps an empty remainder into hx-target's
// outerHTML/innerHTML -- silently blanking the very row/element the
// operator was looking at, which is worse than the "indistinguishable
// from a click that did nothing" failure mode this is meant to prevent.
func (s *Server) writeToast(w http.ResponseWriter, level, message string) {
	if err := s.templates.ExecuteTemplate(w, "toast", map[string]any{"Level": level, "Message": message}); err != nil {
		log.Printf("web: rendering toast: %v", err)
	}
}

// writeFallbackRow is the last-resort target content for the rare case
// where even re-fetching current state to render alongside an error toast
// fails -- still better than leaving hx-target's swap empty.
func (s *Server) writeFallbackRow(w http.ResponseWriter, rowID string) {
	w.Write([]byte(`<tr id="` + html.EscapeString(rowID) + `"><td colspan="5">Could not refresh -- reload the page.</td></tr>`))
}
