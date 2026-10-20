// Package dnsmasq generates the CDN domain-interception config dnsmasq
// reads (R9, KTD3), and the BYOD export for operators who manage their own
// DNS (KTD6). It writes nothing to disk itself -- WriteFile in reload.go
// does that -- so this package stays a pure, fully testable renderer.
package dnsmasq

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

//go:embed cachedomains/*.txt cachedomains/cache_domains.json
var cacheDomainsFS embed.FS

// hostnameRE matches a syntactically valid DNS hostname (no leading "*.",
// callers strip that first). Mirrors internal/nginx's hostnameRE: conservative
// on purpose, since a malformed entry must not be able to break the
// generated dnsmasq config's grammar -- defense in depth on top of the fact
// that this data is a pinned, vendored snapshot (see cachedomains/SOURCE.md),
// not arbitrary runtime input.
var hostnameRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)+$`)

// DomainKind distinguishes an exact hostname from a wildcard subdomain
// pattern -- the two need different dnsmasq mechanisms (KTD6/KTD7's
// reload-time tradeoff): exact hostnames go through --addn-hosts, which
// dnsmasq hot-reloads on SIGHUP; wildcards need --address=/domain/ip in the
// main config, which dnsmasq only re-reads on a full restart (confirmed
// against dnsmasq's own docs: "SIGHUP does NOT re-read the configuration
// file"). See generate.go for how each kind is rendered.
type DomainKind int

const (
	DomainExact DomainKind = iota
	DomainWildcard
)

// Domain is one CDN hostname entry from the vendored cache-domains lists.
type Domain struct {
	// Base is the hostname with any leading "*." stripped. dnsmasq's
	// --address domain matching already covers subdomains of Base without
	// a wildcard token, so Base is what both the addn-hosts and
	// --address renderers need.
	Base string
	Kind DomainKind
}

// Platform is one CDN/game-platform entry (R8: "view all CDN platforms").
type Platform struct {
	Name        string
	Description string
	Domains     []Domain
}

type cacheDomainsFile struct {
	CacheDomains []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		DomainFiles []string `json:"domain_files"`
	} `json:"cache_domains"`
}

// LoadPlatforms parses the vendored, pinned cache-domains snapshot
// (cachedomains/SOURCE.md) into the platform list. Domains that fail
// hostname validation are dropped rather than included verbatim and
// returned in skipped, alongside their owning platform name, for the
// caller to log -- the primary defense is that this data is pinned and
// already hand-verified at vendoring time (see SOURCE.md), but a generated
// config must never trust vendored data unconditionally either.
func LoadPlatforms() (platforms []Platform, skipped []string, err error) {
	raw, err := cacheDomainsFS.ReadFile("cachedomains/cache_domains.json")
	if err != nil {
		return nil, nil, fmt.Errorf("reading cache_domains.json: %w", err)
	}

	var parsed cacheDomainsFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, fmt.Errorf("parsing cache_domains.json: %w", err)
	}

	for _, p := range parsed.CacheDomains {
		platform := Platform{Name: p.Name, Description: p.Description}

		for _, domainFile := range p.DomainFiles {
			content, err := cacheDomainsFS.ReadFile("cachedomains/" + domainFile)
			if err != nil {
				return nil, nil, fmt.Errorf("reading %s for platform %s: %w", domainFile, p.Name, err)
			}

			for _, line := range strings.Split(string(content), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}

				kind := DomainExact
				base := line
				if strings.HasPrefix(line, "*.") {
					kind = DomainWildcard
					base = strings.TrimPrefix(line, "*.")
				}
				base = strings.ToLower(base)

				if !hostnameRE.MatchString(base) {
					skipped = append(skipped, fmt.Sprintf("%s: %s", p.Name, line))
					continue
				}

				platform.Domains = append(platform.Domains, Domain{Base: base, Kind: kind})
			}
		}

		sort.Slice(platform.Domains, func(i, j int) bool { return platform.Domains[i].Base < platform.Domains[j].Base })
		platforms = append(platforms, platform)
	}

	sort.Slice(platforms, func(i, j int) bool { return platforms[i].Name < platforms[j].Name })
	return platforms, skipped, nil
}
