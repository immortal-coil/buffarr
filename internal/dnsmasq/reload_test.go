package dnsmasq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFile_WritesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cdn-hosts")

	if err := WriteFile(path, "10.0.0.5 example.com\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if string(got) != "10.0.0.5 example.com\n" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestWriteFile_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cdn-hosts")

	if err := WriteFile(path, "content"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file (the rename target), got %d: %v", len(entries), entries)
	}
}

func TestWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cdn-wildcards.conf")

	if err := WriteFile(path, "address=/old.example.com/10.0.0.5\n"); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := WriteFile(path, "address=/new.example.com/10.0.0.5\n"); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if strings.Contains(string(got), "old.example.com") {
		t.Fatalf("stale content from prior write still present: %q", got)
	}
	if !strings.Contains(string(got), "new.example.com") {
		t.Fatalf("new content missing after overwrite: %q", got)
	}
}
