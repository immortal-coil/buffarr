# Source

Vendored from https://github.com/uklans/cache-domains at commit
`170c0905e4d5230833a1854b896299c743c3059e` (master, 2025-08-16).

This is the community-maintained CDN domain list the common LAN game caches
draw from (the plan's KTD reference to a "cdn-domains" list is an informal name
for this same project). Pinned per KTD3/U3: treated as untrusted input, validated against a
syntactic allowlist before being written into any generated config (see
internal/dnsmasq/platforms.go).

To update: re-fetch `cache_domains.json` and every file listed in its
`domain_files` arrays at a new commit, update the commit SHA in this file,
and re-run `go test ./internal/dnsmasq/...`.
