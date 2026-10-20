package identify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var steamDepotPathRE = regexp.MustCompile(`^/depot/(\d+)/`)

// SteamDepotURI extracts the depot ID from a Steam CDN request path.
// Confirmed against SteamKit2's CDN.Client (the library steam-lancache-
// prefill and steamcmd both build on): chunk and manifest requests are both
// "/depot/{depotId}/...", so depot ID extraction doesn't need to
// distinguish the two.
func SteamDepotURI(uri string) (depotID int, ok bool) {
	m := steamDepotPathRE.FindStringSubmatch(uri)
	if m == nil {
		return 0, false
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return id, true
}

// OwnedApp is one game from the operator's Steam library.
type OwnedApp struct {
	AppID int
	Name  string
}

// SteamAPI is the external surface SteamResolver's background sync needs.
// There is deliberately no single "depot belongs to app X" endpoint on
// Steam's public Web API -- confirmed directly against the live API (a
// keyless GetAppList call now 404s "method not found", and GetOwnedGames
// is the only viable enumeration point, consistent with this project's
// "Single Steam account for v1" non-goal). GetAppDepots is satisfied by
// steamcmd.net's community-run app-info mirror (the same PICS data source
// SteamDB itself is built on), not steampowered.com.
type SteamAPI interface {
	GetOwnedGames(steamID64 string) ([]OwnedApp, error)
	// GetAppDepots returns the list of depot IDs and the sum of their
	// maxsize values (an estimate of total download size) for the given app.
	GetAppDepots(appID int) (depotIDs []int, totalBytes int64, err error)
}

// SteamResolver maintains a depot->app reverse index built by walking the
// operator's owned-games list (the only tractable enumeration point) and
// fetching each owned app's depot list. Resolve is a pure map lookup --
// the tailer (catalog package) must never block on a network call per log
// line, so all the resolver's I/O lives in Sync, not Resolve.
type SteamResolver struct {
	api         SteamAPI
	steamID64   string
	syncDelay   time.Duration // pacing between per-app GetAppDepots calls
	mu          sync.RWMutex
	depotToApp  map[int]int
	appToDepots map[int][]int
	appNames    map[int]string
	appSizes    map[int]int64 // sum of depot maxsize values per app (download estimate)
}

// NewSteamResolver returns a resolver that is safe to call Resolve on
// immediately (empty maps, every depot Unidentified) before the first Sync
// completes -- Steam resolution being unavailable or not-yet-synced must
// never block log ingestion (R3's error-path requirement).
func NewSteamResolver(api SteamAPI, steamID64 string) *SteamResolver {
	return &SteamResolver{
		api:        api,
		steamID64:  steamID64,
		syncDelay:  200 * time.Millisecond,
		depotToApp: make(map[int]int),
		appNames:   make(map[int]string),
		appSizes:   make(map[int]int64),
	}
}

// Resolve looks up depotID in the current reverse index. It never makes a
// network call.
func (r *SteamResolver) Resolve(depotID int) (appID int, name string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	appID, ok = r.depotToApp[depotID]
	if !ok {
		return 0, "", false
	}
	return appID, r.appNames[appID], true
}

// DepotsForApp returns every depot ID Steam's own API lists under appID, from
// the last Sync. Used by delete to purge the full known depot set for a game
// -- not just the depots the nginx-log tailer happened to observe requests
// for, which can undercount a real Steam client's own downloads (see
// lifecycle.Delete). Returns nil if unsynced or the app is unknown.
func (r *SteamResolver) DepotsForApp(appID int) []int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.appToDepots[appID]
}

// GetSize returns the estimated total download size (sum of depot maxsize
// values) for the given appID. Returns 0 if unknown or not yet synced.
func (r *SteamResolver) GetSize(appID int) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.appSizes[appID]
}

// Sync rebuilds the reverse index from the operator's current owned-games
// list. Individual per-app depot lookups that fail are skipped (logged),
// not fatal -- one bad app must not abort the whole sync and leave
// everything else stale.
func (r *SteamResolver) Sync() error {
	apps, err := r.api.GetOwnedGames(r.steamID64)
	if err != nil {
		return fmt.Errorf("GetOwnedGames: %w", err)
	}
	if len(apps) == 0 {
		log.Printf("identify: GetOwnedGames returned 0 games for the configured operator SteamID64 -- Steam depot resolution will report everything Unidentified. A private profile returns an empty list even with a valid key; check the profile's privacy setting if this is unexpected.")
	}

	depotToApp := make(map[int]int, len(apps)*4)
	appToDepots := make(map[int][]int, len(apps))
	appNames := make(map[int]string, len(apps))
	appSizes := make(map[int]int64, len(apps))

	for i, app := range apps {
		appNames[app.AppID] = app.Name

		depots, totalBytes, err := r.api.GetAppDepots(app.AppID)
		if err != nil {
			log.Printf("identify: skipping depot lookup for app %d (%s): %v", app.AppID, app.Name, err)
			continue
		}
		for _, depotID := range depots {
			depotToApp[depotID] = app.AppID
		}
		appToDepots[app.AppID] = depots
		if totalBytes > 0 {
			appSizes[app.AppID] = totalBytes
		}

		if i < len(apps)-1 && r.syncDelay > 0 {
			time.Sleep(r.syncDelay)
		}
	}

	r.mu.Lock()
	r.depotToApp = depotToApp
	r.appToDepots = appToDepots
	r.appNames = appNames
	r.appSizes = appSizes
	r.mu.Unlock()
	return nil
}

// HTTPSteamAPI is the real SteamAPI: GetOwnedGames against Steam's official
// Web API, GetAppDepots against steamcmd.net's app-info mirror.
type HTTPSteamAPI struct {
	WebAPIKey  string
	HTTPClient *http.Client
}

func NewHTTPSteamAPI(webAPIKey string) *HTTPSteamAPI {
	return &HTTPSteamAPI{
		WebAPIKey:  webAPIKey,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *HTTPSteamAPI) GetOwnedGames(steamID64 string) ([]OwnedApp, error) {
	const endpoint = "https://api.steampowered.com/IPlayerService/GetOwnedGames/v1/"
	resp, err := a.HTTPClient.Get(fmt.Sprintf(
		"%s?key=%s&steamid=%s&include_appinfo=1&include_played_free_games=1",
		endpoint, a.WebAPIKey, steamID64,
	))
	if err != nil {
		// net/http's *url.Error embeds the full request URL, key included, and
		// callers log these errors. Swap in the key-free endpoint.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			ue.URL = endpoint
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetOwnedGames: unexpected status %d: %s", resp.StatusCode, body)
	}

	var parsed struct {
		Response struct {
			GameCount int `json:"game_count"`
			Games     []struct {
				AppID int    `json:"appid"`
				Name  string `json:"name"`
			} `json:"games"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding GetOwnedGames response: %w", err)
	}

	apps := make([]OwnedApp, 0, len(parsed.Response.Games))
	for _, g := range parsed.Response.Games {
		apps = append(apps, OwnedApp{AppID: g.AppID, Name: g.Name})
	}
	return apps, nil
}

func (a *HTTPSteamAPI) GetAppDepots(appID int) ([]int, int64, error) {
	url := fmt.Sprintf("https://api.steamcmd.net/v1/info/%d", appID)
	resp, err := a.HTTPClient.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("steamcmd.net info: unexpected status %d: %s", resp.StatusCode, body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("reading steamcmd.net info response: %w", err)
	}
	return parseSteamcmdDepots(body, appID)
}

// parseSteamcmdDepots extracts the depot IDs and an estimated download size
// from a steamcmd.net /v1/info response. Split out from GetAppDepots so the
// (fragile, upstream-defined) JSON shape is unit-testable without a network
// call. The per-depot size lives at depots[id].manifests.public.download (the
// compressed CDN bytes the cache actually stores; .size is the larger
// uncompressed install size). The legacy flat "maxsize" field -- which
// steamcmd.net stopped populating -- is kept only as a last-resort fallback.
func parseSteamcmdDepots(body []byte, appID int) ([]int, int64, error) {
	var parsed struct {
		Data map[string]struct {
			Depots map[string]json.RawMessage `json:"depots"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, fmt.Errorf("decoding steamcmd.net info response: %w", err)
	}

	app, ok := parsed.Data[strconv.Itoa(appID)]
	if !ok {
		return nil, 0, fmt.Errorf("steamcmd.net info: no data for app %d", appID)
	}

	// The "depots" object mixes numeric depot-ID keys with non-numeric
	// metadata keys (e.g. "branches", "baselanguages") -- confirmed against
	// a live response. Only numeric keys are real depots.
	depots := make([]int, 0, len(app.Depots))
	var totalBytes int64
	for k, raw := range app.Depots {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		var info struct {
			Manifests struct {
				Public struct {
					Download string `json:"download"`
					Size     string `json:"size"`
				} `json:"public"`
			} `json:"manifests"`
			MaxSize      string `json:"maxsize"`
			DepotFromApp string `json:"depotfromapp"`
		}
		if json.Unmarshal(raw, &info) != nil {
			depots = append(depots, id)
			continue
		}
		// A depotfromapp depot is borrowed from another app (e.g. 228990, the
		// redist every Source game lists). It belongs to no single game: owning
		// it here would let deleting/evicting this game purge content a locked
		// game also uses, and would inflate this game's size estimate.
		if info.DepotFromApp != "" {
			continue
		}
		depots = append(depots, id)
		val := info.Manifests.Public.Download
		if val == "" {
			val = info.Manifests.Public.Size
		}
		if val == "" {
			val = info.MaxSize
		}
		if sz, err := strconv.ParseInt(val, 10, 64); err == nil {
			totalBytes += sz
		}
	}
	return depots, totalBytes, nil
}
