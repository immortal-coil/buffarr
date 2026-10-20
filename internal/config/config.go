// Package config loads runtime configuration for the backend from environment
// variables. All settings have a single source of truth here so other
// packages never read os.Getenv directly.
package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// CacheRoot is the operator-configurable cache storage root folder (R10).
	// Substituted into nginx's proxy_cache_path at container start.
	CacheRoot string

	// CacheMaxBytes is the parsed value of CACHE_MAX_SIZE (e.g. "500g").
	// Zero means unconfigured / unlimited.
	CacheMaxBytes int64

	// HTTPAddr and HTTPSAddr are the webui listen addresses (KTD17).
	// Default to listening on all interfaces (explicit "0.0.0.0:port"
	// rather than the bare ":port" Go shorthand that means the same thing
	// but isn't obviously "all interfaces" on inspection) -- this is a LAN
	// tool game clients and the operator's browser both need to reach from
	// other devices, not a localhost-only service.
	HTTPAddr  string
	HTTPSAddr string

	// TLSCertPath and TLSKeyPath point at the cert/key the backend serves
	// HTTPS with. If either is empty, a self-signed pair is generated on
	// first boot at these paths (KTD17).
	TLSCertPath string
	TLSKeyPath  string

	// SQLitePath is the catalog database file (KTD5).
	SQLitePath string

	// PosterDir is where the backend caches game cover art fetched from the
	// Steam CDN (one .jpg per appID). It lives on the backend-data volume
	// alongside the catalog DB (app metadata), NOT in CacheRoot (the cache
	// content store the disk meter accounts) -- posters are tiny and must not
	// be swept by cache cleanup or skew the used-space readout.
	PosterDir string

	// SteamWebAPIKey authenticates calls to Steam's public Web API
	// (GetOwnedGames, depot->app resolution).
	SteamWebAPIKey string

	// SteamOperatorID64 is the single allowlisted operator SteamID64
	// (KTD13). Login from any other SteamID64 is rejected.
	SteamOperatorID64 string

	// NginxAccessLogPath is the access log U4's tailer follows.
	NginxAccessLogPath string

	// DNSMasqAddnHostsPath and DNSMasqWildcardConfPath are the shared-volume
	// paths the backend writes; nginx/dnsmasq self-watch the files they own
	// (KTD7). The two dnsmasq paths get different reload treatment in
	// dnsmasq's entrypoint -- AddnHosts is SIGHUP-reloadable, WildcardConf
	// requires a full restart (see internal/dnsmasq.DomainKind's doc
	// comment) -- so both paths must match the filenames
	// docker/dnsmasq/entrypoint.sh's watcher matches on exactly.
	DNSMasqAddnHostsPath    string
	DNSMasqWildcardConfPath string
	NginxAllowlistPath      string

	// CacheIP is the LAN IP address game clients should resolve
	// CDN hostnames to (R9/KTD3) -- i.e. this cache server's own address on
	// the operator's network. There is no safe default: a container can
	// only see its own Docker-internal IP, which is meaningless to LAN
	// clients, so this must be operator-supplied.
	CacheIP string

	// APIKey enables the JSON API under /api/v1/ when non-empty
	// (BUFFARR_API_KEY). Requests present it in an X-Api-Key or
	// Authorization: Bearer header. Empty disables the API entirely.
	APIKey string

	// TrustProxyHeaders, when true, makes the webui honor X-Forwarded-Proto
	// and X-Forwarded-Host when building the Steam OpenID return_to/realm, so
	// a login behind a TLS-terminating reverse proxy redirects back to the
	// public scheme/host rather than the backend-local one. Default false:
	// these headers are client-controlled, so trusting them without a proxy
	// in front would let a caller steer the return_to. Enable only when the
	// backend truly sits behind a trusted proxy (TRUST_PROXY_HEADERS=true).
	TrustProxyHeaders bool

	// PrefillTriggerPath, PrefillStatusPath, and PrefillSelectionPath are
	// the shared-volume handshake files between the backend and the
	// prefill sidecar (U8). PrefillTriggerPath's content is just a raw
	// request-id token (not JSON) -- the sidecar's entrypoint reads it
	// with `cat`, no JSON parser needed in the shell script.
	// PrefillSelectionPath is, from the sidecar container's side, the
	// exact path steam-lancache-prefill itself hardcodes
	// (/Config/selectedAppsToPrefill.json, confirmed against the pinned
	// v2.2.2 source) -- the backend writes the real app-ID list there
	// directly (KTD9), in that tool's own bare-JSON-array format.
	PrefillTriggerPath   string
	PrefillStatusPath    string
	PrefillSelectionPath string

	// PrefillLastRunLogPath is the sidecar's --no-ansi run log on the same
	// shared volume (sidecar writes it at /Config/last-run.log; the backend
	// reads it at /shared/prefill/last-run.log). Read-only and best-effort:
	// it drives the Preload page's real download-percent bar, falling back to
	// the indeterminate bar when it can't be parsed.
	PrefillLastRunLogPath string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ParseCacheSize parses nginx-style sizes like "500g", "2t", "1024m".
// Exported so other packages (e.g. the webui's DB-backed min-game-size
// setting) can parse the same operator-facing size syntax. Returns 0 for
// empty or unparseable input.
func ParseCacheSize(s string) int64 {
	if s == "" {
		return 0
	}
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult = 1024
		s = s[:len(s)-1]
	case 'm':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'g':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case 't':
		mult = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	// Negative or overflowing sizes are as unusable as unparseable ones.
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0
	}
	return n * mult
}

// Load reads configuration from the environment, applying defaults that
// match docker-compose.yml's service wiring.
func Load() (*Config, error) {
	cfg := &Config{
		CacheRoot:               envOr("CACHE_ROOT", "/data/cache"),
		CacheMaxBytes:           ParseCacheSize(os.Getenv("CACHE_MAX_SIZE")),
		HTTPAddr:                envOr("HTTP_ADDR", "0.0.0.0:9080"),
		HTTPSAddr:               envOr("HTTPS_ADDR", "0.0.0.0:9443"),
		TLSCertPath:             envOr("TLS_CERT_PATH", "/data/tls/cert.pem"),
		TLSKeyPath:              envOr("TLS_KEY_PATH", "/data/tls/key.pem"),
		SQLitePath:              envOr("SQLITE_PATH", "/data/catalog.db"),
		PosterDir:               envOr("POSTER_DIR", "/data/posters"),
		SteamWebAPIKey:          os.Getenv("STEAM_WEB_API_KEY"),
		SteamOperatorID64:       os.Getenv("STEAM_OPERATOR_ID64"),
		NginxAccessLogPath:      envOr("NGINX_ACCESS_LOG_PATH", "/var/log/nginx/cache-access.log"),
		DNSMasqAddnHostsPath:    envOr("DNSMASQ_ADDN_HOSTS_PATH", "/shared/dnsmasq/cdn-hosts"),
		DNSMasqWildcardConfPath: envOr("DNSMASQ_WILDCARD_CONF_PATH", "/shared/dnsmasq/cdn-wildcards.conf"),
		NginxAllowlistPath:      envOr("NGINX_ALLOWLIST_PATH", "/shared/nginx/40_cdn_allowlist.conf"),
		PrefillTriggerPath:      envOr("PREFILL_TRIGGER_PATH", "/shared/prefill/trigger"),
		PrefillStatusPath:       envOr("PREFILL_STATUS_PATH", "/shared/prefill/status.json"),
		PrefillSelectionPath:    envOr("PREFILL_SELECTION_PATH", "/shared/prefill/selectedAppsToPrefill.json"),
		PrefillLastRunLogPath:   envOr("PREFILL_LAST_RUN_LOG_PATH", "/shared/prefill/last-run.log"),
		CacheIP:                 os.Getenv("BUFFARR_IP"),
		APIKey:                  os.Getenv("BUFFARR_API_KEY"),
		TrustProxyHeaders:       os.Getenv("TRUST_PROXY_HEADERS") == "true",
	}

	if cfg.CacheIP == "" {
		return nil, fmt.Errorf("BUFFARR_IP must be set (the LAN IP clients should resolve CDN hostnames to)")
	}

	return cfg, nil
}
