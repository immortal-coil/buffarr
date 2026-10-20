package dnsmasq

import "testing"

func TestLoadPlatforms_KnownPlatformsPresent(t *testing.T) {
	platforms, skipped, err := LoadPlatforms()
	if err != nil {
		t.Fatalf("LoadPlatforms: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skipped domains from the vendored snapshot, got %v", skipped)
	}

	byName := make(map[string]Platform, len(platforms))
	for _, p := range platforms {
		byName[p.Name] = p
	}

	for _, want := range []string{"steam", "blizzard", "epicgames", "uplay", "wsus"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("expected platform %q to be present, got %v", want, names(platforms))
		}
	}
}

func TestLoadPlatforms_ClassifiesWildcardsVersusExact(t *testing.T) {
	platforms, _, err := LoadPlatforms()
	if err != nil {
		t.Fatalf("LoadPlatforms: %v", err)
	}

	var uplay Platform
	for _, p := range platforms {
		if p.Name == "uplay" {
			uplay = p
		}
	}
	if len(uplay.Domains) == 0 {
		t.Fatalf("expected uplay to have at least one domain")
	}
	for _, d := range uplay.Domains {
		if d.Base != "cdn.ubi.com" {
			t.Fatalf("expected uplay's domain base to be cdn.ubi.com (leading *. stripped), got %q", d.Base)
		}
		if d.Kind != DomainWildcard {
			t.Fatalf("expected uplay's *.cdn.ubi.com entry to classify as DomainWildcard")
		}
	}

	var steam Platform
	for _, p := range platforms {
		if p.Name == "steam" {
			steam = p
		}
	}
	if len(steam.Domains) == 0 {
		t.Fatalf("expected steam to have at least one domain")
	}
	hasWildcard := false
	for _, d := range steam.Domains {
		if d.Kind == DomainWildcard {
			hasWildcard = true
		}
	}
	if !hasWildcard {
		t.Fatalf("expected steam to have at least one wildcard domain (e.g. *.steamcontent.com)")
	}
}

func TestLoadPlatforms_SortedDeterministicOutput(t *testing.T) {
	p1, _, err := LoadPlatforms()
	if err != nil {
		t.Fatalf("LoadPlatforms: %v", err)
	}
	p2, _, err := LoadPlatforms()
	if err != nil {
		t.Fatalf("LoadPlatforms: %v", err)
	}

	if len(p1) != len(p2) {
		t.Fatalf("expected repeated calls to return the same platform count")
	}
	for i := range p1 {
		if p1[i].Name != p2[i].Name {
			t.Fatalf("expected deterministic platform order, got %q vs %q at index %d", p1[i].Name, p2[i].Name, i)
		}
	}

	for i, p := range p1 {
		if i > 0 && p1[i-1].Name >= p.Name {
			t.Fatalf("expected platforms sorted by name, got %q before %q", p1[i-1].Name, p.Name)
		}
	}
}

func names(platforms []Platform) []string {
	out := make([]string, len(platforms))
	for i, p := range platforms {
		out[i] = p.Name
	}
	return out
}
