package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// errNoStoreData means the Storefront positively reported no data for the app
// (success:false -- delisted or region-locked). It is permanent, so the caller
// caches a .miss; any other error is treated as transient and retried.
var errNoStoreData = errors.New("storefront: no data for app")

// steamStoreView is the subset of the Steam Storefront appdetails response we
// render on the game detail page. It is cached to disk next to poster art so a
// detail view costs at most one Storefront call per app, ever.
type steamStoreView struct {
	Name             string   `json:"name"`
	ShortDescription string   `json:"short_description"`
	HeaderImage      string   `json:"header_image"`
	ReleaseDate      string   `json:"release_date"`
	Developers       []string `json:"developers"`
	Publishers       []string `json:"publishers"`
	Genres           []string `json:"genres"`
	MetacriticScore  int      `json:"metacritic_score,omitempty"`
	Website          string   `json:"website,omitempty"`
}

// storeEnvelope mirrors the Storefront shape: {"<appid>":{"success":..,"data":..}}.
type storeEnvelope struct {
	Success bool `json:"success"`
	Data    struct {
		Name             string `json:"name"`
		ShortDescription string `json:"short_description"`
		HeaderImage      string `json:"header_image"`
		Website          string `json:"website"`
		ReleaseDate      struct {
			Date string `json:"date"`
		} `json:"release_date"`
		Developers []string `json:"developers"`
		Publishers []string `json:"publishers"`
		Genres     []struct {
			Description string `json:"description"`
		} `json:"genres"`
		Metacritic struct {
			Score int `json:"score"`
		} `json:"metacritic"`
	} `json:"data"`
}

// gameStoreDetails returns Storefront detail for a Steam appID, served from the
// on-disk cache when present and otherwise fetched once and cached. ok=false
// means no detail is available (art disabled, delisted/region-locked app, or a
// fetch error) -- the caller renders the detail page from catalog data alone.
func (s *Server) gameStoreDetails(appID string) (steamStoreView, bool) {
	if s.posterDir == "" || !posterAppID.MatchString(appID) {
		return steamStoreView{}, false
	}
	dir := filepath.Join(s.posterDir, "steam")
	jsonPath := filepath.Join(dir, appID+".appdetails.json")
	miss := filepath.Join(dir, appID+".appdetails.miss")

	if b, err := os.ReadFile(jsonPath); err == nil {
		var v steamStoreView
		if json.Unmarshal(b, &v) == nil {
			return v, true
		}
	}
	if _, err := os.Stat(miss); err == nil {
		return steamStoreView{}, false // known detail-less -- don't re-hit the Storefront
	}

	v, err := s.fetchStoreDetails(appID)
	if err != nil {
		// Only cache a permanent miss when the Storefront positively said
		// "no data" -- a transient network error or 429 must be retried on a
		// later view, not frozen as detail-less.
		if errors.Is(err, errNoStoreData) {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(miss, nil, 0o644)
		}
		return steamStoreView{}, false
	}
	if b, err := json.Marshal(v); err == nil {
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(jsonPath, b, 0o644)
	}
	return v, true
}

func (s *Server) fetchStoreDetails(appID string) (steamStoreView, error) {
	url := "https://store.steampowered.com/api/appdetails?appids=" + appID + "&l=english"
	resp, err := s.httpClient.Get(url)
	if err != nil {
		return steamStoreView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return steamStoreView{}, fmt.Errorf("storefront status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return steamStoreView{}, err
	}
	return parseStoreDetails(body, appID)
}

// parseStoreDetails turns a raw Storefront appdetails body into our view. Pure
// (no I/O) so it can be unit-tested on a captured sample.
func parseStoreDetails(body []byte, appID string) (steamStoreView, error) {
	var env map[string]storeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return steamStoreView{}, err
	}
	e, ok := env[appID]
	if !ok || !e.Success {
		return steamStoreView{}, errNoStoreData
	}
	v := steamStoreView{
		Name: e.Data.Name,
		// Storefront returns HTML-entity-encoded text (e.g. &quot;). Decode it
		// here so html/template re-escapes it correctly instead of showing a
		// literal &quot;.
		ShortDescription: html.UnescapeString(e.Data.ShortDescription),
		HeaderImage:      e.Data.HeaderImage,
		ReleaseDate:      e.Data.ReleaseDate.Date,
		Developers:       e.Data.Developers,
		Publishers:       e.Data.Publishers,
		Website:          e.Data.Website,
		MetacriticScore:  e.Data.Metacritic.Score,
	}
	for _, g := range e.Data.Genres {
		v.Genres = append(v.Genres, g.Description)
	}
	return v, nil
}
