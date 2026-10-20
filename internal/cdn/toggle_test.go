package cdn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/dnsmasq"
)

func testPlatforms() []dnsmasq.Platform {
	return []dnsmasq.Platform{
		{
			Name:        "steam",
			Description: "CDN for Steam",
			Domains: []dnsmasq.Domain{
				{Base: "lancache.steamcontent.com", Kind: dnsmasq.DomainExact},
			},
		},
		{
			Name:        "uplay",
			Description: "CDN for Ubisoft",
			Domains: []dnsmasq.Domain{
				{Base: "cdn.ubi.com", Kind: dnsmasq.DomainWildcard},
			},
		},
	}
}

type testPaths struct {
	addnHosts    string
	wildcardConf string
	allowlist    string
}

func newTestToggler(t *testing.T) (*Toggler, *catalog.Catalog, testPaths) {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	dir := t.TempDir()
	paths := testPaths{
		addnHosts:    filepath.Join(dir, "cdn-hosts"),
		wildcardConf: filepath.Join(dir, "cdn-wildcards.conf"),
		allowlist:    filepath.Join(dir, "40_cdn_allowlist.conf"),
	}

	platforms := testPlatforms()
	if err := cat.SeedPlatformsIfEmpty(platformNames(platforms), []string{"steam"}); err != nil {
		t.Fatalf("SeedPlatformsIfEmpty: %v", err)
	}

	toggler := NewToggler(cat, platforms, "10.0.0.5", paths.addnHosts, paths.wildcardConf, paths.allowlist)

	// Establish the initial on-disk state, matching what main.go's
	// startup-assert does for a freshly-seeded catalog.
	if err := toggler.RegenerateFromPersisted(); err != nil {
		t.Fatalf("RegenerateFromPersisted (initial): %v", err)
	}

	return toggler, cat, paths
}

func platformNames(platforms []dnsmasq.Platform) []string {
	names := make([]string, len(platforms))
	for i, p := range platforms {
		names[i] = p.Name
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestListPlatforms_DefaultsSteamEnabled(t *testing.T) {
	toggler, _, _ := newTestToggler(t)

	statuses, err := toggler.ListPlatforms()
	if err != nil {
		t.Fatalf("ListPlatforms: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 platforms, got %v", statuses)
	}
	for _, s := range statuses {
		wantEnabled := s.Name == "steam"
		if s.Enabled != wantEnabled {
			t.Fatalf("expected steam=enabled, others=disabled by default; got %+v", s)
		}
	}
}

// TestToggle_HappyPath_UpdatesBothLayers covers the plan's happy path:
// disabling a CDN updates both dnsmasq's addn-hosts and nginx's allow-list
// in the same operation.
func TestToggle_HappyPath_UpdatesBothLayers(t *testing.T) {
	toggler, _, paths := newTestToggler(t)

	if err := toggler.Toggle("steam", false); err != nil {
		t.Fatalf("Toggle: %v", err)
	}

	addnHosts := readFile(t, paths.addnHosts)
	if strings.Contains(addnHosts, "lancache.steamcontent.com") {
		t.Fatalf("expected steam's domain removed from addn-hosts after disabling: %q", addnHosts)
	}

	allowlist := readFile(t, paths.allowlist)
	if strings.Contains(allowlist, "lancache.steamcontent.com 1;") {
		t.Fatalf("expected steam's domain removed from the nginx allow-list after disabling: %q", allowlist)
	}

	statuses, err := toggler.ListPlatforms()
	if err != nil {
		t.Fatalf("ListPlatforms: %v", err)
	}
	for _, s := range statuses {
		if s.Name == "steam" && s.Enabled {
			t.Fatalf("expected steam to report disabled after Toggle")
		}
	}
}

func TestToggle_WildcardPlatformAppearsInBothFilesCorrectly(t *testing.T) {
	toggler, _, paths := newTestToggler(t)

	// uplay starts disabled by default; enable it so its wildcard domain
	// renders in both dnsmasq's wildcard-conf (restart path) and nginx's
	// allow-list (leading-dot form), not the SIGHUP-only addn-hosts file.
	if err := toggler.Toggle("uplay", true); err != nil {
		t.Fatalf("Toggle uplay: %v", err)
	}

	addnHosts := readFile(t, paths.addnHosts)
	if strings.Contains(addnHosts, "cdn.ubi.com") {
		t.Fatalf("wildcard domain must not appear in addn-hosts: %q", addnHosts)
	}

	wildcardConf := readFile(t, paths.wildcardConf)
	if !strings.Contains(wildcardConf, "address=/cdn.ubi.com/10.0.0.5") {
		t.Fatalf("expected uplay's wildcard domain in the dnsmasq wildcard conf: %q", wildcardConf)
	}

	allowlist := readFile(t, paths.allowlist)
	if !strings.Contains(allowlist, ".cdn.ubi.com 1;") {
		t.Fatalf("expected uplay's domain in the nginx allow-list using the leading-dot wildcard form: %q", allowlist)
	}
}

func TestToggle_UnknownPlatformIsAnError(t *testing.T) {
	toggler, _, _ := newTestToggler(t)
	if err := toggler.Toggle("not-a-real-platform", false); err == nil {
		t.Fatalf("expected an error for an unknown platform")
	}
}

func TestToggle_ReEnablingRestoresDomain(t *testing.T) {
	toggler, _, paths := newTestToggler(t)

	if err := toggler.Toggle("steam", false); err != nil {
		t.Fatalf("Toggle(false): %v", err)
	}
	if err := toggler.Toggle("steam", true); err != nil {
		t.Fatalf("Toggle(true): %v", err)
	}

	addnHosts := readFile(t, paths.addnHosts)
	if !strings.Contains(addnHosts, "lancache.steamcontent.com") {
		t.Fatalf("expected steam's domain restored after re-enabling: %q", addnHosts)
	}
}

func TestToggle_PartialFailureRevertsAllLayers(t *testing.T) {
	toggler, cat, paths := newTestToggler(t)

	// Capture prior content of the two files that should survive a revert.
	priorAddnHosts := readFile(t, paths.addnHosts)
	priorWildcardConf := readFile(t, paths.wildcardConf)

	// Make the nginx allow-list's directory unwritable to force a failure
	// on the last of the three writes, after the two dnsmasq files have
	// already succeeded -- this is the scenario that actually exercises
	// revert. The atomic writer creates a new temp file in the directory
	// before renaming, so read-only permission on the directory is enough
	// to fail it without needing to touch the existing file.
	if err := os.Chmod(filepath.Dir(paths.allowlist), 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Dir(paths.allowlist), 0o755) })

	err := toggler.Toggle("steam", false)
	os.Chmod(filepath.Dir(paths.allowlist), 0o755) // restore before reading files back
	if err == nil {
		t.Fatalf("expected Toggle to fail when the allow-list path can't be written")
	}

	if got := readFile(t, paths.addnHosts); got != priorAddnHosts {
		t.Fatalf("expected addn-hosts reverted to prior content, got %q want %q", got, priorAddnHosts)
	}
	if got := readFile(t, paths.wildcardConf); got != priorWildcardConf {
		t.Fatalf("expected wildcard-conf reverted to prior content, got %q want %q", got, priorWildcardConf)
	}

	enabled, found, err := cat.PlatformEnabled("steam")
	if err != nil || !found {
		t.Fatalf("PlatformEnabled: found=%v err=%v", found, err)
	}
	if !enabled {
		t.Fatalf("expected the catalog to remain untouched (still enabled) after a partial failure")
	}
}

func TestToggle_RetryAfterPartialFailureSucceeds(t *testing.T) {
	toggler, _, paths := newTestToggler(t)

	if err := os.Chmod(filepath.Dir(paths.allowlist), 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := toggler.Toggle("steam", false); err == nil {
		os.Chmod(filepath.Dir(paths.allowlist), 0o755)
		t.Fatalf("expected the first attempt to fail")
	}
	os.Chmod(filepath.Dir(paths.allowlist), 0o755)

	if err := toggler.Toggle("steam", false); err != nil {
		t.Fatalf("expected the retry to succeed, got: %v", err)
	}

	allowlist := readFile(t, paths.allowlist)
	if strings.Contains(allowlist, "lancache.steamcontent.com 1;") {
		t.Fatalf("expected steam removed from the allow-list after the successful retry: %q", allowlist)
	}
}

func TestRegenerateFromPersisted_ReassertsToggleState(t *testing.T) {
	toggler, _, paths := newTestToggler(t)

	if err := toggler.Toggle("steam", false); err != nil {
		t.Fatalf("Toggle: %v", err)
	}

	// Simulate the files getting reset to some other state (e.g. the
	// containers' own bootstrap defaults after an image update) and the
	// backend restarting -- RegenerateFromPersisted must re-assert the
	// persisted toggle, not the bootstrap default.
	if err := os.WriteFile(paths.addnHosts, []byte("10.0.0.5 lancache.steamcontent.com\n"), 0o644); err != nil {
		t.Fatalf("simulating reset: %v", err)
	}

	if err := toggler.RegenerateFromPersisted(); err != nil {
		t.Fatalf("RegenerateFromPersisted: %v", err)
	}

	addnHosts := readFile(t, paths.addnHosts)
	if strings.Contains(addnHosts, "lancache.steamcontent.com") {
		t.Fatalf("expected RegenerateFromPersisted to re-apply the persisted disable, got %q", addnHosts)
	}
}
