package web

// The JSON API under /api/v1/ exposes the same library operations the webui
// offers, for scripts and integrations (monitoring, automation, third-party
// clients). Authenticated by a static key (BUFFARR_API_KEY) checked in
// authGate; disabled entirely when the key is unset. Versioned so these
// response shapes can stay frozen: breaking changes go to /api/v2/, not here.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/lifecycle"
)

const apiPrefix = "/api/"

// checkAPIKey validates the static key on an /api/ request. Returns a
// non-zero HTTP status and message when the request must be rejected. The
// key rides in X-Api-Key or Authorization: Bearer -- both common enough
// that integrations expect one or the other.
func (s *Server) checkAPIKey(r *http.Request) (int, string) {
	if s.apiKey == "" {
		return http.StatusForbidden, "API disabled: set BUFFARR_API_KEY to enable it"
	}
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
		return http.StatusUnauthorized, "invalid or missing API key"
	}
	return 0, ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: encoding response: %v", err)
	}
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// apiGame is the frozen v1 wire shape of a library entry.
type apiGame struct {
	Platform      string    `json:"platform"`
	GameKey       string    `json:"game_key"`
	Name          string    `json:"name"`
	Locked        bool      `json:"locked"`
	SizeBytes     int64     `json:"size_bytes"`
	CacheKeyCount int       `json:"cache_key_count"`
	LastSeen      time.Time `json:"last_seen"`
}

func toAPIGame(e catalog.LibraryEntry) apiGame {
	return apiGame{
		Platform:      e.Platform,
		GameKey:       e.GameKey,
		Name:          e.Name,
		Locked:        e.Monitored,
		SizeBytes:     e.SizeBytes,
		CacheKeyCount: e.CacheKeyCount,
		LastSeen:      e.LastSeen,
	}
}

// apiListGames returns every cached, identified game above the operator's
// min-game-size floor -- the same set the Library page shows.
func (s *Server) apiListGames(w http.ResponseWriter, r *http.Request) {
	entries, err := s.cat.List(s.minGameSizeBytes())
	if err != nil {
		log.Printf("api: listing games: %v", err)
		apiError(w, http.StatusInternalServerError, "listing games failed")
		return
	}
	games := make([]apiGame, len(entries))
	for i, e := range entries {
		games[i] = toAPIGame(e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"games": games})
}

func (s *Server) apiGetGame(w http.ResponseWriter, r *http.Request) {
	entry, found, err := s.cat.GetGame(r.PathValue("platform"), r.PathValue("gameKey"))
	if err != nil {
		log.Printf("api: getting game: %v", err)
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !found {
		apiError(w, http.StatusNotFound, "game not found")
		return
	}
	writeJSON(w, http.StatusOK, toAPIGame(entry))
}

// apiSetLock locks (protects from eviction/deletion) or unlocks a game and
// returns its updated state.
func (s *Server) apiSetLock(lock bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, gameKey := r.PathValue("platform"), r.PathValue("gameKey")
		_, found, err := s.cat.GetGame(platform, gameKey)
		if err != nil {
			log.Printf("api: getting game for lock: %v", err)
			apiError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if !found {
			apiError(w, http.StatusNotFound, "game not found")
			return
		}
		if lock {
			err = lifecycle.Monitor(s.cat, platform, gameKey)
		} else {
			err = lifecycle.Unmonitor(s.cat, platform, gameKey)
		}
		if err != nil {
			log.Printf("api: lock=%v (%s, %s): %v", lock, platform, gameKey, err)
			apiError(w, http.StatusInternalServerError, "updating lock failed")
			return
		}
		entry, _, err := s.cat.GetGame(platform, gameKey)
		if err != nil {
			log.Printf("api: re-reading game after lock: %v", err)
			apiError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, toAPIGame(entry))
	}
}

// apiDeleteGame mirrors handleDelete: optimistic catalog clear with the slow
// PURGE handed to the background queue, idempotent on an already-gone game,
// and a hard refusal on a locked one.
func (s *Server) apiDeleteGame(w http.ResponseWriter, r *http.Request) {
	platform, gameKey := r.PathValue("platform"), r.PathValue("gameKey")
	prefixes, err := lifecycle.Delete(s.cat, platform, gameKey, s.lookupAppDepots)
	switch {
	case err == nil, errors.Is(err, lifecycle.ErrGameNotFound):
		s.purgeQueue.Enqueue(prefixes)
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	case errors.Is(err, lifecycle.ErrMonitored):
		apiError(w, http.StatusConflict, "game is locked; unlock it before deleting")
	default:
		log.Printf("api: deleting (%s, %s): %v", platform, gameKey, err)
		apiError(w, http.StatusInternalServerError, "delete failed; no files were removed")
	}
}

// apiStatsResponse is the frozen v1 wire shape of /api/v1/stats. Zero-valued
// disk fields mean the cache volume could not be inspected.
type apiStatsResponse struct {
	DiskTotalBytes         int64  `json:"disk_total_bytes"`
	DiskFreeBytes          int64  `json:"disk_free_bytes"`
	DiskUsedBytes          int64  `json:"disk_used_bytes"`
	CacheCapBytes          int64  `json:"cache_cap_bytes"` // 0 = no cap configured
	LockedBytes            int64  `json:"locked_bytes"`
	CachedGames            int    `json:"cached_games"`
	BandwidthSavedBytes    int64  `json:"bandwidth_saved_bytes"`
	BandwidthUpstreamBytes int64  `json:"bandwidth_upstream_bytes"`
	HitRatePct             int    `json:"hit_rate_pct"`
	BandwidthSince         string `json:"bandwidth_since,omitempty"`
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	var resp apiStatsResponse
	if s.cacheRoot != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(s.cacheRoot, &st); err == nil {
			resp.DiskTotalBytes = int64(st.Blocks) * st.Bsize
			resp.DiskFreeBytes = int64(st.Bavail) * st.Bsize
			resp.DiskUsedBytes = resp.DiskTotalBytes - resp.DiskFreeBytes
		}
	}
	resp.CacheCapBytes = s.maxCacheSizeBytes()
	if n, err := s.cat.CachedGameCount(s.minGameSizeBytes()); err == nil {
		resp.CachedGames = n
	}
	if n, err := s.cat.LockedBytes(s.minGameSizeBytes()); err == nil {
		resp.LockedBytes = n
	}
	if totalBytes, hitBytes, totalReqs, hitReqs, err := s.cat.BandwidthTotals(""); err == nil && totalReqs > 0 {
		resp.BandwidthSavedBytes = hitBytes
		resp.BandwidthUpstreamBytes = totalBytes - hitBytes
		resp.HitRatePct = int(hitReqs * 100 / totalReqs)
		if since, err := s.cat.BandwidthSince(); err == nil {
			resp.BandwidthSince = since
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// apiQueueItem is the frozen v1 wire shape of a prefill queue row.
type apiQueueItem struct {
	ID       int64  `json:"id"`
	Position int    `json:"position"`
	Platform string `json:"platform"`
	AppID    string `json:"app_id"`
	Name     string `json:"name"`
	Status   string `json:"status"` // queued | active | failed
	Error    string `json:"error,omitempty"`
}

func (s *Server) apiPrefillStatus(w http.ResponseWriter, r *http.Request) {
	queue, err := s.cat.ListPrefillQueue()
	if err != nil {
		log.Printf("api: listing prefill queue: %v", err)
		apiError(w, http.StatusInternalServerError, "listing queue failed")
		return
	}
	items := make([]apiQueueItem, len(queue))
	for i, q := range queue {
		items[i] = apiQueueItem{ID: q.ID, Position: q.Position, Platform: q.Platform, AppID: q.AppID, Name: q.Name, Status: q.Status, Error: q.Error}
	}
	running, err := s.sidecar.IsRunning()
	if err != nil {
		log.Printf("api: checking prefill running state: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": running, "queue": items})
}

// apiPrefillTrigger queues owned Steam games for preload, mirroring
// handlePrefillTrigger's ownership gate: un-owned or unknown app IDs are
// skipped and reported back rather than failing the whole request.
func (s *Server) apiPrefillTrigger(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppIDs []int `json:"app_ids"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // a list of app IDs; 1 MiB is generous
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "malformed JSON body; expected {\"app_ids\": [730, ...]}")
		return
	}
	if len(req.AppIDs) == 0 {
		apiError(w, http.StatusBadRequest, "app_ids is empty")
		return
	}

	webAPIKey, operatorSteamID64, _ := s.steamConfig()
	games, err := s.fetchOwnedGames(webAPIKey, operatorSteamID64)
	if err != nil {
		log.Printf("api: fetching owned games for prefill trigger: %v", err)
		apiError(w, http.StatusBadGateway, "could not verify ownership; Steam's Web API is unreachable")
		return
	}
	names := make(map[int]string, len(games))
	for _, g := range games {
		names[g.AppID] = g.Name
	}

	var queued, already int
	skipped := []int{} // [] not null on the wire when nothing was skipped
	for _, appID := range req.AppIDs {
		name, owned := names[appID]
		if !owned {
			skipped = append(skipped, appID)
			continue
		}
		added, eErr := s.cat.EnqueuePrefill("steam", strconv.Itoa(appID), name)
		if eErr != nil {
			log.Printf("api: enqueueing prefill for app %d: %v", appID, eErr)
			skipped = append(skipped, appID)
			continue
		}
		if added {
			queued++
		} else {
			already++
		}
	}
	if queued > 0 {
		s.wakePrefillWorker()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"queued":         queued,
		"already_queued": already,
		"skipped":        skipped,
	})
}
