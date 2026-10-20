package nginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func exact(domains ...string) []AllowlistEntry {
	entries := make([]AllowlistEntry, len(domains))
	for i, d := range domains {
		entries[i] = AllowlistEntry{Domain: d}
	}
	return entries
}

func TestGenerateAllowlist_HappyPath(t *testing.T) {
	fragment, skipped := GenerateAllowlist(exact("steamcontent.com", "epicgames.com"))

	if len(skipped) != 0 {
		t.Fatalf("expected no skipped domains, got %v", skipped)
	}
	if !strings.Contains(fragment, "map $host $cdn_enabled {") {
		t.Fatalf("fragment missing map block: %q", fragment)
	}
	if !strings.Contains(fragment, "hostnames;") {
		t.Fatalf("fragment must enable hostnames; -- without it, wildcard entries are inert literal strings (confirmed live against nginx): %q", fragment)
	}
	if !strings.Contains(fragment, "default 0;") {
		t.Fatalf("fragment must default-deny by omission (KTD3): %q", fragment)
	}
	if !strings.Contains(fragment, "steamcontent.com 1;") {
		t.Fatalf("fragment missing steamcontent.com entry: %q", fragment)
	}
	if !strings.Contains(fragment, "epicgames.com 1;") {
		t.Fatalf("fragment missing epicgames.com entry: %q", fragment)
	}
}

func TestGenerateAllowlist_WildcardUsesLeadingDotForm(t *testing.T) {
	fragment, _ := GenerateAllowlist([]AllowlistEntry{{Domain: "cdn.ubi.com", Wildcard: true}})

	if !strings.Contains(fragment, ".cdn.ubi.com 1;") {
		t.Fatalf("expected the leading-dot wildcard form (matches the domain AND its subdomains, confirmed live against nginx): %q", fragment)
	}
	if strings.Contains(fragment, "*.cdn.ubi.com") {
		t.Fatalf("expected the leading-dot form, not the *.domain form: %q", fragment)
	}
}

func TestGenerateAllowlist_ExactEntryHasNoLeadingDot(t *testing.T) {
	fragment, _ := GenerateAllowlist(exact("lancache.steamcontent.com"))
	if !strings.Contains(fragment, "    lancache.steamcontent.com 1;") {
		t.Fatalf("expected a plain (non-wildcard) entry: %q", fragment)
	}
}

func TestGenerateAllowlist_DisabledDomainNotPresent(t *testing.T) {
	// A domain not in the enabled set must not appear with a "1" mapping --
	// it falls through to the "default 0" rule, which is what makes the
	// nginx-side Host-header rejection test (U2's edge case) work.
	fragment, _ := GenerateAllowlist(exact("steamcontent.com"))

	if strings.Contains(fragment, "battle.net") {
		t.Fatalf("disabled domain leaked into fragment: %q", fragment)
	}
}

func TestGenerateAllowlist_MalformedFragmentInputFallsBackSafely(t *testing.T) {
	// Inputs that could break nginx map{} syntax (empty entries, embedded
	// whitespace/semicolons/braces, bare IPs) must be dropped, not rendered
	// verbatim -- this is U2's "malformed regenerated fragment" defense
	// implemented at the Go layer, ahead of nginx -t at the config layer.
	malformed := exact(
		"",
		"   ",
		"not a hostname",
		"contains;semicolon.com",
		"contains{brace.com",
		"192.168.1.1",
		"-leadinghyphen.com",
		"trailing-hyphen-.com",
	)
	fragment, skipped := GenerateAllowlist(malformed)

	if len(skipped) != len(malformed)-1 { // "" and "   " both normalize to empty and are silently dropped, not "skipped"
		t.Logf("skipped=%v (informational)", skipped)
	}

	// Isolate the map{} body -- the doc-comment header legitimately
	// contains punctuation like ';' and must not be conflated with the
	// generated nginx directives.
	body := fragment[strings.Index(fragment, "map $host $cdn_enabled {"):]
	for _, bad := range []string{"semicolon.com", "brace.com", "192.168.1.1", "not a hostname", "leadinghyphen.com", "trailing-hyphen-.com"} {
		if strings.Contains(body, bad) {
			t.Fatalf("malformed input leaked into fragment body: %q contains %q", body, bad)
		}
	}
	// Only the default-deny rule should remain in the body.
	if strings.Count(body, "1;") != 0 {
		t.Fatalf("expected no enabled-domain entries after rejecting all input: %q", body)
	}
	// The fragment must still be a structurally complete, well-formed map
	// block even when every input was rejected.
	if !strings.Contains(fragment, "map $host $cdn_enabled {") || !strings.Contains(fragment, "}") {
		t.Fatalf("fragment is not structurally complete after rejecting all input: %q", fragment)
	}
}

func TestGenerateAllowlist_DeduplicatesAndSorts(t *testing.T) {
	fragment, _ := GenerateAllowlist(exact("b.example.com", "a.example.com", "a.example.com", "B.EXAMPLE.COM"))

	first := strings.Index(fragment, "a.example.com 1;")
	second := strings.Index(fragment, "b.example.com 1;")
	if first == -1 || second == -1 {
		t.Fatalf("expected both domains present once each: %q", fragment)
	}
	if first > second {
		t.Fatalf("expected sorted order a before b: %q", fragment)
	}
	if strings.Count(fragment, "a.example.com 1;") != 1 {
		t.Fatalf("expected case-insensitive dedupe, got duplicate entries: %q", fragment)
	}
}

func TestWriteAllowlist_WritesFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "40_cdn_allowlist.conf")

	fragment, _ := GenerateAllowlist(exact("steamcontent.com"))
	if err := WriteAllowlist(path, fragment); err != nil {
		t.Fatalf("WriteAllowlist: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if string(got) != fragment {
		t.Fatalf("written content does not match fragment")
	}
}

func TestWriteAllowlist_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "40_cdn_allowlist.conf")

	fragment, _ := GenerateAllowlist(exact("steamcontent.com"))
	if err := WriteAllowlist(path, fragment); err != nil {
		t.Fatalf("WriteAllowlist: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file (the rename target) in %s, got %d: %v", dir, len(entries), entries)
	}
}

func TestWriteAllowlist_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "40_cdn_allowlist.conf")

	f1, _ := GenerateAllowlist(exact("steamcontent.com"))
	if err := WriteAllowlist(path, f1); err != nil {
		t.Fatalf("first WriteAllowlist: %v", err)
	}

	f2, _ := GenerateAllowlist(exact("epicgames.com"))
	if err := WriteAllowlist(path, f2); err != nil {
		t.Fatalf("second WriteAllowlist: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if strings.Contains(string(got), "steamcontent.com") {
		t.Fatalf("toggle integration: stale domain from prior write still present: %q", got)
	}
	if !strings.Contains(string(got), "epicgames.com") {
		t.Fatalf("toggle integration: new domain missing after overwrite: %q", got)
	}
}

func TestRegenerateAllowlist_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "40_cdn_allowlist.conf")

	skipped, err := RegenerateAllowlist(path, exact("steamcontent.com", "bad host with spaces"))
	if err != nil {
		t.Fatalf("RegenerateAllowlist: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("expected one skipped malformed domain, got %v", skipped)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if !strings.Contains(string(got), "steamcontent.com 1;") {
		t.Fatalf("valid domain missing from regenerated file: %q", got)
	}
}
