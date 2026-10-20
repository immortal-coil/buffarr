package dnsmasq

import "strings"

// ClassifyHost reports which platform owns host, reusing the same vendored
// domain data dnsmasq's own config renders from (KTD2/KTD6's single source
// of truth, extended to U4's log classification so there isn't a second,
// independently-maintained copy of "which host belongs to which CDN").
// Matching mirrors dnsmasq's own --address semantics (confirmed in
// generate.go): an exact-kind domain matches only the literal hostname; a
// wildcard-kind domain also matches any subdomain of its base.
func ClassifyHost(host string, platforms []Platform) (platformName string, ok bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, p := range platforms {
		for _, d := range p.Domains {
			if host == d.Base {
				return p.Name, true
			}
			if d.Kind == DomainWildcard && strings.HasSuffix(host, "."+d.Base) {
				return p.Name, true
			}
		}
	}
	return "", false
}
