package catalog

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// maxTailChunkBytes caps how much of the unread log region a single Poll
// reads into memory, so a huge backlog drains over several polls instead of
// in one OOM-risking allocation (see Poll).
const maxTailChunkBytes = 8 << 20 // 8 MiB

// StateStore persists tail position across restarts (so a process restart
// resumes mid-file instead of re-ingesting or skipping lines) and across
// rotation (detected via inode change, see Poll).
type StateStore interface {
	LoadTailState(path string) (TailState, bool, error)
	SaveTailState(path string, state TailState) error
}

// Tailer incrementally reads new lines appended to path, surviving both
// restart (persisted offset) and log rotation (inode-change and truncation
// detection) without needing inotify -- a poll interval is precise enough
// for log ingestion and keeps the backend's dependency footprint small.
type Tailer struct {
	path  string
	store StateStore
	file  *os.File
	state TailState
	buf   []byte // bytes read past the last complete newline, carried to the next Poll
}

// NewTailer resumes from any state store, the previous run persisted for
// path. If the file doesn't exist yet (e.g. nginx hasn't started), Poll
// simply returns no lines until it appears.
func NewTailer(path string, store StateStore) (*Tailer, error) {
	t := &Tailer{path: path, store: store}

	if st, found, err := store.LoadTailState(path); err != nil {
		return nil, fmt.Errorf("loading tail state for %s: %w", path, err)
	} else if found {
		t.state = st
	}

	return t, nil
}

func inodeOf(fi os.FileInfo) uint64 {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return sys.Ino
	}
	return 0
}

// Poll reads any lines appended since the last call and returns them.
func (t *Tailer) Poll() ([]string, error) {
	fi, err := os.Stat(t.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stating %s: %w", t.path, err)
	}

	inode := inodeOf(fi)
	rotated := t.state.Inode != 0 && inode != t.state.Inode
	truncated := !rotated && fi.Size() < t.state.Offset

	if t.file == nil || rotated || truncated {
		if t.file != nil {
			t.file.Close()
			t.file = nil
		}
		f, err := os.Open(t.path)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", t.path, err)
		}
		t.file = f
		t.state.Inode = inode
		if rotated || truncated {
			t.state.Offset = 0
			t.buf = nil
		}
		if _, err := t.file.Seek(t.state.Offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seeking %s: %w", t.path, err)
		}
	}

	// Bounded per poll: on first boot against a pre-existing multi-GB access
	// log (offset 0), or after a long outage during heavy traffic, an
	// unbounded ReadAll would load the whole unread region into RAM at once
	// (~3x its size once t.buf and the []string lines are counted) -- an OOM
	// risk on the small boxes this targets. Reading a capped chunk lets the
	// offset advance incrementally; at the 2s poll cadence the backlog drains
	// quickly, and t.file stays open so each poll resumes where the last left
	// off. A partial trailing line is carried in t.buf to the next poll.
	chunk, err := io.ReadAll(io.LimitReader(t.file, maxTailChunkBytes))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", t.path, err)
	}
	if len(chunk) == 0 {
		return nil, nil
	}

	t.buf = append(t.buf, chunk...)
	t.state.Offset += int64(len(chunk))

	lines, remainder := splitLines(t.buf)
	t.buf = remainder

	if err := t.store.SaveTailState(t.path, t.state); err != nil {
		return nil, fmt.Errorf("saving tail state for %s: %w", t.path, err)
	}

	return lines, nil
}

// Offset returns the byte position the tailer has consumed up to (persisted
// across restarts). The backfill reads [0, Offset()) so it never overlaps
// with the forward counter.
func (t *Tailer) Offset() int64 {
	return t.state.Offset
}

// Inode returns the inode of the file the tailer last read. A mismatch
// against the current file's inode indicates log rotation has occurred.
func (t *Tailer) Inode() uint64 {
	return t.state.Inode
}

func (t *Tailer) Close() error {
	if t.file == nil {
		return nil
	}
	return t.file.Close()
}

// splitLines splits b on newlines, returning every complete line (blank
// lines dropped) and any trailing bytes after the last newline -- those
// belong to a line that hasn't been fully written yet and must carry over
// to the next Poll rather than being parsed as-is.
func splitLines(b []byte) (lines []string, remainder []byte) {
	start := 0
	for i, c := range b {
		if c == '\n' {
			line := trimCR(string(b[start:i]))
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(b) {
		remainder = append([]byte{}, b[start:]...)
	}
	return lines, remainder
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}
