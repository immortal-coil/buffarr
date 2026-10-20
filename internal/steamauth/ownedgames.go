package steamauth

import "github.com/immortal-coil/buffarr/internal/identify"

// OwnedGame is one game in the operator's Steam library (R12).
type OwnedGame = identify.OwnedApp

// GetOwnedGames fetches the authenticated operator's owned Steam games
// (R12), reusing the same GetOwnedGames call U4's depot resolver makes
// rather than a second, independent implementation of the same Steam Web
// API request.
//
// A non-nil error here means the API call itself failed (network error,
// non-200 status, bad response body) -- distinct from a legitimately empty
// or private-profile result, which succeeds with a zero-length slice. This
// distinction is what lets a caller show an explicit error state instead
// of presenting "API unreachable" as if it were "no games" (R12's error
// path).
func GetOwnedGames(webAPIKey, steamID64 string) ([]OwnedGame, error) {
	return identify.NewHTTPSteamAPI(webAPIKey).GetOwnedGames(steamID64)
}
