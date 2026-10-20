package nginx

import (
	"os"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPurgeQueue_DeletesFileForEnqueuedPrefix(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/730/chunk/abc")
	q := NewPurgeQueue(dir, "")

	q.Enqueue([]string{"steam/depot/730/"})

	waitFor(t, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}, 2*time.Second, "file to be deleted")
}

func TestPurgeQueue_SkipsNonMatchingFile(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/731/chunk/abc")
	q := NewPurgeQueue(dir, "")

	q.Enqueue([]string{"steam/depot/730/"})

	// Wait long enough for a walk to complete.
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("non-matching file should not be deleted: %v", err)
	}
}

// TestPurgeQueue_DedupesPendingPrefixes: the same prefix enqueued multiple
// times still results in correct deletion (dedup prevents redundant scans).
func TestPurgeQueue_DedupesPendingPrefixes(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/730/chunk/abc")
	q := NewPurgeQueue(dir, "")

	q.Enqueue([]string{"steam/depot/730/"})
	q.Enqueue([]string{"steam/depot/730/"})
	q.Enqueue([]string{"steam/depot/730/"})

	waitFor(t, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}, 2*time.Second, "file to be deleted despite duplicate enqueues")
}

func TestPurgeQueue_EnqueueEmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := makeFakeCacheFile(t, dir, "steam/depot/730/chunk/abc")
	q := NewPurgeQueue(dir, "")

	q.Enqueue(nil)
	q.Enqueue([]string{})

	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should remain after empty enqueue: %v", err)
	}
}

// A purge left unfinished by a restart is resumed from the state file, and
// the state file is emptied once the walk completes.
func TestPurgeQueue_ResumesPersistedPrefixes(t *testing.T) {
	dir := t.TempDir()
	state := t.TempDir() + "/purge-pending.json"
	if err := os.WriteFile(state, []byte(`["steam/depot/730/"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	path := makeFakeCacheFile(t, dir, "steam/depot/730/chunk/abc")
	q := NewPurgeQueue(dir, state)

	waitFor(t, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err) && q.Idle()
	}, 2*time.Second, "persisted prefix to be purged")
	if b, _ := os.ReadFile(state); string(b) != "[]" {
		t.Fatalf("state file should be empty after the walk, got %q", b)
	}
}
