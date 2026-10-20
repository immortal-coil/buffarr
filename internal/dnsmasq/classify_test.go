package dnsmasq

import "testing"

func TestClassifyHost_ExactMatch(t *testing.T) {
	platforms := testPlatforms()
	name, ok := ClassifyHost("lancache.steamcontent.com", platforms)
	if !ok || name != "steam" {
		t.Fatalf("expected steam, got %q ok=%v", name, ok)
	}
}

func TestClassifyHost_ExactKindDoesNotMatchSubdomain(t *testing.T) {
	platforms := testPlatforms()
	if _, ok := ClassifyHost("foo.lancache.steamcontent.com", platforms); ok {
		t.Fatalf("expected an exact-kind domain to not match a subdomain")
	}
}

func TestClassifyHost_WildcardMatchesSubdomain(t *testing.T) {
	platforms := testPlatforms()
	name, ok := ClassifyHost("eu-1.cdn.ubi.com", platforms)
	if !ok || name != "uplay" {
		t.Fatalf("expected uplay, got %q ok=%v", name, ok)
	}
}

func TestClassifyHost_WildcardMatchesBaseDomainItself(t *testing.T) {
	platforms := testPlatforms()
	name, ok := ClassifyHost("cdn.ubi.com", platforms)
	if !ok || name != "uplay" {
		t.Fatalf("expected uplay, got %q ok=%v", name, ok)
	}
}

func TestClassifyHost_NoMatch(t *testing.T) {
	platforms := testPlatforms()
	if _, ok := ClassifyHost("unrelated.example.com", platforms); ok {
		t.Fatalf("expected no match for an unrelated host")
	}
}

func TestClassifyHost_CaseInsensitive(t *testing.T) {
	platforms := testPlatforms()
	name, ok := ClassifyHost("LanCache.SteamContent.Com", platforms)
	if !ok || name != "steam" {
		t.Fatalf("expected case-insensitive match to steam, got %q ok=%v", name, ok)
	}
}
