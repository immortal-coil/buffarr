package web

import (
	"errors"
	"strings"
	"testing"
)

// sampleAppDetails mirrors the real store.steampowered.com/api/appdetails shape
// (confirmed by curl for app 620), including the HTML-entity-encoded
// short_description the Storefront returns.
const sampleAppDetails = `{"620":{"success":true,"data":{
  "name":"Portal 2",
  "short_description":"The &quot;Perpetual Testing Initiative&quot; has been expanded.",
  "header_image":"https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/620/header.jpg",
  "website":"http://www.thinkwithportals.com/",
  "release_date":{"coming_soon":false,"date":"Apr 18, 2011"},
  "developers":["Valve"],
  "publishers":["Valve"],
  "genres":[{"id":"1","description":"Action"},{"id":"25","description":"Adventure"}],
  "metacritic":{"score":95,"url":"https://www.metacritic.com/game/pc/portal-2"}
}}}`

func TestParseStoreDetails(t *testing.T) {
	v, err := parseStoreDetails([]byte(sampleAppDetails), "620")
	if err != nil {
		t.Fatalf("parseStoreDetails: %v", err)
	}
	if v.Name != "Portal 2" {
		t.Errorf("Name = %q, want Portal 2", v.Name)
	}
	// HTML entities must be decoded (so html/template re-escapes correctly,
	// not double-escaped into a literal &quot;).
	if !strings.Contains(v.ShortDescription, `"Perpetual Testing Initiative"`) {
		t.Errorf("ShortDescription not entity-decoded: %q", v.ShortDescription)
	}
	if v.ReleaseDate != "Apr 18, 2011" {
		t.Errorf("ReleaseDate = %q, want Apr 18, 2011", v.ReleaseDate)
	}
	if len(v.Developers) != 1 || v.Developers[0] != "Valve" {
		t.Errorf("Developers = %v, want [Valve]", v.Developers)
	}
	if strings.Join(v.Genres, ",") != "Action,Adventure" {
		t.Errorf("Genres = %v, want [Action Adventure]", v.Genres)
	}
	if v.MetacriticScore != 95 {
		t.Errorf("MetacriticScore = %d, want 95", v.MetacriticScore)
	}
	if v.Website == "" {
		t.Errorf("Website empty, want set")
	}
}

// A delisted/region-locked app returns success:false, which must surface as
// errNoStoreData so the caller caches a permanent .miss (vs a transient error,
// which is retried).
func TestParseStoreDetails_SuccessFalseIsNoData(t *testing.T) {
	_, err := parseStoreDetails([]byte(`{"999":{"success":false}}`), "999")
	if !errors.Is(err, errNoStoreData) {
		t.Fatalf("expected errNoStoreData for success:false, got %v", err)
	}
}
