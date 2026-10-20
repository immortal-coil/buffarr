package prefill

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestSidecar(t *testing.T) (*Sidecar, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	triggerPath := filepath.Join(dir, "trigger")
	statusPath := filepath.Join(dir, "status.json")
	selectionPath := filepath.Join(dir, "selectedAppsToPrefill.json")
	logPath := filepath.Join(dir, "last-run.log")
	return NewSidecar(triggerPath, statusPath, selectionPath, logPath), triggerPath, statusPath, selectionPath
}

func TestTrigger_HappyPath_WritesSelectionThenTrigger(t *testing.T) {
	s, triggerPath, _, selectionPath := newTestSidecar(t)
	owned := map[int]bool{730: true}

	requestID, err := s.Trigger([]int{730}, owned)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if requestID == "" {
		t.Fatalf("expected a non-empty request ID")
	}

	selection, err := os.ReadFile(selectionPath)
	if err != nil {
		t.Fatalf("reading selection file: %v", err)
	}
	var appIDs []int
	if err := json.Unmarshal(selection, &appIDs); err != nil {
		t.Fatalf("expected the selection file to be a bare JSON array (steam-lancache-prefill's own format), got %q: %v", selection, err)
	}
	if len(appIDs) != 1 || appIDs[0] != 730 {
		t.Fatalf("unexpected selection content: %v", appIDs)
	}

	gotRequestID, err := os.ReadFile(triggerPath)
	if err != nil {
		t.Fatalf("reading trigger file: %v", err)
	}
	if string(gotRequestID) != requestID {
		t.Fatalf("expected the trigger file to contain the raw request ID, got %q", gotRequestID)
	}
}

// TestTrigger_NonOwnedAppRejected covers the plan's edge case: an app ID
// not in the operator's owned-games list is rejected before any selection
// file is written.
func TestTrigger_NonOwnedAppRejected(t *testing.T) {
	s, _, _, selectionPath := newTestSidecar(t)
	owned := map[int]bool{730: true}

	_, err := s.Trigger([]int{440}, owned)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("expected ErrNotOwned, got %v", err)
	}

	if _, statErr := os.Stat(selectionPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no selection file written for a rejected non-owned app")
	}
}

func TestTrigger_PartiallyOwnedSelectionRejectedEntirely(t *testing.T) {
	s, _, _, selectionPath := newTestSidecar(t)
	owned := map[int]bool{730: true}

	// One owned, one not -- the whole batch is rejected rather than
	// silently prefilling only the owned subset.
	_, err := s.Trigger([]int{730, 999}, owned)
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("expected ErrNotOwned, got %v", err)
	}
	if _, statErr := os.Stat(selectionPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no selection file written when any app in the batch is rejected")
	}
}

func TestTrigger_EmptySelectionRejected(t *testing.T) {
	s, _, _, _ := newTestSidecar(t)
	if _, err := s.Trigger(nil, map[int]bool{}); !errors.Is(err, ErrNoAppIDs) {
		t.Fatalf("expected ErrNoAppIDs, got %v", err)
	}
}

// TestTrigger_ConcurrentTriggerRejected covers the plan's edge case: a
// second prefill trigger while one is already running is rejected, not
// queued or silently overwritten.
func TestTrigger_ConcurrentTriggerRejected(t *testing.T) {
	s, _, _, _ := newTestSidecar(t)
	owned := map[int]bool{730: true, 440: true}

	if _, err := s.Trigger([]int{730}, owned); err != nil {
		t.Fatalf("first Trigger: %v", err)
	}

	if _, err := s.Trigger([]int{440}, owned); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning for a concurrent trigger, got %v", err)
	}

	running, err := s.IsRunning()
	if err != nil || !running {
		t.Fatalf("IsRunning: running=%v err=%v", running, err)
	}
}

func TestIsRunning_FalseWhenNoTrigger(t *testing.T) {
	s, _, _, _ := newTestSidecar(t)
	running, err := s.IsRunning()
	if err != nil || running {
		t.Fatalf("expected IsRunning=false with no trigger written, got running=%v err=%v", running, err)
	}
}

func TestPollStatus_NotFoundBeforeAnyStatusWritten(t *testing.T) {
	s, _, _, _ := newTestSidecar(t)
	_, found, err := s.PollStatus("some-request-id")
	if err != nil || found {
		t.Fatalf("expected found=false before any status exists, got found=%v err=%v", found, err)
	}
}

func writeStatusFile(t *testing.T, path string, st Status) {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshaling status: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing status file: %v", err)
	}
}

func TestPollStatus_StaleResultFromPriorRequestIsNotFound(t *testing.T) {
	s, _, statusPath, _ := newTestSidecar(t)
	writeStatusFile(t, statusPath, Status{RequestID: "old-request", State: StateSuccess, ExitCode: 0})

	_, found, err := s.PollStatus("new-request")
	if err != nil || found {
		t.Fatalf("expected a stale status (different request id) to read as not-found, got found=%v err=%v", found, err)
	}
}

func TestPollStatus_MatchingRequestIDFound(t *testing.T) {
	s, _, statusPath, _ := newTestSidecar(t)
	want := Status{RequestID: "req-1", State: StateSuccess, ExitCode: 0, FinishedAt: time.Now().UTC().Truncate(time.Second)}
	writeStatusFile(t, statusPath, want)

	got, found, err := s.PollStatus("req-1")
	if err != nil || !found {
		t.Fatalf("PollStatus: found=%v err=%v", found, err)
	}
	if got.State != StateSuccess || got.ExitCode != 0 {
		t.Fatalf("unexpected status: %+v", got)
	}
}

// TestPollStatus_FailedExitCodeSurfaced covers the plan's error path: the
// sidecar process exits non-zero; the backend surfaces a failed-prefill
// state rather than leaving the operator unsure whether it succeeded.
func TestPollStatus_FailedExitCodeSurfaced(t *testing.T) {
	s, _, statusPath, _ := newTestSidecar(t)
	writeStatusFile(t, statusPath, Status{RequestID: "req-1", State: StateFailed, ExitCode: 1, Error: "SteamPrefill exited non-zero"})

	got, found, err := s.PollStatus("req-1")
	if err != nil || !found {
		t.Fatalf("PollStatus: found=%v err=%v", found, err)
	}
	if got.State != StateFailed || got.ExitCode != 1 {
		t.Fatalf("expected a failed state surfaced, got %+v", got)
	}
}

func TestWaitForCompletion_ReturnsOnceStatusAppears(t *testing.T) {
	s, _, statusPath, _ := newTestSidecar(t)

	go func() {
		time.Sleep(20 * time.Millisecond)
		writeStatusFile(t, statusPath, Status{RequestID: "req-1", State: StateSuccess, ExitCode: 0})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := s.WaitForCompletion(ctx, "req-1", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if got.State != StateSuccess {
		t.Fatalf("unexpected status: %+v", got)
	}
}

// TestWaitForCompletion_TimeoutSurfacedAsHang covers the plan's error
// path: the sidecar hangs past the maximum-runtime timeout; the backend
// marks it failed/cancellable rather than waiting indefinitely. This is
// the backend-side backstop -- the authoritative timeout lives in the
// sidecar's entrypoint (docker/prefill/entrypoint.sh's `timeout` wrapper),
// which guarantees the trigger lock always releases; this test covers the
// case where even the entrypoint process itself never reports back.
func TestWaitForCompletion_TimeoutSurfacedAsHang(t *testing.T) {
	s, _, _, _ := newTestSidecar(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := s.WaitForCompletion(ctx, "req-1", 5*time.Millisecond)
	if err == nil {
		t.Fatalf("expected WaitForCompletion to surface a timeout rather than hang indefinitely")
	}
}

// TestParseProgress_Fixture pins the parser against a byte-exact capture of a
// real prod run (Cuphead, 3.23 GiB), control bytes and box-drawing summary
// table included -- the most load-bearing case, since the whole feature rests
// on this undocumented --no-ansi format.
func TestParseProgress_Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "last-run-cuphead.log"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	// The fixture is a *finished* run -- it ends with "Prefill complete!", so the
	// terminal marker outranks the last "Downloading..: 98%" line and the phase is
	// "complete". The parser still reports the last percent it actually saw (98);
	// the caller is what snaps the displayed bar to 100%.
	pct, phase, total, ok := ParseProgress(data)
	if !ok || pct != 98 || phase != "complete" || total != "3.23 GiB" {
		t.Fatalf("ParseProgress(fixture) = (%d, %q, %q, %v); want (98, \"complete\", \"3.23 GiB\", true)",
			pct, phase, total, ok)
	}
}

func TestParseProgress_Cases(t *testing.T) {
	cases := []struct {
		name      string
		log       string
		wantPct   int
		wantPhase string
		wantTotal string
		wantOK    bool
	}{
		{
			name:   "empty log -> indeterminate",
			log:    "",
			wantOK: false,
		},
		{
			name:      "preparing: size known, no percent yet",
			log:       "[2:55:08 AM] Starting Cuphead\nFetching depot manifests...\n[2:55:11 AM] Downloading 3.23 GiB\n",
			wantPct:   0,
			wantPhase: "preparing",
			wantTotal: "3.23 GiB",
			wantOK:    true,
		},
		{
			name:      "preparing: Starting before any size line",
			log:       "[2:55:06 AM] Starting login!\nConnecting to Steam...\n",
			wantPct:   0,
			wantPhase: "preparing",
			wantTotal: "",
			wantOK:    true,
		},
		{
			name:   "retry-error noise only -> indeterminate (lowercase 'downloading' must not match)",
			log:    "[2:55:08 AM]    An unexpected error (System.Net.Http.HttpRequestException)\noccurred while downloading manifests.  Retrying...\n",
			wantOK: false,
		},
		{
			name:      "takes the LAST percent, not the first",
			log:       "Downloading 1.00 GiB\nDownloading..: 5%\nDownloading..: 51%\nDownloading..: 75%\n",
			wantPct:   75,
			wantPhase: "downloading",
			wantTotal: "1.00 GiB",
			wantOK:    true,
		},
		{
			name:      "clamps an out-of-range percent to 100",
			log:       "Downloading 1.00 GiB\nDownloading..: 103%\n",
			wantPct:   100,
			wantPhase: "downloading",
			wantTotal: "1.00 GiB",
			wantOK:    true,
		},
		{
			name:      "percent survives surrounding box-drawing bytes",
			log:       "── Downloading..: 42% ││\n",
			wantPct:   42,
			wantPhase: "downloading",
			wantTotal: "",
			wantOK:    true,
		},
		{
			name:      "complete marker outranks the last percent",
			log:       "Downloading 3.23 GiB\nDownloading..: 98%\n[2:55:57 AM] Prefill complete!\n",
			wantPct:   98,
			wantPhase: "complete",
			wantTotal: "3.23 GiB",
			wantOK:    true,
		},
		{
			name:      "complete with no percent line (instant/tiny run)",
			log:       "[2:55:08 AM] Starting Cuphead\nPrefill complete!\n",
			wantPct:   0,
			wantPhase: "complete",
			wantTotal: "",
			wantOK:    true,
		},
		{
			name:      "'Finished in' speed line is NOT complete -- still downloading",
			log:       "Downloading 1.00 GiB\nDownloading..: 98%\n[2:55:57 AM] Finished in 45.6 - 606 Mbit/s\n",
			wantPct:   98,
			wantPhase: "downloading",
			wantTotal: "1.00 GiB",
			wantOK:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pct, phase, total, ok := ParseProgress([]byte(tc.log))
			if ok != tc.wantOK || pct != tc.wantPct || phase != tc.wantPhase || total != tc.wantTotal {
				t.Fatalf("ParseProgress = (%d, %q, %q, %v); want (%d, %q, %q, %v)",
					pct, phase, total, ok, tc.wantPct, tc.wantPhase, tc.wantTotal, tc.wantOK)
			}
		})
	}
}

// TestProgress_Method covers the read-only tail + the stale-log mtime gate that
// keeps a previous run's final percent from leaking into a fresh run during the
// trigger-write / log-truncate swap window.
func TestProgress_Method(t *testing.T) {
	t.Run("missing log -> not ok", func(t *testing.T) {
		s, _, _, _ := newTestSidecar(t)
		if _, _, _, ok := s.Progress(); ok {
			t.Fatalf("Progress() on a missing log should be ok=false")
		}
	})

	t.Run("fresh log parses", func(t *testing.T) {
		s, triggerPath, _, _ := newTestSidecar(t)
		if err := os.WriteFile(triggerPath, []byte("req-1"), 0o644); err != nil {
			t.Fatalf("writing trigger: %v", err)
		}
		// Log written after the trigger -> at least as new -> trusted.
		if err := os.WriteFile(s.logPath, []byte("Downloading 2.00 GiB\nDownloading..: 51%\n"), 0o644); err != nil {
			t.Fatalf("writing log: %v", err)
		}
		pct, _, total, ok := s.Progress()
		if !ok || pct != 51 || total != "2.00 GiB" {
			t.Fatalf("Progress() = (%d, %q, %v); want (51, \"2.00 GiB\", true)", pct, total, ok)
		}
	})

	t.Run("complete log -> phase complete", func(t *testing.T) {
		s, triggerPath, _, _ := newTestSidecar(t)
		if err := os.WriteFile(triggerPath, []byte("req-1"), 0o644); err != nil {
			t.Fatalf("writing trigger: %v", err)
		}
		// A finished run whose entrypoint hasn't cleared the trigger yet: the log
		// already carries the terminal marker, so Progress reports phase=complete.
		if err := os.WriteFile(s.logPath, []byte("Downloading 2.00 GiB\nDownloading..: 99%\nPrefill complete!\n"), 0o644); err != nil {
			t.Fatalf("writing log: %v", err)
		}
		_, phase, _, ok := s.Progress()
		if !ok || phase != "complete" {
			t.Fatalf("Progress() phase = %q (ok=%v); want \"complete\", true", phase, ok)
		}
	})

	t.Run("ParseTotalBytes", func(t *testing.T) {
		cases := []struct {
			in   string
			want int64
		}{
			{"2.00 GiB", 2 * (1 << 30)},
			{"512 MiB", 512 * (1 << 20)},
			{"1 KiB", 1024},
			{"1.5 TiB", int64(1.5 * (1 << 40))},
			{"", 0},
			{"bad", 0},
			{"1.0 MB", 0},
		}
		for _, tc := range cases {
			if got := ParseTotalBytes(tc.in); got != tc.want {
				t.Errorf("ParseTotalBytes(%q) = %d; want %d", tc.in, got, tc.want)
			}
		}
	})

	t.Run("stale log older than trigger is rejected", func(t *testing.T) {
		s, triggerPath, _, _ := newTestSidecar(t)
		// Previous run's log, then a NEWER trigger for a fresh run that hasn't
		// truncated the log yet: the old 98% must not leak through.
		if err := os.WriteFile(s.logPath, []byte("Downloading 9.00 GiB\nDownloading..: 98%\n"), 0o644); err != nil {
			t.Fatalf("writing stale log: %v", err)
		}
		old := time.Now().Add(-1 * time.Minute)
		if err := os.Chtimes(s.logPath, old, old); err != nil {
			t.Fatalf("backdating log: %v", err)
		}
		if err := os.WriteFile(triggerPath, []byte("req-2"), 0o644); err != nil {
			t.Fatalf("writing trigger: %v", err)
		}
		if _, _, _, ok := s.Progress(); ok {
			t.Fatalf("Progress() should reject a log older than the trigger (stale prior-run percent)")
		}
	})
}
