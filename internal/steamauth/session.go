package steamauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SessionCookieName is the cookie the operator's browser carries the
// session ID in.
const SessionCookieName = "buffarr_session"

// CSRFHeader is the header U9's htmx/forms requests carry the per-session
// CSRF token in for state-changing requests (KTD15).
const CSRFHeader = "X-CSRF-Token"

// sessionTTL is the absolute lifetime of a session (H2). A session older than
// this is rejected server-side and the cookie is given a matching Max-Age, so a
// captured cookie is not valid indefinitely.
const sessionTTL = 24 * time.Hour

// Session is the single operator's authenticated state. There is
// deliberately no multi-account storage (KTD13's single allowlisted
// SteamID64) -- Manager just keys these by an opaque session ID so the
// cookie value itself never reveals the operator's SteamID64.
type Session struct {
	SteamID64 string
	CSRFToken string
	CreatedAt time.Time
}

// Manager holds active sessions in memory. Sessions do not survive a
// backend restart -- restarting simply requires the operator to log in
// with Steam again, which is an acceptable, common pattern for a
// single-operator tool and avoids persisting session/CSRF secrets to disk.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

func NewManager() *Manager {
	return &Manager{sessions: make(map[string]Session)}
}

// Create establishes a new session for steamID64 and returns its opaque
// session ID and CSRF token, both cryptographically random.
func (m *Manager) Create(steamID64 string) (sessionID, csrfToken string, err error) {
	sessionID, err = randomToken()
	if err != nil {
		return "", "", fmt.Errorf("generating session ID: %w", err)
	}
	csrfToken, err = randomToken()
	if err != nil {
		return "", "", fmt.Errorf("generating CSRF token: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionID] = Session{SteamID64: steamID64, CSRFToken: csrfToken, CreatedAt: time.Now()}
	return sessionID, csrfToken, nil
}

// Get returns the session for sessionID, if any.
func (m *Manager) Get(sessionID string) (Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	return s, ok
}

// Delete ends a session (logout).
func (m *Manager) Delete(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetSessionCookie writes sessionID to the response (KTD15/KTD17):
// HttpOnly always; Secure only when the request actually arrived over TLS,
// since a Secure cookie set over plain HTTP would never be sent back by the
// browser on the plain-HTTP LAN deployment this stack targets by default,
// silently breaking login.
//
// SameSite is Lax, NOT Strict. The session is established by the Steam OpenID
// callback (KTD8), which is a cross-site top-level redirect from
// steamcommunity.com back to /auth/steam/callback. With Strict, the browser
// withholds the freshly-set cookie on the subsequent 303 -> /library because
// that navigation was cross-site-initiated, bouncing the operator straight
// back to /login even though login succeeded (manual navigation then works,
// since it's same-site-initiated). Lax sends the cookie on top-level GET
// navigations, which is exactly the redirect we need, while still withholding
// it on cross-site POSTs. CSRF protection for state-changing requests is
// carried by the per-session CSRF token (KTD15), not by SameSite.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie removes the session cookie (logout).
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

// ErrNoSession and ErrInvalidCSRF distinguish why a request was rejected,
// so callers can respond appropriately (e.g. redirect to login vs. a
// plain 403).
var (
	ErrNoSession   = errors.New("no valid session")
	ErrInvalidCSRF = errors.New("missing or invalid csrf token")
)

// RequireSession returns the request's session, gating access (KTD16:
// every webui route -- library, monitor, delete, CDN control, owned-games,
// prefill -- requires the same authenticated, allowlisted session, not
// just the owned-games/prefill views R11 alone would imply).
func (m *Manager) RequireSession(r *http.Request) (Session, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return Session{}, ErrNoSession
	}
	session, ok := m.Get(cookie.Value)
	if !ok {
		return Session{}, ErrNoSession
	}
	// Absolute expiry (H2): a captured cookie must not be valid forever. Evict
	// the stale session on the way out so the map doesn't accumulate them.
	if time.Since(session.CreatedAt) > sessionTTL {
		m.Delete(cookie.Value)
		return Session{}, ErrNoSession
	}
	return session, nil
}

// RequireCSRF additionally validates the request's CSRF token against its
// session (KTD15) -- for state-changing requests (delete/monitor/toggle/
// prefill), not plain page loads.
func (m *Manager) RequireCSRF(r *http.Request) (Session, error) {
	session, err := m.RequireSession(r)
	if err != nil {
		return Session{}, err
	}
	token := r.Header.Get(CSRFHeader)
	// Constant-time compare so a timing side channel can't be used to recover
	// the token byte by byte. The empty-token check is a cheap presence guard;
	// it leaks nothing about the secret.
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(session.CSRFToken)) != 1 {
		return Session{}, ErrInvalidCSRF
	}
	return session, nil
}
