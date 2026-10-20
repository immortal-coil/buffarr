package dnsmasq

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// ConfigSnapshot identifies an enabled-platform set at a point in time.
// Version is a deterministic digest of the set (order-independent), so two
// snapshots of the same enabled platforms always compare equal regardless
// of how the set was assembled.
type ConfigSnapshot struct {
	Version string
	AsOf    time.Time
}

// Snapshot builds a ConfigSnapshot for the given enabled-platform set.
// Callers (U6's toggle handler) call this once per toggle and keep the
// latest result as "the live snapshot" to compare exports against.
func Snapshot(enabledNames []string, asOf time.Time) ConfigSnapshot {
	normalized := make([]string, len(enabledNames))
	for i, n := range enabledNames {
		normalized[i] = strings.ToLower(strings.TrimSpace(n))
	}
	sort.Strings(normalized)

	h := sha256.Sum256([]byte(strings.Join(normalized, ",")))
	return ConfigSnapshot{Version: hex.EncodeToString(h[:]), AsOf: asOf}
}

// BYODExport is the operator-downloadable domain->IP mapping for an
// operator running their own DNS (e.g. Pi-hole) instead of the vendored
// dnsmasq (KTD6's "Bring Your Own DNS" escape hatch).
type BYODExport struct {
	Content     string
	GeneratedAt time.Time
	Snapshot    ConfigSnapshot
}

// GenerateBYODExport renders the same enabled-platform set Generate uses
// (KTD6: one in-memory model, two output shapes) into a plain-text export.
// Unlike Generate, wildcard domains are NOT split into a separate
// restart-only file or stripped to their base domain -- a BYOD operator
// manages their own DNS server's reload semantics, so this export keeps
// each entry's original wildcard-or-not shape ("*.domain" vs "domain") so
// they have the information to configure wildcard matching on their end if
// their DNS server supports it.
func GenerateBYODExport(cacheServerIP string, platforms []Platform, enabledNames []string, generatedAt time.Time) (BYODExport, error) {
	if net.ParseIP(cacheServerIP) == nil {
		return BYODExport{}, fmt.Errorf("cacheServerIP %q is not a valid IP address", cacheServerIP)
	}

	exact, wildcard := EnabledDomains(platforms, enabledNames)

	var b strings.Builder
	b.WriteString(rendererHeader)
	b.WriteString("# BYOD export: import into your own DNS server. Wildcard entries\n")
	b.WriteString("# (\"*.domain\") need a DNS server that supports wildcard subdomain\n")
	b.WriteString("# matching (e.g. dnsmasq's --address=/domain/ip, which already covers\n")
	b.WriteString("# subdomains without a literal wildcard token); a plain hosts-file\n")
	b.WriteString("# import only intercepts the exact name as written.\n")
	for _, d := range exact {
		fmt.Fprintf(&b, "%s %s\n", cacheServerIP, d)
	}
	for _, d := range wildcard {
		fmt.Fprintf(&b, "%s *.%s\n", cacheServerIP, d)
	}

	return BYODExport{
		Content:     b.String(),
		GeneratedAt: generatedAt,
		Snapshot:    Snapshot(enabledNames, generatedAt),
	}, nil
}

// StaleSince reports whether this export's enabled-platform set no longer
// matches current, and if so, since when (current.AsOf -- the moment the
// live config diverged from what was exported).
func (e BYODExport) StaleSince(current ConfigSnapshot) (stale bool, since time.Time) {
	if e.Snapshot.Version == current.Version {
		return false, time.Time{}
	}
	return true, current.AsOf
}
