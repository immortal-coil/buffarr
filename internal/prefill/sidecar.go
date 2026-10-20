// Package prefill drives the steam-lancache-prefill sidecar (R13, R14):
// the backend writes the app-selection file directly and triggers a
// non-interactive run via a shared-volume handshake (KTD9), since the
// tool has no CLI flag for passing app IDs and its own selection format
// is an undocumented integration point discovered via source inspection
// of the pinned image, not a stable public contract.
package prefill

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	StateSuccess = "success"
	StateFailed  = "failed"
)

// Status mirrors the JSON the sidecar's entrypoint writes to
// PrefillStatusPath (docker/prefill/entrypoint.sh's write_status).
type Status struct {
	RequestID  string    `json:"request_id"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exit_code"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error"`
}

var sizeUnits = map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}

// ParseTotalBytes converts a size string returned by ParseProgress ("3.23 GiB",
// "512 MiB", etc.) to bytes. Returns 0 for unrecognized formats.
func ParseTotalBytes(s string) int64 {
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) != 2 {
		return 0
	}
	n, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || n < 0 {
		return 0
	}
	mult, ok := sizeUnits[parts[1]]
	if !ok {
		return 0
	}
	return int64(n * float64(mult))
}

var (
	// ErrAlreadyRunning means the trigger lock (the trigger file's mere
	// existence) is currently held -- a prefill is pending pickup,
	// actively running, or has just finished and not yet had its status
	// written. The sidecar's entrypoint only removes the trigger file
	// after writing a terminal status, on every path (success, non-zero
	// exit, or timeout), so this lock is guaranteed to eventually release
	// rather than wedge on a hang.
	ErrAlreadyRunning = errors.New("a prefill is already running")

	// ErrNotOwned means a requested app ID isn't in the operator's owned-
	// games list (R13's "owned games" scope) -- rejected before anything
	// is written, per the plan's edge case.
	ErrNotOwned = errors.New("app is not in the operator's owned-games list")

	ErrNoAppIDs = errors.New("no app IDs given")
)

// Sidecar is the backend's side of the handshake: the shared-volume paths it
// writes to and polls (config.Config's PrefillTriggerPath, PrefillStatusPath,
// PrefillSelectionPath) plus logPath, the sidecar's --no-ansi run log that
// Progress tails read-only for a real download percentage.
type Sidecar struct {
	triggerPath   string
	statusPath    string
	selectionPath string
	logPath       string
}

func NewSidecar(triggerPath, statusPath, selectionPath, logPath string) *Sidecar {
	return &Sidecar{triggerPath: triggerPath, statusPath: statusPath, selectionPath: selectionPath, logPath: logPath}
}

// SteamAuthenticated reports whether the steam-lancache-prefill sidecar has
// completed its one-time interactive Steam login. v2.2.2 writes
// account.config (binary JWT store); earlier docs incorrectly cited
// AccountSettingsStore.json. We check both so a version bump either way
// doesn't silently break the settings-page indicator.
func (s *Sidecar) SteamAuthenticated() bool {
	dir := filepath.Dir(s.selectionPath)
	for _, name := range []string{"account.config", "AccountSettingsStore.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// IsRunning reports whether the trigger lock is currently held.
func (s *Sidecar) IsRunning() (bool, error) {
	_, err := os.Stat(s.triggerPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking trigger lock: %w", err)
}

// prefillPercentRe matches steam-lancache-prefill's --no-ansi per-tick line,
// e.g. "Downloading..: 25%". prefillTotalRe matches the one-shot run-total
// line, e.g. "Downloading 3.23 GiB" (capital D, a space, then a number) --
// the leading "[2:55:11 AM] " timestamp and any box-drawing noise around it
// are tolerated because neither pattern is line-anchored. The lowercase
// "downloading manifests" retry line can't match prefillTotalRe (capital D)
// and has no "..:%", so it's ignored by both.
var (
	prefillPercentRe = regexp.MustCompile(`Downloading\.\.:\s*(\d{1,3})%`)
	prefillTotalRe   = regexp.MustCompile(`Downloading\s+([0-9][0-9.]*\s*[KMGT]iB)`)
)

// ParseProgress scans steam-lancache-prefill's --no-ansi log bytes and returns
// the last reported download percent plus the run's total size string. ok=false
// means "no confident percent" -- the caller must fall back to the indeterminate
// bar, never render a misleading 0%. The log format is the same undocumented
// integration point as the rest of this package (KTD9), so parsing is
// deliberately lenient: it operates line-wise on raw bytes (never assuming valid
// UTF-8, so box-drawing summary tables don't break it) and bails to ok=false
// rather than guessing. phase is "complete" once the terminal "Prefill complete!"
// marker appears (the run is done — the caller snaps the bar to 100%),
// "downloading" once a percent line appears, or "preparing" during the
// login/manifest/connect stage before the first percent.
func ParseProgress(log []byte) (pct int, phase, total string, ok bool) {
	var (
		sawPercent  bool
		sawComplete bool
		lastPct     int
		preparing   bool
	)
	for _, line := range bytes.Split(log, []byte("\n")) {
		// The terminal marker steam-lancache-prefill prints when a run finishes.
		// It jumps straight from "Downloading..: 99%" to this line and never logs
		// 100%, so completion can only be read from the marker, not the percent.
		if bytes.Contains(line, []byte("Prefill complete!")) {
			sawComplete = true
			continue
		}
		if m := prefillPercentRe.FindSubmatch(line); m != nil {
			if n, err := strconv.Atoi(string(m[1])); err == nil {
				lastPct = n
				sawPercent = true
			}
			continue
		}
		if total == "" {
			if m := prefillTotalRe.FindSubmatch(line); m != nil {
				total = strings.TrimSpace(string(m[1]))
				preparing = true
				continue
			}
		}
		if !preparing &&
			(bytes.Contains(line, []byte("Fetching depot manifests")) ||
				bytes.Contains(line, []byte("Starting "))) {
			preparing = true
		}
	}

	if lastPct < 0 {
		lastPct = 0
	}
	if lastPct > 100 {
		lastPct = 100
	}

	switch {
	case sawComplete:
		// "complete" outranks "downloading": once the marker is present the run
		// is done regardless of the last percent line. The parser stays honest
		// and reports the last percent it actually saw (98/99); the caller snaps
		// the *display* to 100% so the operator never sees a parked 99%.
		return lastPct, "complete", total, true
	case sawPercent:
		return lastPct, "downloading", total, true
	case preparing:
		return 0, "preparing", total, true
	default:
		return 0, "", "", false
	}
}

// Progress reads the sidecar's --no-ansi run log (best-effort) and returns the
// latest parsed download percent. A missing/unreadable log, a stale prior-run
// log, or an unrecognized format all yield ok=false so the caller renders the
// honest indeterminate bar instead of a misleading fraction.
func (s *Sidecar) Progress() (pct int, phase, total string, ok bool) {
	if s.logPath == "" {
		return 0, "", "", false
	}
	logInfo, err := os.Stat(s.logPath)
	if err != nil {
		return 0, "", "", false
	}
	// Reject a stale prior-run log. The backend writes the trigger lock BEFORE
	// the sidecar truncates last-run.log for the new run (entrypoint redirects
	// with `>`), so during that swap window the log still holds the previous
	// run's final 98%. Trusting the percent only when the log is at least as new
	// as the trigger keeps that stale value from leaking into a fresh run. We
	// gate on mtime, not name-matching "Starting <game>", because name-match
	// breaks when the same game is loaded twice in a row.
	if trigInfo, err := os.Stat(s.triggerPath); err == nil && logInfo.ModTime().Before(trigInfo.ModTime()) {
		return 0, "", "", false
	}
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		return 0, "", "", false
	}
	return ParseProgress(data)
}

// Trigger validates appIDs against ownedAppIDs (R13: only owned games),
// rejects if a prefill is already running (KTD9's locking, not queued or
// silently overwritten), then writes the selection file in
// steam-lancache-prefill's own bare-JSON-array format followed by the
// trigger lock. Write order matters: the entrypoint watches only the
// trigger file's path and assumes the selection file is already complete
// by the time it fires, so the selection write must land first.
func (s *Sidecar) Trigger(appIDs []int, ownedAppIDs map[int]bool) (requestID string, err error) {
	if len(appIDs) == 0 {
		return "", ErrNoAppIDs
	}
	for _, id := range appIDs {
		if !ownedAppIDs[id] {
			return "", fmt.Errorf("%w: app %d", ErrNotOwned, id)
		}
	}

	running, err := s.IsRunning()
	if err != nil {
		return "", err
	}
	if running {
		return "", ErrAlreadyRunning
	}

	requestID, err = randomToken()
	if err != nil {
		return "", fmt.Errorf("generating request id: %w", err)
	}

	selectionJSON, err := json.Marshal(appIDs)
	if err != nil {
		return "", fmt.Errorf("encoding selection file: %w", err)
	}
	if err := atomicWriteFile(s.selectionPath, selectionJSON); err != nil {
		return "", fmt.Errorf("writing selection file (shared volume may be unwritable): %w", err)
	}
	if err := atomicWriteFile(s.triggerPath, []byte(requestID)); err != nil {
		return "", fmt.Errorf("writing trigger file (shared volume may be unwritable): %w", err)
	}

	return requestID, nil
}

// Cancel removes the trigger lock, allowing a new prefill to be started.
// Safe to call while a prefill is actively running -- the sidecar process
// keeps running to completion and cleans up on its own; this just unblocks
// the backend's IsRunning() check for recovery from wedged states.
func (s *Sidecar) Cancel() error {
	if err := os.Remove(s.triggerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing trigger lock: %w", err)
	}
	return nil
}

// PollStatus returns the result for requestID if the sidecar has written
// it yet. found is false both when no status exists yet and when the
// status on disk is for a *different* (stale, previous) request -- the
// caller should keep waiting in either case, not treat a stale result as
// this request's answer.
func (s *Sidecar) PollStatus(requestID string) (Status, bool, error) {
	data, err := os.ReadFile(s.statusPath)
	if os.IsNotExist(err) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("reading status file: %w", err)
	}

	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return Status{}, false, fmt.Errorf("parsing status file: %w", err)
	}
	if st.RequestID != requestID {
		return Status{}, false, nil
	}
	return st, true, nil
}

// LatestStatus returns the most recent terminal status the sidecar has
// written, regardless of which request it was for. Unlike PollStatus it does
// NOT match against a request ID -- it's for restart recovery, where the
// backend has lost the in-memory request→game mapping but still needs to
// reconcile a pending Pre-Stage against whatever the sidecar last finished.
// found is false when no status file exists yet.
func (s *Sidecar) LatestStatus() (Status, bool, error) {
	data, err := os.ReadFile(s.statusPath)
	if os.IsNotExist(err) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("reading status file: %w", err)
	}
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return Status{}, false, fmt.Errorf("parsing status file: %w", err)
	}
	return st, true, nil
}

// WaitForCompletion polls PollStatus until it finds requestID's result or
// ctx is done. The sidecar's own `timeout`-wrapped run (docker/prefill/
// entrypoint.sh) is the authoritative bound on a single run; ctx's
// deadline here is a backstop for the entrypoint process itself dying
// without writing a status (e.g. the container being killed), and should
// be set comfortably longer than PREFILL_MAX_RUNTIME_SECONDS, not equal
// to it.
func (s *Sidecar) WaitForCompletion(ctx context.Context, requestID string, pollInterval time.Duration) (Status, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		st, found, err := s.PollStatus(requestID)
		if err != nil {
			return Status{}, err
		}
		if found {
			return st, nil
		}

		select {
		case <-ctx.Done():
			return Status{}, fmt.Errorf("waiting for prefill request %s to complete: %w", requestID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// atomicWriteFile writes content to path via a temp file + rename in the
// same directory, so the sidecar's file watcher (which reacts to
// create/close_write/moved_to events) never observes a partially-written
// file -- the same pattern internal/nginx and internal/dnsmasq each use
// for their own watched files.
func atomicWriteFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".prefill-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
	}
	return nil
}
