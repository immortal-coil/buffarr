package identify

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

type fakeSteamAPI struct {
	ownedGames    []OwnedApp
	ownedGamesErr error
	depots        map[int][]int
	depotsErr     map[int]error
}

func (f *fakeSteamAPI) GetOwnedGames(steamID64 string) ([]OwnedApp, error) {
	return f.ownedGames, f.ownedGamesErr
}

func (f *fakeSteamAPI) GetAppDepots(appID int) ([]int, int64, error) {
	if err, ok := f.depotsErr[appID]; ok {
		return nil, 0, err
	}
	return f.depots[appID], 0, nil
}

func newTestResolver(api SteamAPI) *SteamResolver {
	r := NewSteamResolver(api, "76561197960435530")
	r.syncDelay = 0
	return r
}

func TestSteamResolver_ResolveBeforeSyncIsUnidentified(t *testing.T) {
	r := newTestResolver(&fakeSteamAPI{})
	if _, _, ok := r.Resolve(730); ok {
		t.Fatalf("expected no resolution before Sync has run")
	}
}

func TestSteamResolver_HappyPath(t *testing.T) {
	api := &fakeSteamAPI{
		ownedGames: []OwnedApp{{AppID: 730, Name: "Counter-Strike 2"}},
		depots:     map[int][]int{730: {731, 732}},
	}
	r := newTestResolver(api)

	if err := r.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	appID, name, ok := r.Resolve(731)
	if !ok || appID != 730 || name != "Counter-Strike 2" {
		t.Fatalf("expected depot 731 -> app 730 (Counter-Strike 2), got appID=%d name=%q ok=%v", appID, name, ok)
	}
}

func TestSteamResolver_UnmappedDepotStaysUnidentified(t *testing.T) {
	api := &fakeSteamAPI{
		ownedGames: []OwnedApp{{AppID: 730, Name: "Counter-Strike 2"}},
		depots:     map[int][]int{730: {731}},
	}
	r := newTestResolver(api)
	if err := r.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, _, ok := r.Resolve(999999); ok {
		t.Fatalf("expected an unowned/unmapped depot to not resolve")
	}
}

func TestSteamResolver_OneAppFailingDoesNotAbortSync(t *testing.T) {
	api := &fakeSteamAPI{
		ownedGames: []OwnedApp{
			{AppID: 1, Name: "Broken App"},
			{AppID: 730, Name: "Counter-Strike 2"},
		},
		depots: map[int][]int{730: {731}},
		depotsErr: map[int]error{
			1: errors.New("steamcmd.net: simulated failure"),
		},
	}
	r := newTestResolver(api)
	if err := r.Sync(); err != nil {
		t.Fatalf("expected Sync to tolerate one app's depot-lookup failure, got: %v", err)
	}

	appID, _, ok := r.Resolve(731)
	if !ok || appID != 730 {
		t.Fatalf("expected the other app's depots to still resolve despite the failure, appID=%d ok=%v", appID, ok)
	}
}

func TestSteamResolver_GetOwnedGamesErrorDoesNotPanic(t *testing.T) {
	api := &fakeSteamAPI{ownedGamesErr: errors.New("API unreachable")}
	r := newTestResolver(api)

	if err := r.Sync(); err == nil {
		t.Fatalf("expected Sync to surface the GetOwnedGames error")
	}
	if _, _, ok := r.Resolve(730); ok {
		t.Fatalf("expected no resolution after a failed sync")
	}
}

func TestSteamResolver_EmptyOwnedGamesIsNotAnError(t *testing.T) {
	// A private profile returns an empty list even with a valid key -- this
	// must not be treated as a hard failure (see Sync's doc comment).
	r := newTestResolver(&fakeSteamAPI{ownedGames: []OwnedApp{}})
	if err := r.Sync(); err != nil {
		t.Fatalf("expected an empty owned-games list to not be an error, got: %v", err)
	}
}

func TestSteamResolver_ReSyncReplacesStaleEntries(t *testing.T) {
	api := &fakeSteamAPI{
		ownedGames: []OwnedApp{{AppID: 730, Name: "Counter-Strike 2"}},
		depots:     map[int][]int{730: {731}},
	}
	r := newTestResolver(api)
	if err := r.Sync(); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	// Simulate the owned-games list changing between syncs (e.g. a game
	// removed from the account, or a depot reassigned).
	api.ownedGames = []OwnedApp{{AppID: 440, Name: "Team Fortress 2"}}
	api.depots = map[int][]int{440: {441}}
	if err := r.Sync(); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	if _, _, ok := r.Resolve(731); ok {
		t.Fatalf("expected the stale depot 731 to no longer resolve after re-sync")
	}
	appID, name, ok := r.Resolve(441)
	if !ok || appID != 440 || name != "Team Fortress 2" {
		t.Fatalf("expected depot 441 -> app 440, got appID=%d name=%q ok=%v", appID, name, ok)
	}
}

// TestParseSteamcmdDepots covers the size extraction against steamcmd.net's
// current JSON shape (size nested under manifests.public), the size/maxsize
// fallbacks, and skipping non-numeric depot keys. This guards the exact
// regression where steamcmd.net dropped the flat "maxsize" field and every
// app's estimated size silently became 0.
func TestParseSteamcmdDepots(t *testing.T) {
	body := []byte(`{"data":{"620":{"depots":{
		"621":{"manifests":{"public":{"download":"7509295520","size":"12674356162"}}},
		"622":{"manifests":{"public":{"size":"2000"}}},
		"623":{"maxsize":"500"},
		"228990":{"depotfromapp":"228980","sharedinstall":"1","maxsize":"9999"},
		"branches":{"public":{"buildid":"99"}}
	}}}}`)
	depots, total, err := parseSteamcmdDepots(body, 620)
	if err != nil {
		t.Fatalf("parseSteamcmdDepots: %v", err)
	}
	if len(depots) != 3 {
		t.Fatalf("expected 3 own depots (non-numeric keys and shared depotfromapp skipped), got %v", depots)
	}
	// 621 uses download; 622 falls back to size; 623 falls back to legacy maxsize.
	want := int64(7509295520 + 2000 + 500)
	if total != want {
		t.Fatalf("expected total %d, got %d", want, total)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: i/o timeout")
}

// Callers log GetOwnedGames errors, so a transport failure must not echo the
// request URL's key= query parameter.
func TestHTTPSteamAPI_GetOwnedGamesErrorOmitsKey(t *testing.T) {
	api := &HTTPSteamAPI{WebAPIKey: "SECRETKEY123", HTTPClient: &http.Client{Transport: failingTransport{}}}
	_, err := api.GetOwnedGames("76561197960435530")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SECRETKEY123") {
		t.Fatalf("error leaks the API key: %v", err)
	}
}
