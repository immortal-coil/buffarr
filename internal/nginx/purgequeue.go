package nginx

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// PurgeQueue runs cache purges off the request path on a single background
// worker.
//
// Why this exists: directly deleting cache files is what makes delete appear
// to "do nothing" when done synchronously -- the old nginx PURGE-API approach
// scanned the entire zone (O(cache size), ~308s on the live box) per prefix.
// Delete is now optimistic: the catalog row is cleared immediately (the
// operator sees the row vanish) and the actual file deletion is handed here
// to complete in the background.
//
// A single worker serializes purges deliberately. Filesystem walks contend on
// the same disk I/O; running two walks concurrently against the same directory
// tree achieves nothing and doubles the I/O. FIFO with one worker keeps disk
// pressure bounded and makes "is the cache still draining" a single,
// observable queue.
//
// Batch drain: the worker drains the entire pending queue into one snapshot
// before each walk. This means one filesystem scan covers all prefixes
// accumulated since the last walk, rather than a separate scan per prefix.
//
// Pending-prefix dedup: if the same depot prefix is enqueued twice before the
// worker drains (operator double-clicks, or two games share a depot), it is
// scanned for once. A prefix in-flight is NOT deduped against a freshly
// enqueued copy -- by the time the next walk runs, new bytes may have landed,
// so re-scanning is correct, not wasteful.
//
// Failure handling and the optimistic-delete tradeoff: a failed walk is logged
// and the queue moves on. The cost is the known, accepted tradeoff: because
// the catalog row was already cleared optimistically, a failed walk leaves
// bytes on disk that the catalog reports as gone. Those bytes are eventually
// reclaimed by nginx's own inactive eviction, but are not operator-retriable
// from the UI in the meantime. The loud log is the signal that this happened.
type PurgeQueue struct {
	cacheRoot string
	statePath string // "" = in-memory only

	mu       sync.Mutex
	cond     *sync.Cond
	pending  []string
	inflight []string // the batch the worker is currently walking
	working  bool     // true while the worker is mid-walk (drained but not yet done)
}

// NewPurgeQueue creates a PurgeQueue and starts its background worker. The
// worker lives for the process's lifetime; there is no Close because the
// backend never tears the queue down independently of shutdown.
//
// statePath, when set, persists not-yet-finished prefixes (pending plus the
// in-flight batch) and reloads them here. Without it, a restart mid-purge
// would lose the walk while the catalog rows are already cleared: the bytes
// stay on disk uncatalogued (reconcile only counts them), Statfs stays high,
// and the cache-max enforcer evicts further games to compensate.
func NewPurgeQueue(cacheRoot, statePath string) *PurgeQueue {
	q := &PurgeQueue{cacheRoot: cacheRoot, statePath: statePath}
	q.cond = sync.NewCond(&q.mu)
	if statePath != "" {
		if b, err := os.ReadFile(statePath); err == nil {
			if err := json.Unmarshal(b, &q.pending); err != nil {
				log.Printf("nginx: ignoring unreadable purge state %s: %v", statePath, err)
			} else if len(q.pending) > 0 {
				log.Printf("nginx: resuming %d purge prefixes left from a previous run", len(q.pending))
			}
		}
	}
	go q.run()
	return q
}

// saveLocked writes inflight+pending to statePath (temp file + rename). Best
// effort: a failed write only loses crash-resume, never the purge itself.
func (q *PurgeQueue) saveLocked() {
	if q.statePath == "" {
		return
	}
	b, _ := json.Marshal(append(append([]string{}, q.inflight...), q.pending...))
	tmp := q.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("nginx: saving purge state: %v", err)
		return
	}
	if err := os.Rename(tmp, q.statePath); err != nil {
		log.Printf("nginx: saving purge state: %v", err)
	}
}

// Enqueue schedules each prefix for background eviction. Prefixes already
// waiting in the queue are not added twice. Enqueue never blocks on the
// purge itself -- it appends and returns, so the delete handler stays fast.
func (q *PurgeQueue) Enqueue(prefixes []string) {
	if len(prefixes) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range prefixes {
		if q.isPendingLocked(p) {
			continue
		}
		q.pending = append(q.pending, p)
	}
	q.saveLocked()
	q.cond.Signal()
}

func (q *PurgeQueue) isPendingLocked(prefix string) bool {
	for _, p := range q.pending {
		if p == prefix {
			return true
		}
	}
	return false
}

func (q *PurgeQueue) run() {
	for {
		q.mu.Lock()
		for len(q.pending) == 0 {
			q.cond.Wait()
		}
		// Drain all pending prefixes in one snapshot so a single walk
		// covers the whole batch rather than one walk per prefix.
		prefixes := make([]string, len(q.pending))
		copy(prefixes, q.pending)
		q.pending = q.pending[:0]
		q.inflight = prefixes // still persisted until the walk finishes
		q.working = true      // stays busy across the unlocked walk below (Idle)
		q.mu.Unlock()

		deleted, err := WalkPurge(q.cacheRoot, prefixes)
		if err != nil {
			log.Printf("nginx: purge walk failed (%d prefixes): %v", len(prefixes), err)
		} else if deleted > 0 {
			log.Printf("nginx: purge walk deleted %d files (%d prefixes)", deleted, len(prefixes))
		}

		q.mu.Lock()
		q.working = false
		q.inflight = nil
		q.saveLocked()
		q.mu.Unlock()
	}
}

// Idle reports whether the queue has nothing pending and no walk in flight.
// The cache enforcer skips its tick while the queue is non-idle so it doesn't
// over-evict before an in-progress purge has actually freed disk.
func (q *PurgeQueue) Idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) == 0 && !q.working
}
