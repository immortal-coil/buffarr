package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/immortal-coil/buffarr/internal/dnsmasq"
)

func testIngestPlatforms() []dnsmasq.Platform {
	return []dnsmasq.Platform{
		{
			Name: "steam",
			Domains: []dnsmasq.Domain{
				{Base: "lancache.steamcontent.com", Kind: dnsmasq.DomainExact},
			},
		},
		{
			Name: "blizzard",
			Domains: []dnsmasq.Domain{
				{Base: "dist.blizzard.com", Kind: dnsmasq.DomainExact},
			},
		},
	}
}

type fakeSteamLookup struct {
	depotToApp map[int]int
	appNames   map[int]string
}

func (f *fakeSteamLookup) Resolve(depotID int) (int, string, bool) {
	appID, ok := f.depotToApp[depotID]
	if !ok {
		return 0, "", false
	}
	return appID, f.appNames[appID], true
}

func writeLogFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access.log")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// --- accrueBandwidth unit tests (U1) ---

func makeLogLine(status string, bytes int64, dayOffset int) LogLine {
	return LogLine{
		UpstreamCacheStatus: status,
		BodyBytesSent:       bytes,
		Time:                time.Date(2026, 1, 1+dayOffset, 12, 0, 0, 0, time.UTC),
	}
}

func TestAccrueBandwidth_HIT(t *testing.T) {
	bw := map[string]*bwDay{}
	accrueBandwidth(bw, makeLogLine("HIT", 1000, 0))
	d := bw["2026-01-01"]
	if d == nil {
		t.Fatal("expected bucket for 2026-01-01")
	}
	if d.totalBytes != 1000 || d.totalReqs != 1 || d.hitBytes != 1000 || d.hitReqs != 1 {
		t.Fatalf("HIT: got total=%d/%d hit=%d/%d, want 1000/1/1000/1", d.totalBytes, d.totalReqs, d.hitBytes, d.hitReqs)
	}
}

func TestAccrueBandwidth_MISS(t *testing.T) {
	bw := map[string]*bwDay{}
	accrueBandwidth(bw, makeLogLine("MISS", 500, 0))
	d := bw["2026-01-01"]
	if d == nil {
		t.Fatal("expected bucket for 2026-01-01")
	}
	if d.totalBytes != 500 || d.totalReqs != 1 || d.hitBytes != 0 || d.hitReqs != 0 {
		t.Fatalf("MISS: got total=%d/%d hit=%d/%d, want 500/1/0/0", d.totalBytes, d.totalReqs, d.hitBytes, d.hitReqs)
	}
}

func TestAccrueBandwidth_SameDayAccumulates(t *testing.T) {
	bw := map[string]*bwDay{}
	accrueBandwidth(bw, makeLogLine("HIT", 1000, 0))
	accrueBandwidth(bw, makeLogLine("MISS", 500, 0))
	d := bw["2026-01-01"]
	if d.totalBytes != 1500 || d.totalReqs != 2 || d.hitBytes != 1000 || d.hitReqs != 1 {
		t.Fatalf("got total=%d/%d hit=%d/%d, want 1500/2/1000/1", d.totalBytes, d.totalReqs, d.hitBytes, d.hitReqs)
	}
}

func TestAccrueBandwidth_DifferentDaysSeparateBuckets(t *testing.T) {
	bw := map[string]*bwDay{}
	accrueBandwidth(bw, makeLogLine("HIT", 1000, 0))
	accrueBandwidth(bw, makeLogLine("HIT", 200, 1))
	if len(bw) != 2 {
		t.Fatalf("expected 2 day buckets, got %d", len(bw))
	}
	if bw["2026-01-01"].totalBytes != 1000 || bw["2026-01-02"].totalBytes != 200 {
		t.Fatalf("unexpected bucket values: %+v", bw)
	}
}

// --- BackfillBandwidth tests (U2) ---

// hitLine / missLine are minimal valid access log lines for two different UTC days.
const (
	hitLineDay1  = `[steam] 1.2.3.4 - - [01/Jan/2026:10:00:00 +0000] "GET /depot/730/chunk/a HTTP/1.1" 200 1000 "-" "ua" "HIT" "lancache.steamcontent.com"`
	missLineDay1 = `[steam] 1.2.3.4 - - [01/Jan/2026:11:00:00 +0000] "GET /depot/730/chunk/b HTTP/1.1" 200 500 "-" "ua" "MISS" "lancache.steamcontent.com"`
	hitLineDay2  = `[steam] 1.2.3.4 - - [02/Jan/2026:10:00:00 +0000] "GET /depot/730/chunk/c HTTP/1.1" 200 2000 "-" "ua" "HIT" "lancache.steamcontent.com"`
	dashLine     = `[localhost] 127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /healthz HTTP/1.1" 200 2 "-" "ua" "-" "localhost"`
)

// makeIngesterAtOffset writes lines to a temp log file, creates a tailer backed
// by cat, Polls it to advance the offset to end-of-file, then returns the
// ingester and log path. The tailer offset is therefore at the end of lines.
func makeIngesterAtOffset(t *testing.T, cat *Catalog, lines ...string) (*Ingester, string) {
	t.Helper()
	logPath := writeLogFile(t, lines...)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	// Poll advances the tailer's offset to the end of the current file content.
	if _, err := tailer.Poll(); err != nil {
		t.Fatalf("Poll to advance offset: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)
	return ing, logPath
}

func TestBackfillBandwidth_PopulatesPerDayTotals(t *testing.T) {
	cat := openTestCatalog(t)
	ing, logPath := makeIngesterAtOffset(t, cat, hitLineDay1, missLineDay1, hitLineDay2)

	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}

	tb, hb, tr, hr, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	// day1: 1000+500 total, 1000 hit; day2: 2000 total, 2000 hit
	if tb != 3500 || tr != 3 {
		t.Fatalf("total bytes/reqs = %d/%d, want 3500/3", tb, tr)
	}
	if hb != 3000 || hr != 2 {
		t.Fatalf("hit bytes/reqs = %d/%d, want 3000/2", hb, hr)
	}
}

func TestBackfillBandwidth_SetsEarliestDayAsSince(t *testing.T) {
	cat := openTestCatalog(t)
	ing, logPath := makeIngesterAtOffset(t, cat, hitLineDay2, hitLineDay1) // day2 first in file

	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}

	since, err := cat.BandwidthSince()
	if err != nil {
		t.Fatalf("BandwidthSince: %v", err)
	}
	if since != "2026-01-01" {
		t.Fatalf("bandwidth_since = %q, want 2026-01-01", since)
	}
}

func TestBackfillBandwidth_OffsetBoundRespected(t *testing.T) {
	cat := openTestCatalog(t)
	// Advance offset over only day1 lines, then append day2 line after.
	ing, logPath := makeIngesterAtOffset(t, cat, hitLineDay1)

	// Append a line that falls after the offset -- must not be counted.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := fmt.Fprintln(f, hitLineDay2); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}

	tb, _, tr, _, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tb != 1000 || tr != 1 {
		t.Fatalf("got %d bytes / %d reqs, want 1000/1 (post-offset line must be excluded)", tb, tr)
	}
}

func TestBackfillBandwidth_PartialTrailingLineSkipped(t *testing.T) {
	cat := openTestCatalog(t)
	// Write a complete line, then append a partial line (no trailing newline)
	// within the offset window.
	logPath := filepath.Join(t.TempDir(), "access.log")
	content := hitLineDay1 + "\n" + hitLineDay2[:20] // partial, no newline
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	// Poll advances offset to len(content) -- which includes the partial line.
	if _, err := tailer.Poll(); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)

	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}

	tb, _, tr, _, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	// Only the complete line should be counted.
	if tb != 1000 || tr != 1 {
		t.Fatalf("got %d bytes / %d reqs, want 1000/1 (partial line must be skipped)", tb, tr)
	}
}

func TestBackfillBandwidth_ZeroOffsetIsNoop(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t, hitLineDay1)
	// Create tailer but do NOT Poll -- offset stays 0.
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)

	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}

	_, _, tr, _, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tr != 0 {
		t.Fatalf("expected 0 rows (no-op), got %d reqs", tr)
	}
}

func TestBackfillBandwidth_DashLinesNotCounted(t *testing.T) {
	cat := openTestCatalog(t)
	ing, logPath := makeIngesterAtOffset(t, cat, dashLine, hitLineDay1)

	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}

	tb, _, tr, _, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tb != 1000 || tr != 1 {
		t.Fatalf("got %d bytes / %d reqs, want 1000/1 (dash line must not count)", tb, tr)
	}
}

func TestBackfillBandwidth_RotatedLogIsNoop(t *testing.T) {
	cat := openTestCatalog(t)
	// Poll the original file so the tailer records its inode and offset.
	ing, logPath := makeIngesterAtOffset(t, cat, hitLineDay1)

	// Simulate log rotation: rename the current file away and create a new
	// one at the same path. The new file gets a different inode.
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(hitLineDay2+"\n"), 0o644); err != nil {
		t.Fatalf("write new log: %v", err)
	}

	// BackfillBandwidth must detect the inode mismatch and be a no-op.
	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth after rotation: %v", err)
	}

	_, _, tr, _, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tr != 0 {
		t.Fatalf("expected 0 rows after rotated-log backfill, got %d reqs", tr)
	}
}

// --- U3 boundary and startup-gate tests ---

// TestBackfillPlusForward_NoDoubleCount is the load-bearing invariant test:
// backfill [0, offset) + forward RunOnce from [offset, ∞) must equal a single
// full-log accrual. If this test fails, the partition design is broken.
func TestBackfillPlusForward_NoDoubleCount(t *testing.T) {
	// Write two historical lines, Poll to capture their offset, then append a
	// third line that belongs to the forward counter.
	cat := openTestCatalog(t)
	logPath := writeLogFile(t, hitLineDay1, missLineDay1)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	// Advance offset to end of historical lines.
	if _, err := tailer.Poll(); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	historicalOffset := tailer.Offset()

	// Append a "future" line after the offset.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := fmt.Fprintln(f, hitLineDay2); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)

	// Backfill covers [0, historicalOffset).
	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}
	// Forward counter picks up the appended line from [historicalOffset, ∞).
	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	tb, hb, tr, hr, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	// Expected: hitLineDay1(1000 HIT) + missLineDay1(500 MISS) + hitLineDay2(2000 HIT)
	if tb != 3500 || tr != 3 {
		t.Fatalf("total = %d bytes / %d reqs, want 3500/3", tb, tr)
	}
	if hb != 3000 || hr != 2 {
		t.Fatalf("hit = %d bytes / %d reqs, want 3000/2", hb, hr)
	}
	_ = historicalOffset // used above to assert the appended line is past it
}

func TestStartupGate_SkipsBackfillWhenTableNonEmpty(t *testing.T) {
	cat := openTestCatalog(t)
	// Pre-populate bandwidth_daily with one entry.
	if err := cat.AddBandwidth("2026-01-01", 999, 1, 0, 0); err != nil {
		t.Fatalf("AddBandwidth: %v", err)
	}

	logPath := writeLogFile(t, hitLineDay2)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	if _, err := tailer.Poll(); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)

	// Simulate the startup gate: only call BackfillBandwidth when table is empty.
	empty, err := cat.IsBandwidthEmpty()
	if err != nil {
		t.Fatalf("IsBandwidthEmpty check: %v", err)
	}
	if empty {
		if err := ing.BackfillBandwidth(logPath); err != nil {
			t.Fatalf("BackfillBandwidth: %v", err)
		}
	}

	// Table should still hold only the pre-existing 999 bytes entry.
	tb, _, _, _, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tb != 999 {
		t.Fatalf("total bytes = %d, want 999 (backfill must not run when table non-empty)", tb)
	}
}

func TestStartupGate_FreshInstallForwardPathCorrect(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t, hitLineDay1, hitLineDay2)
	// Tailer at offset 0 (not yet Polled) -- fresh install scenario.
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)

	// Startup gate: table is empty AND offset == 0 -- backfill is a no-op.
	if err := ing.BackfillBandwidth(logPath); err != nil {
		t.Fatalf("BackfillBandwidth: %v", err)
	}
	// Forward counter reads the whole log (tailer was at 0).
	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	tb, hb, tr, hr, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	// hitLineDay1(1000 HIT) + hitLineDay2(2000 HIT)
	if tb != 3000 || tr != 2 || hb != 3000 || hr != 2 {
		t.Fatalf("got total=%d/%d hit=%d/%d, want 3000/2/3000/2", tb, tr, hb, hr)
	}
}

func TestIngester_SteamDepotResolvesToRealGame(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/730/chunk/abc HTTP/1.1" 200 1000 "-" "Valve/Steam HTTP Client 1.0" "HIT" "lancache.steamcontent.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	steam := &fakeSteamLookup{depotToApp: map[int]int{730: 730}, appNames: map[int]string{730: "Counter-Strike 2"}}
	ing := NewIngester(tailer, testIngestPlatforms(), steam, cat)

	n, err := ing.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 line ingested, got %d", n)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "Counter-Strike 2" || entries[0].GameKey != "730" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].SizeBytes != 1000 {
		t.Fatalf("expected size 1000, got %d", entries[0].SizeBytes)
	}
}

func TestIngester_UnmappedSteamDepotFallsBackToPerDepotEntry(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/999999/chunk/abc HTTP/1.1" 200 2000000 "-" "Valve/Steam HTTP Client 1.0" "HIT" "lancache.steamcontent.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	steam := &fakeSteamLookup{}
	ing := NewIngester(tailer, testIngestPlatforms(), steam, cat)

	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Unresolved depots are individually tracked (depot:N) so they remain
	// addressable for later name resolution via the Steam Store API. They are
	// guest content, so they surface via GuestGames(), not the owned-games
	// Library (List excludes depot:/unidentified by design).
	entries, err := cat.GuestGames(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("GuestGames: %v", err)
	}
	if len(entries) != 1 || entries[0].GameKey != "depot:999999" {
		t.Fatalf("expected per-depot entry depot:999999, got %+v", entries)
	}
}

func TestIngester_NilSteamLookupDoesNotBlockIngestion(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/730/chunk/abc HTTP/1.1" 200 2000000 "-" "Valve/Steam HTTP Client 1.0" "HIT" "lancache.steamcontent.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}

	// Steam resolution unavailable (e.g. no API key configured) -- must
	// degrade to per-depot tracking, not error or block ingestion.
	ing := NewIngester(tailer, testIngestPlatforms(), nil, cat)

	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Per-depot fallback is guest content -- assert via GuestGames(), not the
	// owned-games Library.
	entries, err := cat.GuestGames(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("GuestGames: %v", err)
	}
	if len(entries) != 1 || entries[0].GameKey != "depot:730" {
		t.Fatalf("expected per-depot entry depot:730, got %+v", entries)
	}
}

func TestIngester_BattlenetKnownPrefixResolves(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		`[dist.blizzard.com] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /tpr/wow/wow-update-12345.zip HTTP/1.1" 200 2000 "-" "Blizzard Agent" "HIT" "dist.blizzard.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), nil, cat)

	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "World of Warcraft" {
		t.Fatalf("expected World of Warcraft, got %+v", entries)
	}
}

func TestIngester_UnclassifiableHostFallsBackToHostBucket(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		`[unknownhost.example.com] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET / HTTP/1.1" 200 2000000 "-" "curl/8.6.0" "HIT" "unknownhost.example.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), nil, cat)

	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// An unclassifiable host becomes a "<host>:unidentified" guest bucket, so
	// it surfaces via GuestGames(), not the owned-games Library.
	entries, err := cat.GuestGames(MinLibrarySizeBytes)
	if err != nil {
		t.Fatalf("GuestGames: %v", err)
	}
	if len(entries) != 1 || entries[0].Platform != "unknownhost.example.com" {
		t.Fatalf("expected fallback to the literal host as platform, got %+v", entries)
	}
}

func TestIngester_DirectReturnLinesSkipped(t *testing.T) {
	cat := openTestCatalog(t)
	// Healthcheck, 403, and other direct nginx return() responses log
	// upstream_cache_status="-" (no cache interaction). These must not
	// pollute the catalog -- only HIT/MISS/EXPIRED/etc. carry real
	// accounting data.
	logPath := writeLogFile(t,
		`[localhost] 127.0.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /healthz HTTP/1.1" 200 2 "-" "Wget" "-" "localhost"`,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/730/chunk/abc HTTP/1.1" 200 1000 "-" "Valve/Steam HTTP Client 1.0" "MISS" "lancache.steamcontent.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	steam := &fakeSteamLookup{depotToApp: map[int]int{730: 730}, appNames: map[int]string{730: "Counter-Strike 2"}}
	ing := NewIngester(tailer, testIngestPlatforms(), steam, cat)

	n, err := ing.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 ingested (the MISS line), got %d", n)
	}
	entries, err := cat.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "Counter-Strike 2" {
		t.Fatalf("expected only the real cache entry, got %+v", entries)
	}
}

func TestIngester_MalformedLineIsSkippedNotFatal(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		"this is not a valid log line",
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/730/chunk/abc HTTP/1.1" 200 1000 "-" "Valve/Steam HTTP Client 1.0" "HIT" "lancache.steamcontent.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	steam := &fakeSteamLookup{depotToApp: map[int]int{730: 730}, appNames: map[int]string{730: "Counter-Strike 2"}}
	ing := NewIngester(tailer, testIngestPlatforms(), steam, cat)

	n, err := ing.RunOnce()
	if err != nil {
		t.Fatalf("expected the malformed line to be skipped, not returned as an error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly the 1 valid line to be ingested, got %d", n)
	}
}

// TestIngester_RecordsBandwidthHitVsTotal proves the ingest counter (FEATURES
// #1): every served line counts toward total bytes/requests, only HITs toward
// "saved", and direct-return ("-") lines count toward neither.
func TestIngester_RecordsBandwidthHitVsTotal(t *testing.T) {
	cat := openTestCatalog(t)
	logPath := writeLogFile(t,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/730/chunk/a HTTP/1.1" 200 1000 "-" "ua" "HIT" "lancache.steamcontent.com"`,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:48 +0000] "GET /depot/730/chunk/b HTTP/1.1" 200 500 "-" "ua" "MISS" "lancache.steamcontent.com"`,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:49 +0000] "GET /depot/730/chunk/c HTTP/1.1" 200 200 "-" "ua" "EXPIRED" "lancache.steamcontent.com"`,
		`[steam] 172.20.0.1 - - [19/Jun/2026:21:54:50 +0000] "GET /healthz HTTP/1.1" 200 2 "-" "ua" "-" "lancache.steamcontent.com"`,
	)
	tailer, err := NewTailer(logPath, cat)
	if err != nil {
		t.Fatalf("NewTailer: %v", err)
	}
	ing := NewIngester(tailer, testIngestPlatforms(), &fakeSteamLookup{}, cat)
	if _, err := ing.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	tb, hb, tr, hr, err := cat.BandwidthTotals("")
	if err != nil {
		t.Fatalf("BandwidthTotals: %v", err)
	}
	if tb != 1700 || tr != 3 { // 1000+500+200 over 3 lines; the "-" line excluded
		t.Fatalf("served = %d bytes / %d reqs, want 1700/3", tb, tr)
	}
	if hb != 1000 || hr != 1 { // only the HIT
		t.Fatalf("from-cache = %d bytes / %d reqs, want 1000/1", hb, hr)
	}
	if since, _ := cat.BandwidthSince(); since != "2026-06-19" {
		t.Fatalf("BandwidthSince = %q, want 2026-06-19", since)
	}
}
