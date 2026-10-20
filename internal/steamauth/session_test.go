package steamauth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManager_CreateAndGet(t *testing.T) {
	m := NewManager()
	sessionID, csrfToken, err := m.Create("76561197960435530")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	session, ok := m.Get(sessionID)
	if !ok {
		t.Fatalf("expected the session to be retrievable")
	}
	if session.SteamID64 != "76561197960435530" || session.CSRFToken != csrfToken {
		t.Fatalf("unexpected session: %+v", session)
	}
}

func TestManager_CreateGeneratesDistinctTokens(t *testing.T) {
	m := NewManager()
	id1, csrf1, _ := m.Create("76561197960435530")
	id2, csrf2, _ := m.Create("76561197960435530")
	if id1 == id2 {
		t.Fatalf("expected distinct session IDs across calls")
	}
	if csrf1 == csrf2 {
		t.Fatalf("expected distinct CSRF tokens across calls")
	}
}

func TestManager_Delete(t *testing.T) {
	m := NewManager()
	sessionID, _, _ := m.Create("76561197960435530")
	m.Delete(sessionID)
	if _, ok := m.Get(sessionID); ok {
		t.Fatalf("expected the session to be gone after Delete")
	}
}

// TestSetSessionCookie_AttributesAlwaysSet covers the plan's edge case:
// the session cookie carries HttpOnly always and SameSite=Lax always. Lax
// (not Strict) is required so the cookie survives the cross-site Steam OpenID
// callback redirect to /library; see SetSessionCookie for the full rationale.
func TestSetSessionCookie_AttributesAlwaysSet(t *testing.T) {
	for _, tls := range []bool{false, true} {
		req := httptest.NewRequest("GET", "http://cache.local/", nil)
		if tls {
			req.TLS = &tls13ConnectionState
		}
		rec := httptest.NewRecorder()

		SetSessionCookie(rec, req, "some-session-id")

		resp := rec.Result()
		cookies := resp.Cookies()
		if len(cookies) != 1 {
			t.Fatalf("expected exactly one cookie set, got %d", len(cookies))
		}
		c := cookies[0]
		if !c.HttpOnly {
			t.Fatalf("expected HttpOnly always, tls=%v", tls)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("expected SameSite=Lax always, tls=%v, got %v", tls, c.SameSite)
		}
		if c.MaxAge <= 0 {
			t.Fatalf("expected a positive Max-Age session TTL, tls=%v, got %d", tls, c.MaxAge)
		}
	}
}

// TestRequireSession_ExpiredSessionRejected covers the H2 absolute TTL: a
// session older than sessionTTL is rejected and evicted, so a captured cookie
// is not valid forever.
func TestRequireSession_ExpiredSessionRejected(t *testing.T) {
	m := NewManager()
	sessionID, _, err := m.Create("76561197960435530")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Age the session past the TTL (white-box: same package).
	m.mu.Lock()
	s := m.sessions[sessionID]
	s.CreatedAt = time.Now().Add(-sessionTTL - time.Minute)
	m.sessions[sessionID] = s
	m.mu.Unlock()

	req := httptest.NewRequest("GET", "https://cache.local/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	if _, err := m.RequireSession(req); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession for an expired session, got %v", err)
	}
	if _, ok := m.Get(sessionID); ok {
		t.Fatalf("expected the expired session to be evicted from the manager")
	}
}

// TestSetSessionCookie_SecureOnlyOverTLS covers KTD17: Secure only on the
// HTTPS connection, since a Secure cookie set over plain HTTP would never
// be sent back by the browser on this stack's default plain-HTTP LAN
// deployment.
func TestSetSessionCookie_SecureOnlyOverTLS(t *testing.T) {
	plainReq := httptest.NewRequest("GET", "http://cache.local/", nil)
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, plainReq, "some-session-id")
	if rec.Result().Cookies()[0].Secure {
		t.Fatalf("expected Secure=false over plain HTTP")
	}

	tlsReq := httptest.NewRequest("GET", "https://cache.local/", nil)
	tlsReq.TLS = &tls13ConnectionState
	rec = httptest.NewRecorder()
	SetSessionCookie(rec, tlsReq, "some-session-id")
	if !rec.Result().Cookies()[0].Secure {
		t.Fatalf("expected Secure=true over TLS")
	}
}

func TestRequireSession_NoCookieRejected(t *testing.T) {
	m := NewManager()
	req := httptest.NewRequest("GET", "http://cache.local/", nil)
	if _, err := m.RequireSession(req); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession, got %v", err)
	}
}

func TestRequireSession_InvalidCookieRejected(t *testing.T) {
	m := NewManager()
	req := httptest.NewRequest("GET", "http://cache.local/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "nonexistent-session"})
	if _, err := m.RequireSession(req); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession for an unknown session ID, got %v", err)
	}
}

func TestRequireSession_ValidCookieAccepted(t *testing.T) {
	m := NewManager()
	sessionID, _, _ := m.Create("76561197960435530")

	req := httptest.NewRequest("GET", "http://cache.local/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})

	session, err := m.RequireSession(req)
	if err != nil {
		t.Fatalf("RequireSession: %v", err)
	}
	if session.SteamID64 != "76561197960435530" {
		t.Fatalf("unexpected session: %+v", session)
	}
}

// TestRequireCSRF_MissingOrWrongTokenRejected covers the plan's edge case:
// a state-changing request without a valid CSRF token is rejected.
func TestRequireCSRF_MissingOrWrongTokenRejected(t *testing.T) {
	m := NewManager()
	sessionID, _, _ := m.Create("76561197960435530")

	req := httptest.NewRequest("POST", "http://cache.local/delete", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	// no X-CSRF-Token header at all
	if _, err := m.RequireCSRF(req); err != ErrInvalidCSRF {
		t.Fatalf("expected ErrInvalidCSRF for a missing token, got %v", err)
	}

	req.Header.Set(CSRFHeader, "wrong-token")
	if _, err := m.RequireCSRF(req); err != ErrInvalidCSRF {
		t.Fatalf("expected ErrInvalidCSRF for a wrong token, got %v", err)
	}
}

func TestRequireCSRF_CorrectTokenAccepted(t *testing.T) {
	m := NewManager()
	sessionID, csrfToken, _ := m.Create("76561197960435530")

	req := httptest.NewRequest("POST", "http://cache.local/delete", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	req.Header.Set(CSRFHeader, csrfToken)

	if _, err := m.RequireCSRF(req); err != nil {
		t.Fatalf("RequireCSRF: %v", err)
	}
}

func TestRequireCSRF_NoSessionRejectedBeforeCSRFCheck(t *testing.T) {
	m := NewManager()
	req := httptest.NewRequest("POST", "http://cache.local/delete", nil)
	req.Header.Set(CSRFHeader, "anything")
	if _, err := m.RequireCSRF(req); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession when there's no session at all, got %v", err)
	}
}

// tls13ConnectionState is a non-nil *tls.ConnectionState stand-in: the
// session cookie logic only checks r.TLS != nil, so a zero-value struct is
// sufficient to simulate "this request arrived over TLS" without setting
// up a real TLS handshake in tests.
var tls13ConnectionState tls.ConnectionState
