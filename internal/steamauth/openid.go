// Package steamauth implements Steam's OpenID 2.0 login flow by hand
// (KTD8): the most-used Go library for this (markbates/goth's Steam
// provider) skips the security-critical check_authentication
// re-verification step entirely, and no actively maintained, correctly
// implemented alternative exists.
package steamauth

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// steamOpenIDEndpoint is Steam's documented OP Endpoint URL
// (partner.steamgames.com/doc/features/auth: "Configure your OpenID
// library to use the following URL as Steam's OP Endpoint URL").
const steamOpenIDEndpoint = "https://steamcommunity.com/openid/login"

const openIDNamespace = "http://specs.openid.net/auth/2.0"

// claimedIDRE extracts a SteamID64 from a claimed_id value. Steam's own
// docs give the format as "http://steamcommunity.com/openid/id/<steamid>"
// but the live endpoint actually returns "https://" -- confirmed live
// against steamcommunity.com -- so the scheme is optional here rather than
// hardcoded to either.
var claimedIDRE = regexp.MustCompile(`^https?://steamcommunity\.com/openid/id/(\d{17})$`)

// LoginRedirectURL builds the URL to redirect the operator to for Steam's
// login page (checkid_setup mode). returnTo is where Steam redirects back
// to after authentication; realm is this site's base URL (the scope Steam
// asks the user to authorize).
func LoginRedirectURL(returnTo, realm string) string {
	v := url.Values{
		"openid.ns":         {openIDNamespace},
		"openid.mode":       {"checkid_setup"},
		"openid.return_to":  {returnTo},
		"openid.realm":      {realm},
		"openid.identity":   {"http://specs.openid.net/auth/2.0/identifier_select"},
		"openid.claimed_id": {"http://specs.openid.net/auth/2.0/identifier_select"},
	}
	return steamOpenIDEndpoint + "?" + v.Encode()
}

var (
	// ErrNotAuthenticated covers any callback that fails OpenID
	// verification: wrong mode, malformed claimed_id, an unsigned
	// claimed_id/identity (the spec violation an attacker forging an
	// assertion would have to commit), or check_authentication returning
	// is_valid:false (a forged or tampered callback).
	ErrNotAuthenticated = errors.New("steam openid verification failed")
)

// VerifyCallback re-POSTs the callback's openid.* parameters back to Steam
// with openid.mode=check_authentication (OpenID 2.0's stateless
// verification mode, spec section 11.4.2) and, only if Steam confirms the
// assertion is valid, extracts and returns the SteamID64. It does not
// check any operator allowlist -- that's a separate, deliberately later
// step (KTD13; see CompleteLogin).
//
// expectedReturnTo is this server's own callback URL. The assertion's
// return_to must be signed and match it exactly (spec section 11.1):
// otherwise a signed assertion Steam issued to some other site's "Sign in
// with Steam" could be replayed here, and check_authentication would still
// report it valid.
func VerifyCallback(client *http.Client, params url.Values, expectedReturnTo string) (steamID64 string, err error) {
	if params.Get("openid.ns") != openIDNamespace {
		return "", fmt.Errorf("%w: unexpected or missing openid.ns", ErrNotAuthenticated)
	}
	if params.Get("openid.mode") != "id_res" {
		return "", fmt.Errorf("%w: unexpected openid.mode %q", ErrNotAuthenticated, params.Get("openid.mode"))
	}

	claimedID := params.Get("openid.claimed_id")
	if claimedID == "" {
		return "", fmt.Errorf("%w: missing openid.claimed_id", ErrNotAuthenticated)
	}

	// Spec section 10.1 (Positive Assertions): "signed" MUST include
	// claimed_id and identity whenever those fields are present in the
	// response. An OP that omits them from the signed set while still
	// including them in the assertion would let an attacker substitute a
	// different identity into an otherwise-validly-signed response --
	// trusting claimed_id without this check is exactly the class of
	// mistake KTD8 hand-rolls this flow to avoid.
	signed := strings.Split(params.Get("openid.signed"), ",")
	if !containsAll(signed, "claimed_id", "identity", "return_to") {
		return "", fmt.Errorf("%w: claimed_id/identity/return_to not covered by openid.signed", ErrNotAuthenticated)
	}
	if got := params.Get("openid.return_to"); got != expectedReturnTo {
		return "", fmt.Errorf("%w: openid.return_to %q does not match %q", ErrNotAuthenticated, got, expectedReturnTo)
	}

	verifyParams := url.Values{}
	for k, v := range params {
		verifyParams[k] = v
	}
	verifyParams.Set("openid.mode", "check_authentication")

	resp, err := client.PostForm(steamOpenIDEndpoint, verifyParams)
	if err != nil {
		return "", fmt.Errorf("contacting steam for check_authentication: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading check_authentication response: %w", err)
	}
	if !isValidAssertion(string(body)) {
		return "", fmt.Errorf("%w: check_authentication did not return is_valid:true", ErrNotAuthenticated)
	}

	m := claimedIDRE.FindStringSubmatch(claimedID)
	if m == nil {
		return "", fmt.Errorf("%w: claimed_id %q is not a recognized Steam identity URL", ErrNotAuthenticated, claimedID)
	}
	return m[1], nil
}

// isValidAssertion parses check_authentication's key:value, newline-
// separated response body (confirmed live against steamcommunity.com;
// e.g. "ns:http://specs.openid.net/auth/2.0\nis_valid:false\n").
func isValidAssertion(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && k == "is_valid" && v == "true" {
			return true
		}
	}
	return false
}

func containsAll(haystack []string, needles ...string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			return false
		}
	}
	return true
}

// ErrNotAllowlisted is returned by CompleteLogin when a callback passes
// OpenID verification (it really is some real Steam account) but the
// resulting SteamID64 doesn't match the configured operator (KTD13).
var ErrNotAllowlisted = errors.New("steam account is not the allowlisted operator")

// CompleteLogin runs the full login decision (KTD13): verify the callback,
// then require the resulting SteamID64 to match operatorSteamID64 exactly
// -- check_authentication only proves the callback came from Steam for
// *some* real account, not that it's the intended operator. Only a match
// establishes a session.
func CompleteLogin(client *http.Client, params url.Values, returnTo, operatorSteamID64 string, sessions *Manager) (sessionID, csrfToken string, err error) {
	steamID64, err := VerifyCallback(client, params, returnTo)
	if err != nil {
		return "", "", err
	}
	if steamID64 != operatorSteamID64 {
		return "", "", ErrNotAllowlisted
	}
	return sessions.Create(steamID64)
}
