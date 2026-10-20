package steamauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLoginRedirectURL_HasRequiredParams(t *testing.T) {
	got := LoginRedirectURL("https://cache.local/auth/steam/callback", "https://cache.local")

	if !strings.HasPrefix(got, steamOpenIDEndpoint+"?") {
		t.Fatalf("expected the URL to target steam's OP endpoint, got %q", got)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing redirect URL: %v", err)
	}
	q := u.Query()
	if q.Get("openid.mode") != "checkid_setup" {
		t.Fatalf("expected openid.mode=checkid_setup, got %q", q.Get("openid.mode"))
	}
	if q.Get("openid.ns") != openIDNamespace {
		t.Fatalf("expected the OpenID 2.0 namespace, got %q", q.Get("openid.ns"))
	}
	if q.Get("openid.return_to") != "https://cache.local/auth/steam/callback" {
		t.Fatalf("expected return_to preserved, got %q", q.Get("openid.return_to"))
	}
	if q.Get("openid.realm") != "https://cache.local" {
		t.Fatalf("expected realm preserved, got %q", q.Get("openid.realm"))
	}
}

// validCallbackParams returns a syntactically well-formed positive
// assertion's params (signed fields, claimed_id, etc. all present and
// consistent with the spec) -- only check_authentication's response (which
// the test server controls) determines whether VerifyCallback accepts it.
const testReturnTo = "https://cache.local/auth/steam/callback"

func validCallbackParams(steamID64 string) url.Values {
	return url.Values{
		"openid.ns":             {openIDNamespace},
		"openid.mode":           {"id_res"},
		"openid.op_endpoint":    {steamOpenIDEndpoint},
		"openid.claimed_id":     {"https://steamcommunity.com/openid/id/" + steamID64},
		"openid.identity":       {"https://steamcommunity.com/openid/id/" + steamID64},
		"openid.return_to":      {testReturnTo},
		"openid.response_nonce": {"2026-06-20T00:00:00Zabcdef"},
		"openid.assoc_handle":   {"handle123"},
		"openid.signed":         {"op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
		"openid.sig":            {"ZmFrZXNpZw=="},
	}
}

// steamStub stands in for steamcommunity.com/openid/login's
// check_authentication endpoint, returning the configured body. The real
// wire format ("ns:...\nis_valid:true\n") was confirmed live against
// steamcommunity.com during this unit's implementation.
func steamStub(t *testing.T, isValid bool) *http.Client {
	t.Helper()
	body := "ns:" + openIDNamespace + "\nis_valid:false\n"
	if isValid {
		body = "ns:" + openIDNamespace + "\nis_valid:true\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	// VerifyCallback always POSTs to the literal steamOpenIDEndpoint
	// constant, so redirect the test client there via a custom Transport
	// rather than parameterizing the endpoint (keeping the production code
	// from needing an endpoint override just for tests).
	return &http.Client{Transport: redirectTransport{target: srv.URL}}
}

type redirectTransport struct{ target string }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestVerifyCallback_HappyPath(t *testing.T) {
	client := steamStub(t, true)
	steamID64, err := VerifyCallback(client, validCallbackParams("76561197960435530"), testReturnTo)
	if err != nil {
		t.Fatalf("VerifyCallback: %v", err)
	}
	if steamID64 != "76561197960435530" {
		t.Fatalf("expected the extracted SteamID64, got %q", steamID64)
	}
}

// TestVerifyCallback_ForgedCallbackRejected covers the plan's edge case: a
// forged/tampered callback (failing check_authentication) is rejected
// rather than establishing a session.
func TestVerifyCallback_ForgedCallbackRejected(t *testing.T) {
	client := steamStub(t, false) // simulates Steam saying is_valid:false
	_, err := VerifyCallback(client, validCallbackParams("76561197960435530"), testReturnTo)
	if err == nil {
		t.Fatalf("expected a forged/tampered callback to be rejected")
	}
}

func TestVerifyCallback_WrongModeRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Set("openid.mode", "cancel")
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatalf("expected a non-id_res mode to be rejected without even contacting steam")
	}
}

func TestVerifyCallback_WrongNamespaceRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Set("openid.ns", "http://specs.openid.net/auth/1.0")
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatalf("expected a non-2.0 namespace to be rejected")
	}
}

func TestVerifyCallback_MissingClaimedIDRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Del("openid.claimed_id")
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatalf("expected a missing claimed_id to be rejected")
	}
}

// TestVerifyCallback_UnsignedClaimedIDRejected is the spec-correctness
// check this hand-rolled implementation exists for (KTD8): per OpenID 2.0
// section 10.1, "signed" MUST cover claimed_id and identity whenever
// present. An attacker who could get a response accepted with claimed_id
// outside the signed set could potentially substitute a different
// identity into an otherwise-validly-signed assertion.
func TestVerifyCallback_UnsignedClaimedIDRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Set("openid.signed", "op_endpoint,return_to,response_nonce,assoc_handle") // claimed_id/identity omitted
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatalf("expected an unsigned claimed_id to be rejected even though it's present in the assertion")
	}
}

func TestVerifyCallback_MalformedClaimedIDRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Set("openid.claimed_id", "https://evil.example.com/not-steam")
	params.Set("openid.identity", "https://evil.example.com/not-steam")
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatalf("expected a claimed_id that isn't a steamcommunity.com identity URL to be rejected")
	}
}

// TestCompleteLogin_NonAllowlistedSteamIDRejected covers the plan's edge
// case: a valid Steam login from a non-allowlisted SteamID64 is rejected
// and no session is established.
func TestCompleteLogin_NonAllowlistedSteamIDRejected(t *testing.T) {
	client := steamStub(t, true)
	sessions := NewManager()

	_, _, err := CompleteLogin(client, validCallbackParams("76561197960435530"), testReturnTo, "76561197999999999", sessions)
	if err == nil {
		t.Fatalf("expected a non-allowlisted SteamID64 to be rejected")
	}

	if len(sessions.sessions) != 0 {
		t.Fatalf("expected no session established for a non-allowlisted SteamID64")
	}
}

func TestCompleteLogin_AllowlistedSteamIDEstablishesSession(t *testing.T) {
	client := steamStub(t, true)
	sessions := NewManager()

	sessionID, csrfToken, err := CompleteLogin(client, validCallbackParams("76561197960435530"), testReturnTo, "76561197960435530", sessions)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if sessionID == "" || csrfToken == "" {
		t.Fatalf("expected a non-empty session ID and CSRF token")
	}

	session, ok := sessions.Get(sessionID)
	if !ok || session.SteamID64 != "76561197960435530" {
		t.Fatalf("expected a session for the allowlisted SteamID64, got %+v ok=%v", session, ok)
	}
}

// A validly signed assertion issued to a different site's return_to must not
// log in here (replay of another relying party's "Sign in with Steam").
func TestVerifyCallback_ForeignReturnToRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Set("openid.return_to", "https://evil.example/auth/steam/callback")
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatal("expected rejection of a foreign return_to")
	}
}

func TestVerifyCallback_UnsignedReturnToRejected(t *testing.T) {
	client := steamStub(t, true)
	params := validCallbackParams("76561197960435530")
	params.Set("openid.signed", "op_endpoint,claimed_id,identity,response_nonce,assoc_handle")
	if _, err := VerifyCallback(client, params, testReturnTo); err == nil {
		t.Fatal("expected rejection when return_to is not signed")
	}
}
