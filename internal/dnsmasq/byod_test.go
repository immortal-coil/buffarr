package dnsmasq

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateBYODExport_ReflectsEnabledSetAtGenerationTime(t *testing.T) {
	now := time.Now()
	export, err := GenerateBYODExport("10.0.0.5", testPlatforms(), []string{"steam"}, now)
	if err != nil {
		t.Fatalf("GenerateBYODExport: %v", err)
	}

	if !strings.Contains(export.Content, "10.0.0.5 lancache.steamcontent.com") {
		t.Fatalf("expected steam's domain in export: %q", export.Content)
	}
	if strings.Contains(export.Content, "ubi.com") {
		t.Fatalf("expected uplay (not enabled) absent from export: %q", export.Content)
	}
}

func TestGenerateBYODExport_KeepsWildcardAnnotationLiteral(t *testing.T) {
	export, err := GenerateBYODExport("10.0.0.5", testPlatforms(), []string{"uplay"}, time.Now())
	if err != nil {
		t.Fatalf("GenerateBYODExport: %v", err)
	}

	if !strings.Contains(export.Content, "10.0.0.5 *.cdn.ubi.com") {
		t.Fatalf("expected BYOD export to keep the wildcard token literal (unlike the internal addn-hosts/--address split): %q", export.Content)
	}
}

func TestBYODExport_StaleSinceUnchangedConfig(t *testing.T) {
	now := time.Now()
	export, err := GenerateBYODExport("10.0.0.5", testPlatforms(), []string{"steam"}, now)
	if err != nil {
		t.Fatalf("GenerateBYODExport: %v", err)
	}

	current := Snapshot([]string{"steam"}, now.Add(time.Hour))
	stale, _ := export.StaleSince(current)
	if stale {
		t.Fatalf("expected export to not be stale when the enabled set is unchanged")
	}
}

func TestBYODExport_StaleSinceConfigChanged(t *testing.T) {
	exportedAt := time.Now()
	export, err := GenerateBYODExport("10.0.0.5", testPlatforms(), []string{"steam"}, exportedAt)
	if err != nil {
		t.Fatalf("GenerateBYODExport: %v", err)
	}

	changedAt := exportedAt.Add(time.Hour)
	current := Snapshot([]string{"steam", "uplay"}, changedAt)

	stale, since := export.StaleSince(current)
	if !stale {
		t.Fatalf("expected export to be stale after the live enabled set changed")
	}
	if !since.Equal(changedAt) {
		t.Fatalf("expected staleness timestamp to be when the live config changed (%v), got %v", changedAt, since)
	}
}

func TestSnapshot_OrderIndependent(t *testing.T) {
	now := time.Now()
	a := Snapshot([]string{"steam", "uplay"}, now)
	b := Snapshot([]string{"uplay", "steam"}, now)
	if a.Version != b.Version {
		t.Fatalf("expected snapshot version to be order-independent, got %q vs %q", a.Version, b.Version)
	}
}

func TestGenerateBYODExport_RejectsInvalidCacheServerIP(t *testing.T) {
	if _, err := GenerateBYODExport("not-an-ip", testPlatforms(), []string{"steam"}, time.Now()); err == nil {
		t.Fatalf("expected an error for a non-IP cacheServerIP")
	}
}
