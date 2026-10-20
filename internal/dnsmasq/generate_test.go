package dnsmasq

import (
	"strings"
	"testing"
)

func testPlatforms() []Platform {
	return []Platform{
		{
			Name: "steam",
			Domains: []Domain{
				{Base: "lancache.steamcontent.com", Kind: DomainExact},
			},
		},
		{
			Name: "uplay",
			Domains: []Domain{
				{Base: "cdn.ubi.com", Kind: DomainWildcard},
			},
		},
		{
			Name: "blizzard",
			Domains: []Domain{
				{Base: "dist.blizzard.com", Kind: DomainExact},
				{Base: "cdn.blizzard.com", Kind: DomainWildcard},
			},
		},
	}
}

func TestGenerate_HappyPath(t *testing.T) {
	addnHosts, wildcardConf, err := Generate("10.0.0.5", testPlatforms(), []string{"steam", "uplay"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(addnHosts, "10.0.0.5 lancache.steamcontent.com") {
		t.Fatalf("expected steam's exact domain in addn-hosts: %q", addnHosts)
	}
	if strings.Contains(addnHosts, "cdn.ubi.com") {
		t.Fatalf("wildcard domain must not appear in addn-hosts (restart-only path): %q", addnHosts)
	}
	if !strings.Contains(wildcardConf, "address=/cdn.ubi.com/10.0.0.5") {
		t.Fatalf("expected uplay's wildcard domain as an --address directive: %q", wildcardConf)
	}
}

func TestGenerate_DisabledPlatformExcluded(t *testing.T) {
	addnHosts, wildcardConf, err := Generate("10.0.0.5", testPlatforms(), []string{"steam"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if strings.Contains(addnHosts, "blizzard") || strings.Contains(addnHosts, "dist.blizzard.com") {
		t.Fatalf("disabled platform's domain leaked into addn-hosts: %q", addnHosts)
	}
	if strings.Contains(wildcardConf, "blizzard.com") {
		t.Fatalf("disabled platform's domain leaked into wildcard conf: %q", wildcardConf)
	}
}

func TestGenerate_SplitsExactAndWildcardWithinOnePlatform(t *testing.T) {
	addnHosts, wildcardConf, err := Generate("10.0.0.5", testPlatforms(), []string{"blizzard"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(addnHosts, "10.0.0.5 dist.blizzard.com") {
		t.Fatalf("expected blizzard's exact domain in addn-hosts: %q", addnHosts)
	}
	if strings.Contains(addnHosts, "cdn.blizzard.com") {
		t.Fatalf("blizzard's wildcard domain must not appear in addn-hosts: %q", addnHosts)
	}
	if !strings.Contains(wildcardConf, "address=/cdn.blizzard.com/10.0.0.5") {
		t.Fatalf("expected blizzard's wildcard domain as an --address directive: %q", wildcardConf)
	}
}

func TestGenerate_RejectsInvalidCacheServerIP(t *testing.T) {
	if _, _, err := Generate("not-an-ip", testPlatforms(), []string{"steam"}); err == nil {
		t.Fatalf("expected an error for a non-IP cacheServerIP")
	}
	if _, _, err := Generate("", testPlatforms(), []string{"steam"}); err == nil {
		t.Fatalf("expected an error for an empty cacheServerIP")
	}
}

func TestGenerate_DeduplicatesAcrossPlatforms(t *testing.T) {
	platforms := []Platform{
		{Name: "a", Domains: []Domain{{Base: "shared.example.com", Kind: DomainExact}}},
		{Name: "b", Domains: []Domain{{Base: "shared.example.com", Kind: DomainExact}}},
	}
	addnHosts, _, err := Generate("10.0.0.5", platforms, []string{"a", "b"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Count(addnHosts, "shared.example.com") != 1 {
		t.Fatalf("expected deduped single entry, got: %q", addnHosts)
	}
}
