package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/cdn"
	"github.com/immortal-coil/buffarr/internal/dnsmasq"
	"github.com/immortal-coil/buffarr/internal/prefill"
	"github.com/immortal-coil/buffarr/internal/steamauth"
)

const testOperatorSteamID = "76561197960435530"

// fakeEnqueuer stands in for *nginx.PurgeQueue: delete is now optimistic and
// asynchronous, so the handler hands the actual (slow, whole-zone-scanning)
// PURGE to a queue rather than running it inline. The real PURGE
// wildcard/HTTP mechanics and the queue's worker/dedup behavior are covered
// in internal/nginx; here we only assert the handler enqueues the right
// prefixes. Concurrency-safe because the background queue would call Enqueue
// from its own goroutine in production.
type fakeEnqueuer struct {
	mu       sync.Mutex
	prefixes []string
	busy     bool // when true, Idle() reports false (a purge is "draining")
}

func (f *fakeEnqueuer) Enqueue(prefixes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefixes = append(f.prefixes, prefixes...)
}

func (f *fakeEnqueuer) Idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.busy
}

func (f *fakeEnqueuer) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prefixes...)
}

func testPlatforms() []dnsmasq.Platform {
	return []dnsmasq.Platform{
		{Name: "steam", Description: "CDN for Steam", Domains: []dnsmasq.Domain{{Base: "lancache.steamcontent.com", Kind: dnsmasq.DomainExact}}},
		{Name: "blizzard", Description: "CDN for Battle.net", Domains: []dnsmasq.Domain{{Base: "dist.blizzard.com", Kind: dnsmasq.DomainExact}}},
	}
}

type testServer struct {
	*Server
	cat        *catalog.Catalog
	sessions   *steamauth.Manager
	enqueuer   *fakeEnqueuer
	sessionID  string
	csrfToken  string
	prefillDir string // holds trigger / status.json / last-run.log for prefill-state tests
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	platforms := testPlatforms()
	names := make([]string, len(platforms))
	for i, p := range platforms {
		names[i] = p.Name
	}
	if err := cat.SeedPlatformsIfEmpty(names, []string{"steam"}); err != nil {
		t.Fatalf("SeedPlatformsIfEmpty: %v", err)
	}

	dir := t.TempDir()
	toggler := cdn.NewToggler(cat, platforms, "10.0.0.5",
		filepath.Join(dir, "cdn-hosts"), filepath.Join(dir, "cdn-wildcards.conf"), filepath.Join(dir, "allowlist.conf"))
	if err := toggler.RegenerateFromPersisted(); err != nil {
		t.Fatalf("RegenerateFromPersisted: %v", err)
	}

	sidecar := prefill.NewSidecar(
		filepath.Join(dir, "trigger"), filepath.Join(dir, "status.json"), filepath.Join(dir, "selection.json"),
		filepath.Join(dir, "last-run.log"))

	sessions := steamauth.NewManager()
	sessionID, csrfToken, err := sessions.Create(testOperatorSteamID)
	if err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}

	enq := &fakeEnqueuer{}
	srv, err := NewServer(cat, sessions, toggler, sidecar, enq, "fake-key", testOperatorSteamID, "", 0, t.TempDir(), "10.0.0.5", false, nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Default fake: no owned games, no error -- individual tests override
	// as needed.
	srv.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) { return nil, nil }

	return &testServer{Server: srv, cat: cat, sessions: sessions, enqueuer: enq, sessionID: sessionID, csrfToken: csrfToken, prefillDir: dir}
}

// newUnconfiguredTestServer builds a server with no Steam settings from
// either source (env or DB) -- the genuine first-boot state setupGate
// exists for.
func newUnconfiguredTestServer(t *testing.T) *testServer {
	t.Helper()

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	dir := t.TempDir()
	toggler := cdn.NewToggler(cat, testPlatforms(), "10.0.0.5",
		filepath.Join(dir, "cdn-hosts"), filepath.Join(dir, "cdn-wildcards.conf"), filepath.Join(dir, "allowlist.conf"))
	sidecar := prefill.NewSidecar(
		filepath.Join(dir, "trigger"), filepath.Join(dir, "status.json"), filepath.Join(dir, "selection.json"),
		filepath.Join(dir, "last-run.log"))
	sessions := steamauth.NewManager()

	enq := &fakeEnqueuer{}
	srv, err := NewServer(cat, sessions, toggler, sidecar, enq, "", "", "", 0, t.TempDir(), "10.0.0.5", false, nil, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	return &testServer{Server: srv, cat: cat, sessions: sessions, enqueuer: enq}
}

func (ts *testServer) authedRequest(method, path string, body url.Values) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(&http.Cookie{Name: steamauth.SessionCookieName, Value: ts.sessionID})
	req.Header.Set(steamauth.CSRFHeader, ts.csrfToken)
	return req
}

// TestAuthGate_ProtectedRoutesRedirectWhenUnauthenticated is the
// discriminating check (KTD16): every route except the public allowlist
// must redirect to login without a valid session. Enumerated so adding a
// new route without registering it here -- or accidentally adding it to
// the public allowlist -- is caught, rather than relying on remembering
// to gate each handler individually.
func TestAuthGate_ProtectedRoutesRedirectWhenUnauthenticated(t *testing.T) {
	ts := newTestServer(t)
	handler := ts.Handler()

	protectedRoutes := []struct {
		method, path string
	}{
		{"GET", "/"},
		{"GET", "/library"},
		{"POST", "/library/steam/730/monitor"},
		{"POST", "/library/steam/730/unmonitor"},
		{"POST", "/library/steam/730/delete"},
		{"GET", "/cdn"},
		{"POST", "/cdn/steam/enable"},
		{"POST", "/cdn/steam/disable"},
		{"GET", "/prefill"},
		{"POST", "/prefill/trigger"},
		{"GET", "/prefill/status"},
		{"POST", "/prefill/cancel"},
		{"POST", "/prefill/queue/1/remove"},
		{"POST", "/prefill/queue/1/up"},
		{"POST", "/prefill/queue/1/down"},
		{"POST", "/prefill/queue/1/retry"},
		{"GET", "/settings"},
		{"POST", "/settings/min-game-size"},
		{"POST", "/settings/pause-eviction"},
		{"POST", "/settings/reset-bandwidth"},
		{"GET", "/guest"},
		{"GET", "/guest/rows"},
		{"POST", "/logout"},
	}

	for _, rt := range protectedRoutes {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s %s: expected a redirect (303) when unauthenticated, got %d", rt.method, rt.path, rec.Code)
			continue
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("%s %s: expected redirect to /login, got %q", rt.method, rt.path, loc)
		}
	}
}

func TestAuthGate_HTMXRequestGetsHXRedirectHeader(t *testing.T) {
	ts := newTestServer(t)
	req := httptest.NewRequest("GET", "/library", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated htmx request, got %d", rec.Code)
	}
	if rec.Header().Get("HX-Redirect") != "/login" {
		t.Fatalf("expected HX-Redirect: /login header, got %q", rec.Header().Get("HX-Redirect"))
	}
}

func TestAuthGate_PublicRoutesAccessibleWithoutSession(t *testing.T) {
	ts := newTestServer(t)
	handler := ts.Handler()

	for _, path := range []string{"/login", "/static/htmx.min.js"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 without a session, got %d", path, rec.Code)
		}
	}
}

// TestCSRF_MutatingRoutesRejectedWithoutValidToken is the CSRF analogue of
// the auth-gate enumeration: every state-changing route must reject a
// request with a valid session but no/wrong CSRF token.
func TestCSRF_MutatingRoutesRejectedWithoutValidToken(t *testing.T) {
	ts := newTestServer(t)
	handler := ts.Handler()

	mutatingRoutes := []string{
		"/library/steam/730/monitor",
		"/library/steam/730/unmonitor",
		"/library/steam/730/delete",
		"/cdn/steam/enable",
		"/cdn/steam/disable",
		"/prefill/trigger",
		"/prefill/cancel",
		"/prefill/queue/1/remove",
		"/prefill/queue/1/up",
		"/prefill/queue/1/down",
		"/prefill/queue/1/retry",
	}

	for _, path := range mutatingRoutes {
		req := httptest.NewRequest("POST", path, nil)
		req.AddCookie(&http.Cookie{Name: steamauth.SessionCookieName, Value: ts.sessionID})
		// deliberately no X-CSRF-Token header
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: expected 403 without a CSRF token, got %d (body: %s)", path, rec.Code, rec.Body.String())
		}
	}
}

// TestCSRF_HTMXStaleTokenSelfHealsWithRefresh covers the common operational
// case the plain-403 path handled badly: an open tab whose CSRF token
// predates a backend restart (sessions are in-memory). The mutation must
// still be rejected, but instead of a silent 403 htmx won't swap, the
// operator gets a page reload that re-arms the token so the next click works.
func TestCSRF_HTMXStaleTokenSelfHealsWithRefresh(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 5_000_000_000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := httptest.NewRequest("POST", "/library/steam/730/monitor", nil)
	req.AddCookie(&http.Cookie{Name: steamauth.SessionCookieName, Value: ts.sessionID})
	req.Header.Set("HX-Request", "true")
	req.Header.Set(steamauth.CSRFHeader, "stale-token-from-an-old-page-load")
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 so htmx processes the response, got %d", rec.Code)
	}
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("expected HX-Refresh:true to reload the stale page, got headers: %v", rec.Header())
	}
	// The rejected request must not have mutated anything.
	entry, found, err := ts.cat.GetGame("steam", "730")
	if err != nil || !found {
		t.Fatalf("GetGame: err=%v found=%v", err, found)
	}
	if entry.Monitored {
		t.Fatalf("a stale-token request must be rejected before mutating; game was monitored")
	}
}

func TestLibrary_HappyPath_ListsEntriesSortedAndWithControls(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 6*1024*1024*1024, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("GET", "/library", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Counter-Strike 2") {
		t.Fatalf("expected the game name in the rendered library, got: %s", body)
	}
	if !strings.Contains(body, "6.0 GiB") {
		t.Fatalf("expected human-readable disk space, got: %s", body)
	}
	if !strings.Contains(body, ">Lock<") || !strings.Contains(body, ">Delete<") {
		t.Fatalf("expected lock/delete controls present, got: %s", body)
	}
}

func TestLibrary_SortedByDiskSpaceDescending(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	// Both sizes are above catalog.MinLibrarySizeBytes so the size floor
	// doesn't hide either -- this test is about ordering, not the threshold.
	if err := ts.cat.UpsertCacheKey("steam", "730", "Small Game", "steam/depot/731/chunk/a", 6*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := ts.cat.UpsertCacheKey("steam", "440", "Big Game", "steam/depot/441/chunk/a", 9*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("GET", "/library", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	bigIdx := strings.Index(body, "Big Game")
	smallIdx := strings.Index(body, "Small Game")
	if bigIdx == -1 || smallIdx == -1 {
		t.Fatalf("expected both games present, got: %s", body)
	}
	if bigIdx > smallIdx {
		t.Fatalf("expected the larger entry (Big Game) listed before the smaller one (disk-space descending), got: %s", body)
	}
}

func TestLibrary_HidesSubThresholdMetadataEntries(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	// A real download (above the floor) and a tiny metadata/manifest entry
	// (a few KB) the Steam client pulled for a game never downloaded.
	if err := ts.cat.UpsertCacheKey("steam", "440", "Real Download", "steam/depot/441/chunk/a", 6*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := ts.cat.UpsertCacheKey("steam", "730", "Metadata Only", "steam/depot/731/manifest", 2_048, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("GET", "/library", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Real Download") {
		t.Fatalf("expected the genuinely-downloaded game to be listed, got: %s", body)
	}
	if strings.Contains(body, "Metadata Only") {
		t.Fatalf("expected the sub-threshold metadata entry to be hidden from the library, got: %s", body)
	}
}

// TestMinGameSize_PersistsAndChangesLibraryFloor is the end-to-end proof that
// the floor is now a live DB setting: an entry hidden under the default 5 GiB
// floor becomes visible after the operator lowers the cutoff through Settings,
// with no restart. This is the behaviour the env-var-to-DB move exists for.
func TestMinGameSize_PersistsAndChangesLibraryFloor(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	if err := ts.cat.UpsertCacheKey("steam", "440", "Real Download", "steam/depot/441/chunk/a", 6*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	// 100 MiB: a real-ish download, but below the historical 5 GiB default.
	if err := ts.cat.UpsertCacheKey("steam", "730", "Mid Game", "steam/depot/731/chunk/a", 100*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	// Default floor (no setting): the 100 MiB game is hidden.
	req := ts.authedRequest("GET", "/library", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if body := rec.Body.String(); strings.Contains(body, "Mid Game") {
		t.Fatalf("expected Mid Game hidden under the 5 GiB default floor, got: %s", body)
	}

	// Lower the floor to 50m through the Settings handler.
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/settings/min-game-size", url.Values{"min_game_size": {"50m"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from min-game-size save, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Minimum game size updated") {
		t.Fatalf("expected a success toast, got: %s", body)
	}

	// Setting persisted, and the live accessor reflects it.
	if got := ts.minGameSizeBytes(); got != 50*1024*1024 {
		t.Fatalf("expected minGameSizeBytes 50 MiB after save, got %d", got)
	}

	// Same Library request now shows the 100 MiB game (no restart in between).
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/library", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Mid Game") {
		t.Fatalf("expected Mid Game visible after lowering the floor to 50m, got: %s", body)
	}
	if !strings.Contains(body, "Real Download") {
		t.Fatalf("expected Real Download still visible, got: %s", body)
	}
}

// TestMinGameSize_RejectsUnparseableInput proves a bad value is neither saved
// nor allowed to silently zero the floor (which would flood the Library and
// make "Purge orphans" sweep everything).
func TestMinGameSize_RejectsUnparseableInput(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/settings/min-game-size", url.Values{"min_game_size": {"banana"}}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (with error toast), got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Enter a size") {
		t.Fatalf("expected an error toast guiding the format, got: %s", body)
	}
	if _, found, _ := ts.cat.GetSetting("min_game_size"); found {
		t.Fatalf("expected no min_game_size persisted for unparseable input")
	}
	if got := ts.minGameSizeBytes(); got != catalog.MinLibrarySizeBytes {
		t.Fatalf("expected floor to stay at the 5 GiB default, got %d", got)
	}
}

func TestMonitor_TogglesAndRendersUpdatedRow(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("POST", "/library/steam/730/monitor", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ">Unlock<") {
		t.Fatalf("expected the row to now show the Unmonitor control, got: %s", rec.Body.String())
	}

	entry, found, err := ts.cat.GetGame("steam", "730")
	if err != nil || !found || !entry.Monitored {
		t.Fatalf("expected the catalog to reflect monitored=true, found=%v entry=%+v err=%v", found, entry, err)
	}
}

// TestDelete_RejectedWhileMonitoredIsExplainedNotSilent covers the plan's
// edge case: a delete on a monitored entry is disabled/explained, not a
// silent failure. The UI disables the button, but a stale tab could still
// POST directly -- confirm that path is explained too.
func TestDelete_RejectedWhileMonitoredIsExplainedNotSilent(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	if err := ts.cat.SetMonitored("steam", "730", true); err != nil {
		t.Fatalf("SetMonitored: %v", err)
	}

	req := ts.authedRequest("POST", "/library/steam/730/delete", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "locked") {
		t.Fatalf("expected an explanatory message mentioning the lock, got: %s", rec.Body.String())
	}
	// The regression this guards against: a toast-only response would
	// blank the row on the client (htmx's OOB swap leaves nothing for the
	// declared hx-target's outerHTML swap). The row's own content --
	// here, the still-held row's Unlock control -- must still be
	// present in the same response as the toast.
	if !strings.Contains(rec.Body.String(), ">Unlock<") {
		t.Fatalf("expected the row's own content still present alongside the toast (not blanked), got: %s", rec.Body.String())
	}

	keys, err := ts.cat.CacheKeysForGame("steam", "730")
	if err != nil || len(keys) != 1 {
		t.Fatalf("expected the entry untouched, keys=%v err=%v", keys, err)
	}
}

// TestDelete_HappyPath_RowDisappearsConsistentWithFullReload covers AE2
// (R6, R7) at the webui layer, and the consistency fix where a real
// successful delete must hide the row the same way the idempotent
// already-gone case does -- not render a visible 0-byte row that a full
// /library reload would immediately make disappear (List() hides
// zero-cache-key games; the partial swap must match).
func TestDelete_HappyPath_RowDisappearsConsistentWithFullReload(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("POST", "/library/steam/730/delete", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `style="display:none"`) {
		t.Fatalf("expected the row hidden after a real delete (matching what a full reload would show), got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Counter-Strike 2") {
		t.Fatalf("expected no visible 0-byte row content after delete, got: %s", rec.Body.String())
	}

	keys, err := ts.cat.CacheKeysForGame("steam", "730")
	if err != nil || len(keys) != 0 {
		t.Fatalf("expected cache keys cleared, keys=%v err=%v", keys, err)
	}

	// The actual eviction is handed to the background queue (the catalog is
	// cleared optimistically); the depot prefix must have been enqueued.
	if got := ts.enqueuer.got(); len(got) != 1 || got[0] != "steam/depot/731/" {
		t.Fatalf("expected the depot prefix enqueued for background purge, got %v", got)
	}
}

// TestDelete_AlreadyDeletedIsIdempotentNotError covers the plan's stale-
// tab edge case: deleting an already-deleted entry is idempotent.
func TestDelete_AlreadyDeletedIsIdempotentNotError(t *testing.T) {
	ts := newTestServer(t)
	// Never cached at all -- same as "already deleted by another tab."
	req := ts.authedRequest("POST", "/library/steam/730/delete", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected a 200 no-op for an already-gone entry, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "class=\"toast toast-error\"") {
		t.Fatalf("expected no error toast for an idempotent delete, got: %s", rec.Body.String())
	}
}

// TestDelete_OptimisticallyClearsCatalogAndEnqueuesPurge documents the
// deliberate semantics change to async delete. The old behavior tried to
// PURGE inline and, on failure, left the catalog intact + showed an error
// toast. That inline PURGE is exactly what made delete "do nothing": a
// wildcard PURGE scans the whole cache zone and routinely outran the HTTP
// client's timeout, so the operator saw no result. Delete is now optimistic:
// the catalog row is cleared and the row removed immediately (the operator
// sees it work), and the slow eviction is handed to the background queue.
// There is therefore no synchronous purge-failure path to surface here.
func TestDelete_OptimisticallyClearsCatalogAndEnqueuesPurge(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("POST", "/library/steam/730/delete", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// No error toast: the operator-visible outcome is success.
	if strings.Contains(rec.Body.String(), "toast-error") {
		t.Fatalf("expected no error toast for an optimistic delete, got: %s", rec.Body.String())
	}
	// The row is removed (hidden), matching what a full reload would show.
	if !strings.Contains(rec.Body.String(), `style="display:none"`) {
		t.Fatalf("expected the row hidden after delete, got: %s", rec.Body.String())
	}
	// Catalog is cleared immediately.
	keys, err := ts.cat.CacheKeysForGame("steam", "730")
	if err != nil || len(keys) != 0 {
		t.Fatalf("expected the catalog cleared immediately, keys=%v err=%v", keys, err)
	}
	// And the eviction was handed off to the background queue.
	if got := ts.enqueuer.got(); len(got) != 1 || got[0] != "steam/depot/731/" {
		t.Fatalf("expected the depot prefix enqueued for background purge, got %v", got)
	}
}

// TestGuestPurge_RemovesRowAndEnqueues covers the guest-cache Purge button,
// which had the same inline-PURGE timeout bug as Delete and now shares the
// optimistic+async path: the row is removed and the catalog cleared
// immediately, with the eviction handed to the background queue.
func TestGuestPurge_RemovesRowAndEnqueues(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "depot:5000", "Steam Depot 5000", "steam/depot/5000/chunk/a", 2*1024*1024, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	req := ts.authedRequest("POST", "/guest/steam/depot:5000/purge", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="guest-steam-depot:5000"`) || !strings.Contains(rec.Body.String(), `style="display:none"`) {
		t.Fatalf("expected the guest row removed (hidden), got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "toast-error") {
		t.Fatalf("expected a success (not error) toast, got: %s", rec.Body.String())
	}
	keys, err := ts.cat.CacheKeysForGame("steam", "depot:5000")
	if err != nil || len(keys) != 0 {
		t.Fatalf("expected guest cache keys cleared, keys=%v err=%v", keys, err)
	}
	if got := ts.enqueuer.got(); len(got) != 1 || got[0] != "steam/depot/5000/" {
		t.Fatalf("expected the depot prefix enqueued for background purge, got %v", got)
	}
}

func TestCDN_HappyPath_ListsAndToggles(t *testing.T) {
	ts := newTestServer(t)

	// Platform control now lives on the Settings page (the standalone
	// Platforms page was folded in); the listing must render there.
	req := ts.authedRequest("GET", "/settings", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "steam") || !strings.Contains(rec.Body.String(), ">Enabled<") {
		t.Fatalf("expected steam listed as enabled on Settings, got: %s", rec.Body.String())
	}

	// And /cdn redirects to Settings rather than rendering its own page.
	rreq := ts.authedRequest("GET", "/cdn", nil)
	rrec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rrec, rreq)
	if rrec.Code != http.StatusSeeOther || rrec.Header().Get("Location") != "/settings" {
		t.Fatalf("expected /cdn to redirect to /settings, got %d %q", rrec.Code, rrec.Header().Get("Location"))
	}

	req = ts.authedRequest("POST", "/cdn/steam/disable", nil)
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ">Disabled<") {
		t.Fatalf("expected the row to reflect disabled, got: %s", rec.Body.String())
	}

	statuses, err := ts.toggler.ListPlatforms()
	if err != nil {
		t.Fatalf("ListPlatforms: %v", err)
	}
	for _, st := range statuses {
		if st.Name == "steam" && st.Enabled {
			t.Fatalf("expected steam disabled after the toggle, got %+v", st)
		}
	}
}

// TestCDN_ToggleFailureRendersErrorNotFalseState covers U6's partial-
// failure integration: a failing Toggle call must not render a false
// "enabled" (or any other state the toggler doesn't actually hold).
func TestCDN_ToggleFailureRendersErrorAndActualState(t *testing.T) {
	ts := newTestServer(t)

	// Force a failure by making the allow-list path's directory read-only.
	dir := t.TempDir()
	toggler := cdn.NewToggler(ts.cat, testPlatforms(), "10.0.0.5",
		filepath.Join(dir, "cdn-hosts"), filepath.Join(dir, "cdn-wildcards.conf"), filepath.Join(dir, "ro", "allowlist.conf"))
	if err := os.MkdirAll(filepath.Join(dir, "ro"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := toggler.RegenerateFromPersisted(); err != nil {
		t.Fatalf("initial RegenerateFromPersisted: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "ro"), 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "ro"), 0o755) })
	ts.toggler = toggler

	req := ts.authedRequest("POST", "/cdn/steam/disable", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "toast-error") {
		t.Fatalf("expected an error toast on toggle failure, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ">Enabled<") || strings.Contains(rec.Body.String(), ">Disabled<") {
		t.Fatalf("expected the row to still reflect the real (reverted, still-enabled) state, got: %s", rec.Body.String())
	}
}

func TestPrefill_OwnedGamesFetchErrorRendersExplicitError(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) { return nil, errTest }

	req := ts.authedRequest("GET", "/prefill", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("expected an explicit error state, got: %s", rec.Body.String())
	}
}

func TestPrefill_TriggerNonOwnedAppRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 730, Name: "Counter-Strike 2"}}, nil
	}

	req := ts.authedRequest("POST", "/prefill/trigger", url.Values{"app_id": {"999"}})
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "toast-error") {
		t.Fatalf("expected an error toast for a non-owned app, got: %s", rec.Body.String())
	}
	// Regression guard: #prefill-status (hx-swap="innerHTML") must still
	// get real content alongside the toast, not be left blank.
	if !strings.Contains(rec.Body.String(), "no preload running") {
		t.Fatalf("expected the status area's actual current content present alongside the toast (not blanked), got: %s", rec.Body.String())
	}
	running, err := ts.sidecar.IsRunning()
	if err != nil || running {
		t.Fatalf("expected no trigger written for a rejected app, running=%v err=%v", running, err)
	}
}

// TestPrefill_TriggerEnqueues confirms the manual Preload button now enqueues
// (rather than triggering the sidecar directly): a successful POST queues the
// owned game and does not itself hold the trigger lock.
func TestPrefill_TriggerEnqueues(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 730, Name: "Counter-Strike 2"}}, nil
	}

	req := ts.authedRequest("POST", "/prefill/trigger", url.Values{"app_id": {"730"}})
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Queued") {
		t.Fatalf("expected a queued confirmation, got: %s", rec.Body.String())
	}
	q, err := ts.cat.ListPrefillQueue()
	if err != nil || len(q) != 1 || q[0].AppID != "730" {
		t.Fatalf("expected app 730 queued, got %+v err=%v", q, err)
	}
	// The handler must NOT trigger the sidecar itself; the worker does.
	if running, _ := ts.sidecar.IsRunning(); running {
		t.Fatalf("the trigger handler should not hold the sidecar lock")
	}
}

// TestPrefill_ConcurrentTriggersBothQueue is the point of the single-driver
// consolidation: a second Preload while one is queued no longer gets rejected,
// it just queues behind the first (FIFO).
func TestPrefill_ConcurrentTriggersBothQueue(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 730, Name: "A"}, {AppID: 440, Name: "B"}}, nil
	}

	for _, app := range []string{"730", "440"} {
		req := ts.authedRequest("POST", "/prefill/trigger", url.Values{"app_id": {app}})
		ts.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	q, err := ts.cat.ListPrefillQueue()
	if err != nil || len(q) != 2 || q[0].AppID != "730" || q[1].AppID != "440" {
		t.Fatalf("expected both games queued FIFO (730,440), got %+v err=%v", q, err)
	}
}

// TestPrefill_StaleLockAfterRestartStillCancellable guards the wedge where a
// trigger outlives the backend process: the on-disk lock persists across a
// restart with no matching queue row. Status must still report the prefill as
// running (driven off the lock, not in-memory state) and offer a Cancel control,
// so the operator can clear the lock from the UI instead of being wedged.
func TestPrefill_StaleLockAfterRestartStillCancellable(t *testing.T) {
	ts := newTestServer(t)
	// Seed only the on-disk lock -- exactly the post-restart state, with no
	// active queue row to name the run.
	if _, err := ts.sidecar.Trigger([]int{730}, map[int]bool{730: true}); err != nil {
		t.Fatalf("seeding the on-disk trigger lock: %v", err)
	}

	req := ts.authedRequest("GET", "/prefill/status", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "/prefill/cancel") {
		t.Fatalf("expected a Cancel control for a stale on-disk lock, got: %s", body)
	}
}

// TestPrefill_StatusSnapsToCompleteOnDoneMarker is the core of the 99%-park fix:
// while the run's trigger is still present, a log carrying the terminal
// "Prefill complete!" marker must render a snapped 100% complete bar (not the
// parked 98/99% last-percent), so the operator never sees a stuck fraction and
// the next poll resolves to the success line on its own -- no manual refresh.
func TestPrefill_StatusSnapsToCompleteOnDoneMarker(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ts.sidecar.Trigger([]int{730}, map[int]bool{730: true}); err != nil {
		t.Fatalf("seeding the on-disk trigger lock: %v", err)
	}
	// Write the finished-run log AFTER the trigger so the mtime stale-gate trusts
	// it (log at least as new as the trigger).
	logPath := filepath.Join(ts.prefillDir, "last-run.log")
	if err := os.WriteFile(logPath, []byte("Downloading 3.23 GiB\nDownloading..: 99%\nPrefill complete!\n"), 0o644); err != nil {
		t.Fatalf("writing complete log: %v", err)
	}

	req := ts.authedRequest("GET", "/prefill/status", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "progress-fill--complete") || !strings.Contains(body, `aria-valuenow="100"`) {
		t.Fatalf("expected a snapped 100%% complete bar, got: %s", body)
	}
	if strings.Contains(body, `aria-valuenow="99"`) {
		t.Fatalf("bar should not still render the parked 99%%, got: %s", body)
	}
}

// TestPrefill_StatusDeterminateDuringDownload guards the un-changed mid-run path:
// a log with a real percent but no completion marker renders the determinate bar
// at that percent, NOT the complete state.
func TestPrefill_StatusDeterminateDuringDownload(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ts.sidecar.Trigger([]int{730}, map[int]bool{730: true}); err != nil {
		t.Fatalf("seeding the on-disk trigger lock: %v", err)
	}
	logPath := filepath.Join(ts.prefillDir, "last-run.log")
	if err := os.WriteFile(logPath, []byte("Downloading 3.23 GiB\nDownloading..: 51%\n"), 0o644); err != nil {
		t.Fatalf("writing mid-run log: %v", err)
	}

	req := ts.authedRequest("GET", "/prefill/status", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `aria-valuenow="51"`) {
		t.Fatalf("expected the determinate bar at 51%%, got: %s", body)
	}
	if strings.Contains(body, "progress-fill--complete") {
		t.Fatalf("a mid-run log must not render the complete state, got: %s", body)
	}
}

// completeSidecarRun mimics the real sidecar entrypoint finishing a run: write
// the terminal status.json (matching requestID), then remove the trigger.
func completeSidecarRun(t *testing.T, ts *testServer, requestID, state, errMsg string) {
	t.Helper()
	statusJSON := fmt.Sprintf(`{"request_id":%q,"state":%q,"exit_code":0,"error":%q}`, requestID, state, errMsg)
	if err := os.WriteFile(filepath.Join(ts.prefillDir, "status.json"), []byte(statusJSON), 0o644); err != nil {
		t.Fatalf("writing status: %v", err)
	}
	if err := os.Remove(filepath.Join(ts.prefillDir, "trigger")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing trigger: %v", err)
	}
}

// seedActivePrefill enqueues a game and runs one worker step to start it,
// leaving it as the active run. The caller must have set fetchOwnedGames to
// include the app.
func (ts *testServer) seedActivePrefill(t *testing.T, appID, name string) (id int64, requestID string) {
	t.Helper()
	if _, err := ts.cat.EnqueuePrefill("steam", appID, name); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ts.prefillStep()
	act, ok, err := ts.cat.ActivePrefillItem()
	if err != nil || !ok || act.AppID != appID {
		t.Fatalf("expected %s active after step, got %+v ok=%v err=%v", appID, act, ok, err)
	}
	return act.ID, act.RequestID
}

func TestPrefillWorker_StartsNextQueuedAndMarksActive(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil
	}
	ts.cat.EnqueuePrefill("steam", "100", "Alpha")

	ts.prefillStep()

	act, ok, err := ts.cat.ActivePrefillItem()
	if err != nil || !ok || act.AppID != "100" || act.RequestID == "" {
		t.Fatalf("expected app 100 active with a request id, got %+v ok=%v err=%v", act, ok, err)
	}
	if running, _ := ts.sidecar.IsRunning(); !running {
		t.Fatalf("starting an item should hold the sidecar trigger lock")
	}
}

func TestPrefillWorker_FinishSuccessRemovesItem(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil
	}
	_, reqID := ts.seedActivePrefill(t, "100", "Alpha")

	completeSidecarRun(t, ts, reqID, "success", "")
	ts.prefillStep() // sees the success status -> finishPrefill -> remove

	if q, _ := ts.cat.ListPrefillQueue(); len(q) != 0 {
		t.Fatalf("a successful run should be removed from the queue, got %+v", q)
	}
}

func TestPrefillWorker_FinishFailureRetainsWithReason(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil
	}
	_, reqID := ts.seedActivePrefill(t, "100", "Alpha")

	completeSidecarRun(t, ts, reqID, "failed", "depot download error")
	ts.prefillStep()

	q, _ := ts.cat.ListPrefillQueue()
	if len(q) != 1 || q[0].Status != "failed" || q[0].Error != "depot download error" {
		t.Fatalf("a failed run should be retained with its reason, got %+v", q)
	}
}

// TestPrefillWorker_DoesNotCompleteNextWithStaleStatus is the load-bearing test:
// status.json is a single shared file, so after game A finishes, its success
// status lingers. When the worker starts game B, B must NOT be falsely completed
// by A's leftover status. This only holds because PollStatus filters on the
// requestID.
func TestPrefillWorker_DoesNotCompleteNextWithStaleStatus(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}, {AppID: 200, Name: "Beta"}}, nil
	}
	ts.cat.EnqueuePrefill("steam", "100", "Alpha")
	ts.cat.EnqueuePrefill("steam", "200", "Beta")

	ts.prefillStep() // start Alpha
	actA, _, _ := ts.cat.ActivePrefillItem()
	reqA := actA.RequestID

	completeSidecarRun(t, ts, reqA, "success", "") // Alpha done; status.json now holds reqA
	ts.prefillStep()                               // finish Alpha (removed)

	ts.prefillStep() // start Beta -- status.json STILL holds reqA's success
	actB, ok, _ := ts.cat.ActivePrefillItem()
	if !ok || actB.AppID != "200" {
		t.Fatalf("expected Beta active, got %+v ok=%v", actB, ok)
	}
	if actB.RequestID == reqA {
		t.Fatalf("Beta must have a fresh request id, not Alpha's")
	}

	// The crux: stepping again must not complete Beta using Alpha's stale status.
	ts.prefillStep()
	again, ok, _ := ts.cat.ActivePrefillItem()
	if !ok || again.AppID != "200" {
		t.Fatalf("Beta was wrongly completed by Alpha's leftover status: %+v ok=%v", again, ok)
	}
}

func TestPrefillWorker_NotOwnedItemFails(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil // 999 not owned
	}
	ts.cat.EnqueuePrefill("steam", "999", "Not Owned")

	ts.prefillStep()

	q, _ := ts.cat.ListPrefillQueue()
	if len(q) != 1 || q[0].Status != "failed" {
		t.Fatalf("an unowned item should be marked failed, got %+v", q)
	}
	if running, _ := ts.sidecar.IsRunning(); running {
		t.Fatalf("an unowned item should not have triggered the sidecar")
	}
}

// TestPrefillWorker_EmptyOwnedDefersNotFails: an empty owned-games result (a
// Steam blip / private profile) must not poison the queue. The item stays
// queued (the worker backs off and retries) rather than being failed as unowned.
func TestPrefillWorker_EmptyOwnedDefersNotFails(t *testing.T) {
	ts := newTestServer(t)
	// fetchOwnedGames returns (nil, nil) -- the no-error, no-games case.
	ts.cat.EnqueuePrefill("steam", "100", "Alpha")

	ts.prefillStep()

	q, _ := ts.cat.ListPrefillQueue()
	if len(q) != 1 || q[0].Status != "queued" {
		t.Fatalf("an empty owned result should leave the item queued (deferred), got %+v", q)
	}
	if running, _ := ts.sidecar.IsRunning(); running {
		t.Fatalf("should not have triggered with an empty owned set")
	}
}

func TestPrefillWorker_InterruptedRunMarkedFailed(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil
	}
	id, _ := ts.seedActivePrefill(t, "100", "Alpha")

	// Sidecar died without writing a status (trigger gone, no status.json).
	os.Remove(filepath.Join(ts.prefillDir, "trigger"))
	ts.prefillStep()

	q, _ := ts.cat.ListPrefillQueue()
	if len(q) != 1 || q[0].ID != id || q[0].Status != "failed" || !strings.Contains(q[0].Error, "interrupted") {
		t.Fatalf("an interrupted run should be marked failed/interrupted, got %+v", q)
	}
}

func TestPrefillCancel_DropsActiveItem(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil
	}
	ts.seedActivePrefill(t, "100", "Alpha")

	req := ts.authedRequest("POST", "/prefill/cancel", nil)
	ts.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if q, _ := ts.cat.ListPrefillQueue(); len(q) != 0 {
		t.Fatalf("cancel should drop the active item, got %+v", q)
	}
	if running, _ := ts.sidecar.IsRunning(); running {
		t.Fatalf("cancel should clear the trigger lock")
	}
}

// TestSteamCallback_NonAllowlistedAccountShowsLoginError covers the
// webui-level surface of KTD13's allowlist rejection.
func TestSteamCallback_NonAllowlistedAccountRedirectsToLoginWithError(t *testing.T) {
	ts := newTestServer(t)
	// No real Steam round-trip is possible in this sandbox (no live
	// credentials, same boundary held throughout this project) -- this
	// exercises the callback handler's own error-rendering path with a
	// request that can't possibly pass verification (steamauth.VerifyCallback
	// itself is covered live/with a stub server in internal/steamauth's
	// own tests).
	req := httptest.NewRequest("GET", "/auth/steam/callback?openid.mode=cancel", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the login page re-rendered with an error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Login failed") && !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("expected an explicit login error rendered, got: %s", rec.Body.String())
	}
}

func TestLogout_ClearsSessionAndRedirects(t *testing.T) {
	ts := newTestServer(t)
	req := ts.authedRequest("POST", "/logout", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected a redirect to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := ts.sessions.Get(ts.sessionID); ok {
		t.Fatalf("expected the session removed after logout")
	}
}

var errTest = &testError{"forced test failure"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// TestSetupGate_UnconfiguredRedirectsEveryRouteToSetup is the setup-gate
// analogue of the auth-gate enumeration: when neither Steam setting is
// configured (env or DB), every route except /setup and static assets
// must redirect to /setup -- there's no usable login to send anyone to
// yet.
func TestSetupGate_UnconfiguredRedirectsEveryRouteToSetup(t *testing.T) {
	ts := newUnconfiguredTestServer(t)
	handler := ts.Handler()

	routes := []string{"/", "/login", "/auth/steam/login", "/library", "/cdn", "/prefill"}
	for _, path := range routes {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
			t.Errorf("%s: expected a redirect to /setup, got %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestSetupGate_SetupAndStaticAccessibleWhenUnconfigured(t *testing.T) {
	ts := newUnconfiguredTestServer(t)
	handler := ts.Handler()

	for _, path := range []string{"/setup", "/static/htmx.min.js"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 while unconfigured, got %d", path, rec.Code)
		}
	}
}

// TestSetupGate_ConfiguredRoutesPassThrough confirms the gate steps aside
// entirely once configured -- the normal auth gate takes over from there
// (covered by the existing auth-gate tests using the already-configured
// fixture).
func TestSetupGate_ConfiguredRoutesPassThrough(t *testing.T) {
	ts := newTestServer(t) // already configured via env in this fixture
	req := httptest.NewRequest("GET", "/library", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	// Falls through to authGate, which redirects to /login (no session)
	// rather than /setup -- proves setupGate didn't intercept it.
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected the auth gate (not the setup gate) to handle this, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSetup_SubmitValidFormPersistsAndRedirectsToLogin(t *testing.T) {
	ts := newUnconfiguredTestServer(t)

	form := url.Values{"steam_web_api_key": {"ABC123"}, "steam_operator_id64": {testOperatorSteamID}}
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected a redirect to /login, got %d %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	webAPIKey, found, err := ts.cat.GetSetting(settingsWebAPIKey)
	if err != nil || !found || webAPIKey != "ABC123" {
		t.Fatalf("expected the API key persisted, got %q found=%v err=%v", webAPIKey, found, err)
	}
	operatorID, found, err := ts.cat.GetSetting(settingsOperatorID64)
	if err != nil || !found || operatorID != testOperatorSteamID {
		t.Fatalf("expected the operator SteamID64 persisted, got %q found=%v err=%v", operatorID, found, err)
	}

	// Now configured -- the gate should let the next request reach the
	// (unauthenticated) login page instead of bouncing back to /setup.
	req2 := httptest.NewRequest("GET", "/login", nil)
	rec2 := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected /login reachable after setup, got %d", rec2.Code)
	}
}

func TestSetup_RejectsMalformedSteamID64(t *testing.T) {
	ts := newUnconfiguredTestServer(t)

	form := url.Values{"steam_web_api_key": {"ABC123"}, "steam_operator_id64": {"not-a-steamid"}}
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("expected the form re-rendered with an error, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, found, _ := ts.cat.GetSetting(settingsOperatorID64); found {
		t.Fatalf("expected nothing persisted for a rejected submission")
	}
}

func TestSetup_RejectsEmptyFields(t *testing.T) {
	ts := newUnconfiguredTestServer(t)

	req := httptest.NewRequest("POST", "/setup", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "required") {
		t.Fatalf("expected an explicit required-fields error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSetup_ClosesOnceConfigured is the load-bearing security property:
// once configured, /setup must not allow re-submission to overwrite the
// allowlisted operator -- otherwise any LAN device could seize control by
// POSTing its own SteamID64.
func TestSetup_ClosesOnceConfigured(t *testing.T) {
	ts := newTestServer(t) // already configured via env

	getRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(getRec, httptest.NewRequest("GET", "/setup", nil))
	if getRec.Code != http.StatusSeeOther || getRec.Header().Get("Location") != "/login" {
		t.Fatalf("expected GET /setup to redirect to /login once configured, got %d %q", getRec.Code, getRec.Header().Get("Location"))
	}

	form := url.Values{"steam_web_api_key": {"ATTACKER-KEY"}, "steam_operator_id64": {"99999999999999999"}}
	postReq := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther || postRec.Header().Get("Location") != "/login" {
		t.Fatalf("expected POST /setup to redirect to /login once configured (not process the submission), got %d %q", postRec.Code, postRec.Header().Get("Location"))
	}
	if _, found, _ := ts.cat.GetSetting(settingsOperatorID64); found {
		t.Fatalf("expected the attacker's submission to never be persisted once already configured")
	}
}

// TestGuestPurgeOrphans_DeclutterAndReclaimBig covers the bulk "Purge orphans"
// button: every orphan fragment (below the min game size, not Pre-Staged/
// monitored) is cleared and the surviving rows re-rendered, while only orphans
// big enough to be worth a full-zone scan are enqueued for nginx eviction.
func TestGuestPurgeOrphans_DeclutterAndReclaimBig(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()

	// A large guest depot (>= the 5 GiB floor) is NOT an orphan: it survives
	// and must still render after the sweep.
	if err := ts.cat.UpsertCacheKey("steam", "depot:5000", "Steam Depot 5000", "steam/depot/5000/chunk/a", 6*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (big guest): %v", err)
	}
	// A medium orphan (>= 1 MiB, below the floor): cleared AND enqueued.
	if err := ts.cat.UpsertCacheKey("steam", "depot:6000", "Steam Depot 6000", "steam/depot/6000/chunk/a", 4*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (medium orphan): %v", err)
	}
	// A tiny sliver (below the guest floor): cleared, not enqueued.
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/manifest", 92, now); err != nil {
		t.Fatalf("UpsertCacheKey (sliver orphan): %v", err)
	}

	req := ts.authedRequest("POST", "/guest/purge-orphans", nil)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "toast-error") {
		t.Fatalf("expected a success (not error) toast, got: %s", body)
	}
	// Re-rendered rows: survivor present, swept orphan gone.
	if !strings.Contains(body, "Steam Depot 5000") {
		t.Fatalf("expected the surviving large guest entry still listed, got: %s", body)
	}
	if strings.Contains(body, "Steam Depot 6000") {
		t.Fatalf("expected the purged orphan removed from the list, got: %s", body)
	}

	// Only the medium orphan's prefix is enqueued for eviction.
	if got := ts.enqueuer.got(); len(got) != 1 || got[0] != "steam/depot/6000/" {
		t.Fatalf("expected only the >=1 MiB orphan prefix enqueued, got %v", got)
	}

	// Catalog: both orphans cleared, the big guest entry intact.
	for _, k := range []string{"depot:6000", "730"} {
		if keys, _ := ts.cat.CacheKeysForGame("steam", k); len(keys) != 0 {
			t.Fatalf("expected orphan %q cleared, still has keys %v", k, keys)
		}
	}
	if keys, _ := ts.cat.CacheKeysForGame("steam", "depot:5000"); len(keys) != 1 {
		t.Fatalf("expected the large guest entry untouched, got keys %v", keys)
	}
}

// TestLibrary_MergesUnmanagedCacheLazily covers the Library page merge: the
// Library page (GET /library) lists identified games AND carries a lazy hook
// for the unmanaged cache, while GET /guest/rows serves that section as a
// fragment with the per-entry Purge control still targeting "closest tr" (the
// depot-row purge fix) and the #guest-rows id intact for the bulk sweep. The
// unmanaged rows must NOT render inline on the Library page, so the lazy Steam
// name resolution never blocks that landing page.
func TestLibrary_MergesUnmanagedCacheLazily(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()

	// An identified game (Library) ...
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 6*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (game): %v", err)
	}
	// ... and a large guest depot (unmanaged cache). Pre-named (no "Steam
	// Depot " prefix) so guestRowViews skips the live Steam Store lookup and
	// this test stays hermetic.
	if err := ts.cat.UpsertCacheKey("steam", "depot:5000", "Big Guest Title", "steam/depot/5000/chunk/a", 6*1024*1024*1024, now); err != nil {
		t.Fatalf("UpsertCacheKey (guest): %v", err)
	}

	// Library page: game listed; buffer present only as a lazy hook, not inline.
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/library", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from Library, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Counter-Strike 2") {
		t.Fatalf("expected the identified game in the Library, got: %s", body)
	}
	if !strings.Contains(body, `hx-get="/guest/rows"`) {
		t.Fatalf("expected the unmanaged cache lazy-load hook, got: %s", body)
	}
	if strings.Contains(body, "Big Guest Title") {
		t.Fatalf("expected the buffer entry to load lazily, not inline on the Library page, got: %s", body)
	}

	// Unmanaged fragment: the guest entry, a Purge control targeting closest tr,
	// and the #guest-rows tbody for the bulk purge swap.
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/guest/rows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from buffer fragment, got %d: %s", rec.Code, rec.Body.String())
	}
	frag := rec.Body.String()
	for _, want := range []string{"Big Guest Title", `id="guest-rows"`, `hx-target="closest tr"`, ">Delete<"} {
		if !strings.Contains(frag, want) {
			t.Fatalf("buffer fragment missing %q, got: %s", want, frag)
		}
	}
}

// TestCacheMax_EvictsLruUnlockedKeepsLocked locks the core of the cache-size
// enforcer: when over the cap it deletes the least-recently-used UNLOCKED game
// first, never a locked one, and stops as soon as the projected size is under
// the cap (no over-eviction).
func TestCacheMax_EvictsLruUnlockedKeepsLocked(t *testing.T) {
	ts := newTestServer(t)
	const gb = int64(1) << 30
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	up := func(key, name string, seen time.Time) {
		if err := ts.cat.UpsertCacheKey("steam", key, name, "steam/depot/"+key+"/chunk/a", 10*gb, seen); err != nil {
			t.Fatalf("UpsertCacheKey %s: %v", key, err)
		}
	}
	up("100", "Old Game", base)                   // LRU: coldest, unlocked
	up("200", "Locked Game", base.Add(time.Hour)) // middle, will be locked
	up("300", "New Game", base.Add(2*time.Hour))  // warmest, unlocked
	if err := ts.cat.SetMonitored("steam", "200", true); err != nil {
		t.Fatalf("SetMonitored: %v", err)
	}

	// Guards: a zero cap disables eviction; under-cap is a no-op.
	if n := ts.evictDownTo(30*gb, 0); n != 0 {
		t.Fatalf("cap 0 must disable eviction, got %d", n)
	}
	if n := ts.evictDownTo(30*gb, 100*gb); n != 0 {
		t.Fatalf("under-cap must not evict, got %d", n)
	}

	// 30 GiB used, 22 GiB cap -> reclaim ~8 GiB -> evict exactly one game, the
	// coldest unlocked (Old Game). Locked Game and New Game stay.
	if n := ts.evictDownTo(30*gb, 22*gb); n != 1 {
		t.Fatalf("expected exactly 1 eviction, got %d", n)
	}
	if k, _ := ts.cat.CacheKeysForGame("steam", "100"); len(k) != 0 {
		t.Fatalf("coldest unlocked game should be evicted, still has %d keys", len(k))
	}
	if k, _ := ts.cat.CacheKeysForGame("steam", "200"); len(k) == 0 {
		t.Fatalf("locked game must never be evicted")
	}
	if k, _ := ts.cat.CacheKeysForGame("steam", "300"); len(k) == 0 {
		t.Fatalf("warmer unlocked game must survive once the cap is met")
	}
	if len(ts.enqueuer.got()) == 0 {
		t.Fatalf("expected purge prefixes enqueued for the evicted game")
	}
}

// TestHero_BackdropAndDashboardStrip verifies the --hero backdrop renders on a
// backdrop page (Library) via the cached-library fallback (owned list is faked
// empty in tests), while the Dashboard carries NO backdrop (option A) and
// instead surfaces the cached game in its largest-cached strip.
func TestHero_BackdropAndDashboardStrip(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "620", "Portal 2", "steam/depot/621/chunk/a", 8<<30, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	lrec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(lrec, ts.authedRequest("GET", "/library", nil))
	lbody := lrec.Body.String()
	if !strings.Contains(lbody, `--hero: url('/posters/steam/620?kind=hero')`) || !strings.Contains(lbody, `class="page-hero"`) {
		t.Fatalf("expected --hero backdrop on Library, got body:\n%s", lbody)
	}

	drec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(drec, ts.authedRequest("GET", "/", nil))
	dbody := drec.Body.String()
	if !strings.Contains(dbody, "Portal 2") || !strings.Contains(dbody, "Largest cached games") {
		t.Fatalf("expected the cached game in the dashboard's largest-cached strip, got:\n%s", dbody)
	}
}

// TestGameDetail_RendersCatalogAndStoreInfo proves the detail page renders both
// catalog data (name, size, Lock/Preload actions) and cached Storefront info,
// reading the Storefront detail from disk so the test never hits the network.
func TestGameDetail_RendersCatalogAndStoreInfo(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "620", "Portal 2", "steam/depot/621/chunk/a", 8<<30, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	// Pre-seed the on-disk Storefront cache so gameStoreDetails reads it
	// instead of calling store.steampowered.com.
	dir := filepath.Join(ts.posterDir, "steam")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(steamStoreView{
		Name: "Portal 2", ShortDescription: "A physics puzzler.",
		ReleaseDate: "Apr 18, 2011", Developers: []string{"Valve"},
		Genres: []string{"Puzzle"}, MetacriticScore: 95,
	})
	if err := os.WriteFile(filepath.Join(dir, "620.appdetails.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/library/steam/620", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// A cached game shows Lock/Delete and NOT Preload (it is already cached).
	for _, want := range []string{"Portal 2", "A physics puzzler.", "Puzzle", "Apr 18, 2011", ">Lock<", ">Delete<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the detail page to contain %q, got:\n%s", want, body)
		}
	}
	// (Assert on the action, not the word "Preload", which also appears as a
	// nav link.) A cached game offers no Preload action.
	if strings.Contains(body, `hx-post="/prefill/trigger"`) {
		t.Fatalf("a cached game's detail page must not offer a Preload action, got:\n%s", body)
	}
}

// TestGameDetail_UncachedSteamAppShowsPreload covers a game reached from the
// Preload grid that is not in the catalog: it still renders (Storefront info +
// a Preload action) rather than 404ing, and hides Lock/Delete.
func TestGameDetail_UncachedSteamAppShowsPreload(t *testing.T) {
	ts := newTestServer(t)
	// Not cached (no UpsertCacheKey). Seed the Storefront cache to stay off
	// the network.
	dir := filepath.Join(ts.posterDir, "steam")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(steamStoreView{Name: "Portal 2", Genres: []string{"Puzzle"}})
	if err := os.WriteFile(filepath.Join(dir, "620.appdetails.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/library/steam/620", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for an uncached Steam app, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Portal 2") || !strings.Contains(body, `hx-post="/prefill/trigger"`) {
		t.Fatalf("expected the uncached app to render with a Preload action, got:\n%s", body)
	}
	if strings.Contains(body, ">Lock<") || strings.Contains(body, ">Delete<") {
		t.Fatalf("an uncached game must not offer Lock/Delete, got:\n%s", body)
	}
	if !strings.Contains(body, "Not cached") {
		t.Fatalf("expected a 'Not cached' indicator, got:\n%s", body)
	}
}

// A non-Steam game that isn't in the catalog 404s (only valid Steam appIDs get
// the synthetic uncached detail page).
func TestGameDetail_UnknownNonSteam404(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/library/epic/some-game", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown non-Steam game, got %d", rec.Code)
	}
}

// TestGameDetail_MonitorViewRendersActions confirms the detail page's Lock
// posts (view=detail) re-render the action cluster, not a grid row.
func TestGameDetail_MonitorViewRendersActions(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 100, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/library/steam/730/monitor?view=detail", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="game-actions"`) || !strings.Contains(body, ">Unlock<") {
		t.Fatalf("expected the detail action cluster with Unlock, got: %s", body)
	}
	if strings.Contains(body, `id="row-steam-730"`) {
		t.Fatalf("detail view must render game_actions, not the grid row, got: %s", body)
	}
}

// TestGameDetail_DeleteViewRedirects confirms a successful delete from the
// detail page sends the browser back to the Library (no grid row to swap).
func TestGameDetail_DeleteViewRedirects(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 1000, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/library/steam/730/delete?view=detail", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/library" {
		t.Fatalf("expected HX-Redirect to /library after a detail delete, got %q", got)
	}
	keys, err := ts.cat.CacheKeysForGame("steam", "730")
	if err != nil || len(keys) != 0 {
		t.Fatalf("expected cache keys cleared after delete, keys=%v err=%v", keys, err)
	}
}

func TestAbout_RendersFAQ(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/about", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /about, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"About Buffarr", "Frequently asked questions", "class=\"faq-item\"", "What is Buffarr", "inspired by", "href=\"/about\"", "How does Buffarr use Steam", "Steam OpenID"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the About page to contain %q, got:\n%s", want, body)
		}
	}
}

func TestFavicon_PublicRedirectToLogo(t *testing.T) {
	ts := newTestServer(t)
	// Reachable without a session (public), and redirects to the embedded logo.
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/favicon.ico", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("expected 301 from /favicon.ico, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/static/logo.png" {
		t.Fatalf("expected redirect to /static/logo.png, got %q", loc)
	}
	// The logo asset itself is served (embedded static).
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/static/logo.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /static/logo.png, got %d", rec.Code)
	}
}

// TestPauseEviction_TimedHandler covers the Settings timed pause-eviction
// control: the page offers a window, a 24h POST persists a future resume time
// (paused), the re-rendered control shows the countdown + Resume, and a resume
// POST clears it.
func TestPauseEviction_TimedHandler(t *testing.T) {
	ts := newTestServer(t)

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/settings", nil))
	if body := rec.Body.String(); !strings.Contains(body, "Pause 24h") {
		t.Fatalf("settings page should offer a timed pause, got: %s", body)
	}

	// Pause 24h -> paused with a countdown.
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/settings/pause-eviction", url.Values{"for": {"24h"}}))
	if !ts.evictionPaused() {
		t.Fatalf("expected eviction paused after a 24h pause")
	}
	if body := rec.Body.String(); !strings.Contains(body, "Paused") || !strings.Contains(body, "resumes in") || !strings.Contains(body, "Resume now") {
		t.Fatalf("control should show Paused + countdown + Resume now, got: %s", body)
	}

	// Resume now -> active again.
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/settings/pause-eviction", url.Values{"for": {"resume"}}))
	if ts.evictionPaused() {
		t.Fatalf("expected eviction active after Resume now")
	}
}

// TestEvictionPause_Expires proves a timed pause auto-resumes: a past resume-at
// timestamp reads as not paused, with no cleanup job.
func TestEvictionPause_Expires(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.cat.SetSetting(settingEvictionPaused, time.Now().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if ts.evictionPaused() {
		t.Fatalf("a past resume-at timestamp must read as not paused")
	}
	if err := ts.cat.SetSetting(settingEvictionPaused, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if !ts.evictionPaused() {
		t.Fatalf("a future resume-at timestamp must read as paused")
	}
}

// TestPauseEviction_GatesCacheEnforcer proves the toggle actually gates the
// enforcer: with a cap the cache is "over," eviction fires only when not paused.
func TestPauseEviction_GatesCacheEnforcer(t *testing.T) {
	ts := newTestServer(t)
	const gb = int64(1) << 30
	if err := ts.cat.UpsertCacheKey("steam", "100", "Old Game", "steam/depot/100/chunk/a", 10*gb, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	// cacheRoot is empty in newTestServer; point it at a real dir so
	// cacheUsedBytes (Statfs) reports a positive used figure.
	ts.cacheRoot = t.TempDir()
	// A 1 MiB cap so the real disk usage is always "over cap" -> eviction would fire.
	if err := ts.cat.SetSetting(settingMaxCacheSize, "1m"); err != nil {
		t.Fatalf("SetSetting cap: %v", err)
	}

	// Paused: the enforcer must skip eviction.
	if err := ts.cat.SetSetting(settingEvictionPaused, "on"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	ts.enforceCacheMax()
	if k, _ := ts.cat.CacheKeysForGame("steam", "100"); len(k) == 0 {
		t.Fatalf("eviction is paused; the game must not be evicted")
	}

	// Resumed: the enforcer evicts the over-cap unlocked game.
	if err := ts.cat.SetSetting(settingEvictionPaused, "off"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	ts.enforceCacheMax()
	if k, _ := ts.cat.CacheKeysForGame("steam", "100"); len(k) != 0 {
		t.Fatalf("eviction resumed; the over-cap game should be evicted, still has %d keys", len(k))
	}
}

// TestDashboard_CachePerformanceCard covers the bandwidth-saved/hit-rate card
// (FEATURES #1): empty state with no data, populated figures once recorded.
func TestDashboard_CachePerformanceCard(t *testing.T) {
	ts := newTestServer(t)

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/", nil))
	if body := rec.Body.String(); !strings.Contains(body, "Cache performance") || !strings.Contains(body, "No cache traffic recorded yet") {
		t.Fatalf("expected the empty cache-performance card, got: %s", body)
	}

	// 10 GiB served, 8 GiB from cache, 100 reqs, 80 HIT -> 80%.
	const gb = int64(1) << 30
	if err := ts.cat.AddBandwidth("2026-06-27", 10*gb, 100, 8*gb, 80); err != nil {
		t.Fatalf("AddBandwidth: %v", err)
	}
	rec = httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"80%", "Served from cache", "pulled from the internet"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the populated card to contain %q, got: %s", want, body)
		}
	}
}

// TestResetBandwidth_Handler covers the Settings "Reset stats" button.
func TestResetBandwidth_Handler(t *testing.T) {
	ts := newTestServer(t)
	const gb = int64(1) << 30
	if err := ts.cat.AddBandwidth("2026-06-27", 10*gb, 100, 8*gb, 80); err != nil {
		t.Fatalf("AddBandwidth: %v", err)
	}
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("POST", "/settings/reset-bandwidth", url.Values{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: expected 200, got %d", rec.Code)
	}
	if tb, _, tr, _, _ := ts.cat.BandwidthTotals(""); tb != 0 || tr != 0 {
		t.Fatalf("expected stats cleared, got %d bytes / %d reqs", tb, tr)
	}
}

// TestJoin_PublicHowToPage covers the attendee join page (FEATURES #8):
// reachable without a session, with the per-platform DNS how-to.
func TestJoin_PublicHowToPage(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/join", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from public /join, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Join the game cache", "How to set your DNS", "Windows", "PlayStation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected /join to contain %q, got: %s", want, body)
		}
	}
}

// TestJoin_ShowsCacheIP confirms BUFFARR_IP is injected into the instructions.
func TestJoin_ShowsCacheIP(t *testing.T) {
	t.Setenv("BUFFARR_IP", "10.0.0.5")
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/join", nil))
	if !strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("expected the cache IP in /join, got: %s", rec.Body.String())
	}
}

func TestPrefill_TriggerMultiAddQueuesOwnedSkipsUnowned(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}, {AppID: 200, Name: "Beta"}}, nil
	}
	req := ts.authedRequest("POST", "/prefill/trigger", url.Values{"app_id": {"100", "200", "999"}})
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Queued 2 games") {
		t.Fatalf("expected a 2-game queued confirmation, got: %s", rec.Body.String())
	}
	q, _ := ts.cat.ListPrefillQueue()
	if len(q) != 2 || q[0].AppID != "100" || q[1].AppID != "200" {
		t.Fatalf("expected 100,200 queued (999 skipped), got %+v", q)
	}
}

func TestQueueRemove_DropsItem(t *testing.T) {
	ts := newTestServer(t)
	ts.cat.EnqueuePrefill("steam", "100", "Alpha")
	q, _ := ts.cat.ListPrefillQueue()

	req := ts.authedRequest("POST", fmt.Sprintf("/prefill/queue/%d/remove", q[0].ID), nil)
	ts.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if got, _ := ts.cat.ListPrefillQueue(); len(got) != 0 {
		t.Fatalf("expected the item removed, got %+v", got)
	}
}

func TestQueueRemove_ActiveItemCancelsRun(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 100, Name: "Alpha"}}, nil
	}
	id, _ := ts.seedActivePrefill(t, "100", "Alpha")

	req := ts.authedRequest("POST", fmt.Sprintf("/prefill/queue/%d/remove", id), nil)
	ts.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if q, _ := ts.cat.ListPrefillQueue(); len(q) != 0 {
		t.Fatalf("expected the active item removed, got %+v", q)
	}
	if running, _ := ts.sidecar.IsRunning(); running {
		t.Fatalf("removing the active item should cancel the run")
	}
}

func TestQueueMove_ReordersViaRoute(t *testing.T) {
	ts := newTestServer(t)
	for _, a := range []string{"100", "200", "300"} {
		ts.cat.EnqueuePrefill("steam", a, "g"+a)
	}
	q, _ := ts.cat.ListPrefillQueue()

	req := ts.authedRequest("POST", fmt.Sprintf("/prefill/queue/%d/up", q[1].ID), nil)
	ts.Handler().ServeHTTP(httptest.NewRecorder(), req)

	q, _ = ts.cat.ListPrefillQueue()
	if q[0].AppID != "200" || q[1].AppID != "100" {
		t.Fatalf("expected 200 moved ahead of 100, got %+v", q)
	}
}

func TestQueueRetry_RequeuesFailed(t *testing.T) {
	ts := newTestServer(t)
	ts.cat.EnqueuePrefill("steam", "100", "Alpha")
	q, _ := ts.cat.ListPrefillQueue()
	ts.cat.MarkPrefillFailed(q[0].ID, "boom")

	req := ts.authedRequest("POST", fmt.Sprintf("/prefill/queue/%d/retry", q[0].ID), nil)
	ts.Handler().ServeHTTP(httptest.NewRecorder(), req)

	q, _ = ts.cat.ListPrefillQueue()
	if len(q) != 1 || q[0].Status != "queued" || q[0].Error != "" {
		t.Fatalf("expected the failed item requeued, got %+v", q)
	}
}

func TestPrefill_StatusRendersQueueCard(t *testing.T) {
	ts := newTestServer(t)
	ts.cat.EnqueuePrefill("steam", "100", "Alpha")
	ts.cat.EnqueuePrefill("steam", "200", "Beta")
	q, _ := ts.cat.ListPrefillQueue()
	ts.cat.MarkPrefillFailed(q[1].ID, "boom") // Beta failed

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/prefill/status", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"Alpha", "Beta",
		fmt.Sprintf("/prefill/queue/%d/up", q[0].ID),     // queued -> reorder
		fmt.Sprintf("/prefill/queue/%d/remove", q[0].ID), // queued -> remove
		fmt.Sprintf("/prefill/queue/%d/retry", q[1].ID),  // failed -> retry
		"Failed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("queue card missing %q, got: %s", want, body)
		}
	}
}

func TestPrefill_PageHasSelectBarAndCheckboxes(t *testing.T) {
	ts := newTestServer(t)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 730, Name: "Counter-Strike 2"}}, nil
	}
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/prefill", nil))
	body := rec.Body.String()

	for _, want := range []string{"prefill-check", "Queue selected", "buffarrQueueSelected", ">Queue<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("prefill page missing %q", want)
		}
	}
	if strings.Contains(body, ">Preload</button>") {
		t.Fatalf("the per-card button should now read Queue, not Preload")
	}
}

func TestJoin_ShowsCachedGamePostersPublicly(t *testing.T) {
	ts := newTestServer(t)
	const GB = int64(1) << 30
	// A cached game above the library floor, plus its poster on disk so
	// handlePoster serves locally (no Steam fetch in the test).
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/a", 6*GB, time.Now()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ts.posterDir, "steam")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "730.jpg"), []byte("\xff\xd8\xff fake jpeg"), 0o644)

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/join", nil))
	body := rec.Body.String()
	for _, want := range []string{"In the cache", "Counter-Strike 2", "/posters/steam/730", "10.0.0.5"} {
		if !strings.Contains(body, want) {
			t.Fatalf("kiosk should show cached posters and the cache IP, missing %q: %s", want, body)
		}
	}

	// The poster URL itself must be reachable without a session.
	prec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(prec, httptest.NewRequest("GET", "/posters/steam/730", nil))
	if prec.Code != http.StatusOK {
		t.Fatalf("expected the poster served publicly (200), got %d", prec.Code)
	}
}

func TestJoin_PhoneInstructionsDropped(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/join", nil))
	body := rec.Body.String()
	for _, gone := range []string{"iPhone", "Android"} {
		if strings.Contains(body, gone) {
			t.Fatalf("phone how-to %q should be dropped", gone)
		}
	}
	if !strings.Contains(body, "Windows") || !strings.Contains(body, "Steam Deck") {
		t.Fatalf("Windows + Steam Deck how-tos should remain")
	}
}

// TestEnforceCacheMax_SkipsWhilePurgeQueueBusy covers the over-eviction guard:
// while a purge is still draining, cacheUsedBytes() (a live Statfs) hasn't seen
// the freed disk yet, so the enforcer must not evict another LRU round. A tiny
// cap guarantees real-FS used bytes exceed it, and an unlocked candidate exists,
// so the only thing that stops eviction is the busy guard.
func TestEnforceCacheMax_SkipsWhilePurgeQueueBusy(t *testing.T) {
	ts := newTestServer(t)
	// cacheUsedBytes() Statfs's cacheRoot; the shared test helper leaves it
	// unset, so point it at a real dir. Statfs reports the whole filesystem's
	// used bytes (many GB), which dwarfs the tiny cap below.
	ts.cacheRoot = t.TempDir()

	if err := ts.cat.SetSetting(settingMaxCacheSize, "1m"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	// Unmonitored game with a cache key -> an eviction candidate.
	if err := ts.cat.UpsertCacheKey("steam", "730", "Counter-Strike 2", "steam/depot/731/chunk/a", 10<<30, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}

	candidates := func() int {
		c, err := ts.cat.EvictionCandidates()
		if err != nil {
			t.Fatalf("EvictionCandidates: %v", err)
		}
		return len(c)
	}
	if candidates() != 1 {
		t.Fatalf("test setup: expected 1 eviction candidate, got %d", candidates())
	}

	// Busy queue: the guard must skip the tick, leaving the candidate intact.
	ts.enqueuer.busy = true
	ts.enforceCacheMax()
	if candidates() != 1 {
		t.Fatalf("expected no eviction while purge queue is busy, candidate was evicted")
	}

	// Idle queue: the enforcer evicts, clearing the candidate's catalog rows.
	ts.enqueuer.busy = false
	ts.enforceCacheMax()
	if candidates() != 0 {
		t.Fatalf("expected the candidate to be evicted once the purge queue is idle, still present")
	}
}

type posterRoundTripFunc func(*http.Request) (*http.Response, error)

func (f posterRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestHandlePoster_TransientErrorNotCached: a network blip while fetching art
// must not write a .miss, or that one blip permanently blanks the poster (the
// .miss short-circuits every future load).
func TestHandlePoster_TransientErrorNotCached(t *testing.T) {
	ts := newTestServer(t)
	ts.httpClient = &http.Client{Transport: posterRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp: connection refused")
	})}

	rec := httptest.NewRecorder()
	// Authenticated: a poster fetch is gated behind a session (see the
	// disk-fill fix), so the fetch path is only reached with one.
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/posters/steam/730", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on fetch failure, got %d", rec.Code)
	}
	miss := filepath.Join(ts.posterDir, "steam", "730.jpg.miss")
	if _, err := os.Stat(miss); err == nil {
		t.Fatalf("a transient fetch error must NOT write a .miss marker")
	}
}

// TestHandlePoster_DefinitiveMissCached: when every candidate returns a clean
// 404 (no art published), a .miss is cached so the CDN isn't re-hit forever.
func TestHandlePoster_DefinitiveMissCached(t *testing.T) {
	ts := newTestServer(t)
	ts.httpClient = &http.Client{Transport: posterRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
	})}

	rec := httptest.NewRecorder()
	// Authenticated: the fetch path (which writes the .miss) is session-gated.
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/posters/steam/730", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no art published, got %d", rec.Code)
	}
	miss := filepath.Join(ts.posterDir, "steam", "730.jpg.miss")
	if _, err := os.Stat(miss); err != nil {
		t.Fatalf("a definitive 404 for all candidates should cache a .miss: %v", err)
	}
}

// TestGuestRowViews_NegativeCachesFailedDepotResolution: a depot whose Store
// lookup fails must not be retried on every guest render -- that serializes
// multiple 15s Store calls into the request and can 429 the endpoint (#7).
func TestGuestRowViews_NegativeCachesFailedDepotResolution(t *testing.T) {
	ts := newTestServer(t)
	// A guest depot above the library floor so it appears in GuestGames.
	if err := ts.cat.UpsertCacheKey("steam", "depot:5000", "Steam Depot 5000", "steam/depot/5000/chunk/a", 6<<30, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
	// Every Store call "fails" (empty body -> decode error -> no name). The
	// transport runs synchronously on this goroutine, so a plain counter is safe.
	var calls int
	ts.httpClient = &http.Client{Transport: posterRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}

	ts.guestRowViews(steamauth.Session{})
	first := calls
	if first == 0 {
		t.Fatalf("expected the first render to attempt Store resolution")
	}

	ts.guestRowViews(steamauth.Session{})
	if calls != first {
		t.Fatalf("second render should skip the negative-cached depot; Store calls went %d -> %d", first, calls)
	}
}

type stubRoundTripFunc func(*http.Request) (*http.Response, error)

func (f stubRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRequestBaseURL_ProxyHeadersGatedByOptIn: X-Forwarded-Proto/Host are
// honored only when trustProxyHeaders is set, so a spoofed header can't steer
// the OpenID return_to on a direct (non-proxied) deployment.
func TestRequestBaseURL_ProxyHeadersGatedByOptIn(t *testing.T) {
	ts := newTestServer(t)
	req := httptest.NewRequest("GET", "http://backend.local:9080/auth/steam", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "buffarr.example.com")

	if got := ts.requestBaseURL(req); got != "http://backend.local:9080" {
		t.Fatalf("trust off: forwarded headers must be ignored, got %q", got)
	}

	ts.trustProxyHeaders = true
	if got := ts.requestBaseURL(req); got != "https://buffarr.example.com" {
		t.Fatalf("trust on: forwarded headers must win, got %q", got)
	}
}

// TestHandlePoster_UnauthenticatedDoesNotFetch: an uncached poster requested
// without a session must 404 without hitting the CDN or writing a .miss --
// otherwise any LAN device is a disk-fill vector (#6).
func TestHandlePoster_UnauthenticatedDoesNotFetch(t *testing.T) {
	ts := newTestServer(t)
	var calls int
	ts.httpClient = &http.Client{Transport: stubRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})}

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/posters/steam/730", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an uncached poster without a session, got %d", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("unauthenticated poster request must not trigger a CDN fetch, made %d", calls)
	}
	if _, err := os.Stat(filepath.Join(ts.posterDir, "steam", "730.jpg.miss")); err == nil {
		t.Fatalf("unauthenticated request must not write a .miss")
	}
}

// TestHandlePoster_AuthenticatedTriggersFetch: with a session, an uncached
// poster does reach the fetch path (the gate lets the operator populate art).
func TestHandlePoster_AuthenticatedTriggersFetch(t *testing.T) {
	ts := newTestServer(t)
	var calls int
	ts.httpClient = &http.Client{Transport: stubRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
	})}

	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/posters/steam/730", nil))
	if calls == 0 {
		t.Fatalf("authenticated poster request should trigger a CDN fetch")
	}
}

// TestUnknownPathReturns404 pins that the "GET /" catch-all no longer renders
// the Dashboard for arbitrary paths -- a typo'd or stale URL must 404.
func TestUnknownPathReturns404(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/definitely-not-a-page", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: got %d, want 404", rec.Code)
	}
}

// TestPreloadAliasRedirectsToPrefill covers the /preload -> /prefill alias: the
// UI calls the page "Preload", so that URL must resolve rather than 404.
func TestPreloadAliasRedirectsToPrefill(t *testing.T) {
	ts := newTestServer(t)
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/preload", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("/preload: got %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/prefill" {
		t.Fatalf("/preload Location: got %q, want /prefill", loc)
	}
}

// TestCapacityGaugeRenders drives the capacity gauge partial end-to-end: with a
// real cacheRoot, Statfs populates the segment data and the prefill status
// fragment must render the segmented track and legend. Existing tests use an
// empty cacheRoot, which skips the gauge, so this is the only coverage of the
// partial actually executing with data.
func TestCapacityGaugeRenders(t *testing.T) {
	ts := newTestServer(t)
	ts.Server.cacheRoot = t.TempDir() // real path -> Statfs succeeds -> gauge has data
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/prefill/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("prefill status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"gauge-track", "gauge-seg--locked", "gauge-seg--evictable", "Locked", "Evictable", "Free"} {
		if !strings.Contains(body, want) {
			t.Fatalf("gauge render missing %q\nbody: %s", want, body)
		}
	}
}

// TestDashboardTilesLinkToFilteredViews pins U11: the dashboard stat tiles are
// links into the relevant views (Locked -> filtered Library, Unmanaged -> Guest)
// rather than inert boxes.
func TestDashboardTilesLinkToFilteredViews(t *testing.T) {
	ts := newTestServer(t)
	ts.Server.cacheRoot = t.TempDir() // Statfs succeeds -> the hero/tiles block renders
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, ts.authedRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`href="/library"`, `href="/library?filter=locked"`, `href="/guest"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard tiles missing link %q", want)
		}
	}
}
