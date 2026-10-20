package catalog

import (
	"testing"
	"time"
)

func TestParseLine_HappyPath(t *testing.T) {
	line := `[steam] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET /depot/730/chunk/abc123 HTTP/1.1" 200 1048576 "-" "Valve/Steam HTTP Client 1.0" "HIT" "lancache.steamcontent.com"`

	got, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}

	if got.CacheIdentifier != "steam" {
		t.Errorf("CacheIdentifier = %q, want steam", got.CacheIdentifier)
	}
	if got.Method != "GET" {
		t.Errorf("Method = %q, want GET", got.Method)
	}
	if got.URI != "/depot/730/chunk/abc123" {
		t.Errorf("URI = %q, want /depot/730/chunk/abc123", got.URI)
	}
	if got.Status != 200 {
		t.Errorf("Status = %d, want 200", got.Status)
	}
	if got.BodyBytesSent != 1048576 {
		t.Errorf("BodyBytesSent = %d, want 1048576", got.BodyBytesSent)
	}
	if got.UpstreamCacheStatus != "HIT" {
		t.Errorf("UpstreamCacheStatus = %q, want HIT", got.UpstreamCacheStatus)
	}
	if got.Host != "lancache.steamcontent.com" {
		t.Errorf("Host = %q, want lancache.steamcontent.com", got.Host)
	}

	wantTime := time.Date(2026, time.June, 19, 21, 54, 47, 0, time.UTC)
	if !got.Time.Equal(wantTime) {
		t.Errorf("Time = %v, want %v", got.Time, wantTime)
	}
}

func TestParseLine_CacheIdentifierFallsBackToHost(t *testing.T) {
	line := `[example.com] 172.20.0.1 - - [19/Jun/2026:21:54:47 +0000] "GET / HTTP/1.1" 200 559 "-" "curl/8.6.0" "HIT" "example.com"`

	got, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if got.CacheIdentifier != "example.com" {
		t.Errorf("CacheIdentifier = %q, want example.com", got.CacheIdentifier)
	}
}

func TestParseLine_MalformedLineReturnsError(t *testing.T) {
	if _, err := ParseLine("this is not a valid log line"); err == nil {
		t.Fatalf("expected an error for a malformed line")
	}
}

func TestCacheKey_ConcatenatesIdentifierAndURI(t *testing.T) {
	line := LogLine{CacheIdentifier: "steam", URI: "/depot/730/chunk/abc123"}
	if got := CacheKey(line); got != "steam/depot/730/chunk/abc123" {
		t.Fatalf("CacheKey = %q, want steam/depot/730/chunk/abc123", got)
	}
}
