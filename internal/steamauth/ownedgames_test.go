package steamauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/immortal-coil/buffarr/internal/identify"
)

// steamWebAPIStub stands in for api.steampowered.com, redirecting
// GetOwnedGames' hardcoded request URL to a local test server via a
// custom Transport (the API client has no base-URL override, same as
// openid_test.go's redirectTransport).
func steamWebAPIStub(t *testing.T, handler http.HandlerFunc) *identify.HTTPSteamAPI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	api := identify.NewHTTPSteamAPI("fake-key")
	api.HTTPClient = &http.Client{Transport: redirectTransport{target: srv.URL}}
	return api
}

// TestGetOwnedGames_HappyPath covers the plan's happy path indirectly: a
// successful Steam Web API call lists the account's games. steamauth.
// GetOwnedGames is a thin re-export of internal/identify's HTTPSteamAPI,
// so this also closes a gap that package's own tests left open (its tests
// cover SteamResolver's logic against a fake API, but never exercised
// HTTPSteamAPI's real HTTP/JSON wire format end to end).
func TestGetOwnedGames_HappyPath(t *testing.T) {
	api := steamWebAPIStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response":{"game_count":1,"games":[{"appid":730,"name":"Counter-Strike 2"}]}}`))
	})

	games, err := api.GetOwnedGames("76561197960435530")
	if err != nil {
		t.Fatalf("GetOwnedGames: %v", err)
	}
	if len(games) != 1 || games[0].Name != "Counter-Strike 2" || games[0].AppID != 730 {
		t.Fatalf("unexpected games: %+v", games)
	}
}

// TestGetOwnedGames_APIErrorIsDistinctFromEmptyList covers the plan's
// error path: Steam's Web API being unreachable must be distinguishable
// from a legitimately empty owned-games list, so a caller (U9's picker)
// can show an error state instead of "no games."
func TestGetOwnedGames_APIErrorIsDistinctFromEmptyList(t *testing.T) {
	api := steamWebAPIStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	games, err := api.GetOwnedGames("76561197960435530")
	if err == nil {
		t.Fatalf("expected an error when the API is unreachable, not a successful empty list")
	}
	if games != nil {
		t.Fatalf("expected no games returned alongside an error, got %v", games)
	}
}

func TestGetOwnedGames_LegitimatelyEmptyListIsNotAnError(t *testing.T) {
	api := steamWebAPIStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response":{"game_count":0,"games":[]}}`))
	})

	games, err := api.GetOwnedGames("76561197960435530")
	if err != nil {
		t.Fatalf("expected a legitimately empty list to succeed, got: %v", err)
	}
	if len(games) != 0 {
		t.Fatalf("expected zero games, got %v", games)
	}
}
