package web

// Tests for the /api/v1/ JSON API: key auth in authGate, and the frozen v1
// response shapes.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/immortal-coil/buffarr/internal/steamauth"
)

const testAPIKey = "test-api-key-123"

// apiRequest builds an /api/ request carrying the key in X-Api-Key.
func apiRequest(method, path, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Api-Key", testAPIKey)
	return req
}

// seedAPIGame inserts a game large enough to clear the Library size floor
// (catalog.MinLibrarySizeBytes, 5 GiB).
func seedAPIGame(t *testing.T, ts *testServer, appID, name string) {
	t.Helper()
	if err := ts.cat.UpsertCacheKey("steam", appID, name, "steam/depot/"+appID+"1/chunk/a", 6<<30, time.Now()); err != nil {
		t.Fatalf("UpsertCacheKey: %v", err)
	}
}

func doAPI(ts *testServer, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAPI_DisabledWhenKeyUnset(t *testing.T) {
	ts := newTestServer(t) // no SetAPIKey call
	rec := doAPI(ts, apiRequest("GET", "/api/v1/games", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when API disabled, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_RejectsMissingOrWrongKey(t *testing.T) {
	ts := newTestServer(t)
	ts.SetAPIKey(testAPIKey)

	req := httptest.NewRequest("GET", "/api/v1/games", nil) // no key at all
	if rec := doAPI(ts, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without key, got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/v1/games", nil)
	req.Header.Set("X-Api-Key", "wrong")
	if rec := doAPI(ts, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong key, got %d", rec.Code)
	}

	// A response must be JSON, not the webui's login redirect.
	req = httptest.NewRequest("GET", "/api/v1/games", nil)
	rec := doAPI(ts, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected JSON error response, got Content-Type %q", ct)
	}
}

func TestAPI_BearerAuthWorks(t *testing.T) {
	ts := newTestServer(t)
	ts.SetAPIKey(testAPIKey)
	req := httptest.NewRequest("GET", "/api/v1/games", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	if rec := doAPI(ts, req); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with Bearer auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_ListAndGetGames(t *testing.T) {
	ts := newTestServer(t)
	ts.SetAPIKey(testAPIKey)
	seedAPIGame(t, ts, "730", "Counter-Strike 2")

	rec := doAPI(ts, apiRequest("GET", "/api/v1/games", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Games []struct {
			Platform  string `json:"platform"`
			GameKey   string `json:"game_key"`
			Name      string `json:"name"`
			Locked    bool   `json:"locked"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"games"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if len(list.Games) != 1 || list.Games[0].GameKey != "730" || list.Games[0].Name != "Counter-Strike 2" || list.Games[0].SizeBytes != 6<<30 {
		t.Fatalf("unexpected list payload: %+v", list.Games)
	}

	rec = doAPI(ts, apiRequest("GET", "/api/v1/games/steam/730", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doAPI(ts, apiRequest("GET", "/api/v1/games/steam/99999", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: expected 404, got %d", rec.Code)
	}
}

func TestAPI_LockUnlockDelete(t *testing.T) {
	ts := newTestServer(t)
	ts.SetAPIKey(testAPIKey)
	seedAPIGame(t, ts, "730", "Counter-Strike 2")

	// Lock, verify the catalog and the response agree.
	rec := doAPI(ts, apiRequest("POST", "/api/v1/games/steam/730/lock", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"locked":true`) {
		t.Fatalf("lock: expected 200 locked=true, got %d: %s", rec.Code, rec.Body.String())
	}
	if entry, _, _ := ts.cat.GetGame("steam", "730"); !entry.Monitored {
		t.Fatal("lock: catalog not monitored after lock")
	}

	// Delete while locked is refused with 409.
	rec = doAPI(ts, apiRequest("DELETE", "/api/v1/games/steam/730", ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete locked: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unlock, then delete succeeds and enqueues a purge.
	rec = doAPI(ts, apiRequest("POST", "/api/v1/games/steam/730/unlock", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"locked":false`) {
		t.Fatalf("unlock: expected 200 locked=false, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doAPI(ts, apiRequest("DELETE", "/api/v1/games/steam/730", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ts.enqueuer.mu.Lock()
	enqueued := len(ts.enqueuer.prefixes)
	ts.enqueuer.mu.Unlock()
	if enqueued == 0 {
		t.Fatal("delete: expected purge prefixes to be enqueued")
	}

	// Deleting an already-gone game is idempotent success.
	rec = doAPI(ts, apiRequest("DELETE", "/api/v1/games/steam/730", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-delete: expected idempotent 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Locking a game that does not exist 404s.
	rec = doAPI(ts, apiRequest("POST", "/api/v1/games/steam/99999/lock", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("lock missing: expected 404, got %d", rec.Code)
	}
}

func TestAPI_Stats(t *testing.T) {
	ts := newTestServer(t)
	ts.SetAPIKey(testAPIKey)
	seedAPIGame(t, ts, "730", "Counter-Strike 2")

	rec := doAPI(ts, apiRequest("GET", "/api/v1/stats", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var stats struct {
		CachedGames int `json:"cached_games"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decoding stats: %v", err)
	}
	if stats.CachedGames != 1 {
		t.Fatalf("expected cached_games=1, got %+v", stats)
	}
}

func TestAPI_PrefillTriggerAndStatus(t *testing.T) {
	ts := newTestServer(t)
	ts.SetAPIKey(testAPIKey)
	ts.fetchOwnedGames = func(string, string) ([]steamauth.OwnedGame, error) {
		return []steamauth.OwnedGame{{AppID: 730, Name: "Counter-Strike 2"}}, nil
	}

	// Un-owned IDs are skipped, owned ones queue.
	rec := doAPI(ts, apiRequest("POST", "/api/v1/prefill", `{"app_ids": [730, 999]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var trig struct {
		Queued  int   `json:"queued"`
		Already int   `json:"already_queued"`
		Skipped []int `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trig); err != nil {
		t.Fatalf("decoding trigger response: %v", err)
	}
	if trig.Queued != 1 || len(trig.Skipped) != 1 || trig.Skipped[0] != 999 {
		t.Fatalf("unexpected trigger result: %+v", trig)
	}

	// Re-queuing is reported as already_queued, not an error.
	rec = doAPI(ts, apiRequest("POST", "/api/v1/prefill", `{"app_ids": [730]}`))
	if err := json.Unmarshal(rec.Body.Bytes(), &trig); err != nil {
		t.Fatalf("decoding second trigger response: %v", err)
	}
	if trig.Queued != 0 || trig.Already != 1 {
		t.Fatalf("expected already_queued=1, got %+v", trig)
	}

	// The queue shows up in status.
	rec = doAPI(ts, apiRequest("GET", "/api/v1/prefill", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var status struct {
		Running bool `json:"running"`
		Queue   []struct {
			AppID  string `json:"app_id"`
			Status string `json:"status"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if len(status.Queue) != 1 || status.Queue[0].AppID != "730" || status.Queue[0].Status != "queued" {
		t.Fatalf("unexpected queue: %+v", status.Queue)
	}

	// Malformed and empty bodies are 400s.
	if rec := doAPI(ts, apiRequest("POST", "/api/v1/prefill", `not json`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d", rec.Code)
	}
	if rec := doAPI(ts, apiRequest("POST", "/api/v1/prefill", `{"app_ids": []}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty app_ids: expected 400, got %d", rec.Code)
	}
}

// TestAPI_BypassesSetupGate confirms an unconfigured deployment serves JSON
// (not a /setup redirect) so monitoring works before first-run setup.
func TestAPI_BypassesSetupGate(t *testing.T) {
	ts := newUnconfiguredTestServer(t)
	ts.SetAPIKey(testAPIKey)
	rec := doAPI(ts, apiRequest("GET", "/api/v1/stats", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from an unconfigured server, got %d: %s", rec.Code, rec.Body.String())
	}
}
