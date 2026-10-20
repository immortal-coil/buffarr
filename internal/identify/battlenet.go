// Package identify resolves a per-CDN request into the game-level identity
// R3 asks for, where that's feasible (Steam, Battle.net); everything else
// falls back to the platform-level Unidentified bucket.
package identify

import "regexp"

// ngdpPrefixToGame is a maintained table (KTD per R3): Battle.net's NGDP
// path structure carries a game-specific prefix, but no public API maps it
// to a name, so this is hand-maintained. Deliberately small and limited to
// the prefixes the project's own brainstorm research already verified
// (docs/brainstorms/2026-06-17-lancache-library-manager-requirements.md);
// extend it as new prefixes are confirmed rather than guessing at one.
// Anything not in this table -- including real, currently-unlisted
// Battle.net titles -- falls back to UnidentifiedKey("blizzard"), which is
// the R3-sanctioned behavior for this table, not a bug.
var ngdpPrefixToGame = map[string]string{
	"wow": "World of Warcraft",
	"d3":  "Diablo III",
	"ovw": "Overwatch",
}

var battlenetPathRE = regexp.MustCompile(`^/tpr/([a-z0-9_]+)/`)

// ResolveBattlenet extracts a Battle.net NGDP path prefix from a request
// URI and maps it to a game name via ngdpPrefixToGame. prefix is returned
// alongside name so the caller can build a stable gameKey even when the
// prefix maps to nothing (R3's Unidentified fallback still wants the
// platform-level bucket, not a dropped request).
func ResolveBattlenet(uri string) (prefix, name string, ok bool) {
	m := battlenetPathRE.FindStringSubmatch(uri)
	if m == nil {
		return "", "", false
	}
	prefix = m[1]
	name, known := ngdpPrefixToGame[prefix]
	return prefix, name, known
}
