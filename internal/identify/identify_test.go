package identify

import "testing"

func TestResolveBattlenet_KnownPrefix(t *testing.T) {
	prefix, name, ok := ResolveBattlenet("/tpr/wow/wow-update-12345.zip")
	if !ok || prefix != "wow" || name != "World of Warcraft" {
		t.Fatalf("expected wow/World of Warcraft, got prefix=%q name=%q ok=%v", prefix, name, ok)
	}
}

func TestResolveBattlenet_UnknownPrefixFallsBackGracefully(t *testing.T) {
	prefix, name, ok := ResolveBattlenet("/tpr/futuregame/data.zip")
	if ok {
		t.Fatalf("expected ok=false for an unmapped prefix")
	}
	if prefix != "futuregame" {
		t.Fatalf("expected the raw prefix to still be returned for the Unidentified bucket, got %q", prefix)
	}
	if name != "" {
		t.Fatalf("expected no name for an unmapped prefix, got %q", name)
	}
}

func TestResolveBattlenet_NotAnNGDPPath(t *testing.T) {
	if _, _, ok := ResolveBattlenet("/some/other/path"); ok {
		t.Fatalf("expected ok=false for a non-NGDP path")
	}
}

func TestUnidentifiedKey(t *testing.T) {
	if got := UnidentifiedKey("epic"); got != "epic:unidentified" {
		t.Fatalf("expected epic:unidentified, got %q", got)
	}
}

func TestSteamDepotURI_Chunk(t *testing.T) {
	id, ok := SteamDepotURI("/depot/730/chunk/aabbccdd")
	if !ok || id != 730 {
		t.Fatalf("expected depot 730, got %d ok=%v", id, ok)
	}
}

func TestSteamDepotURI_Manifest(t *testing.T) {
	id, ok := SteamDepotURI("/depot/440/manifest/123456789/5")
	if !ok || id != 440 {
		t.Fatalf("expected depot 440, got %d ok=%v", id, ok)
	}
}

func TestSteamDepotURI_NotADepotPath(t *testing.T) {
	if _, ok := SteamDepotURI("/some/other/path"); ok {
		t.Fatalf("expected ok=false for a non-depot path")
	}
}
