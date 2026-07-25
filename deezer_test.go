package main

import (
	"encoding/json"
	"testing"
)

func TestGWTrackToTrack(t *testing.T) {
	raw := `{
		"SNG_ID": "123", "SNG_TITLE": "Song", "DURATION": "210",
		"TRACK_NUMBER": "4", "DISK_NUMBER": "1",
		"ART_NAME": "A", "ART_ID": "7", "ART_PICTURE": "artHash",
		"ALB_TITLE": "Al", "ALB_PICTURE": "covHash", "ISRC": "FRLA61300001"
	}`
	var gt gwTrack
	if err := json.Unmarshal([]byte(raw), &gt); err != nil {
		t.Fatal(err)
	}
	got := gt.toTrack()
	want := Track{
		ProviderTrackID: "123", Title: "Song", Artist: "A", Album: "Al", AlbumArtist: "A",
		TrackNo: 4, DiscNo: 1, Duration: 210, ProviderArtistID: "7",
		CoverImageURL:  "https://e-cdns-images.dzcdn.net/images/cover/covHash/500x500-000000-80-0-0.jpg",
		ArtistImageURL: "https://e-cdns-images.dzcdn.net/images/artist/artHash/500x500-000000-80-0-0.jpg",
		ISRC:           "FRLA61300001",
	}
	if got != want {
		t.Fatalf("gw mapping mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestGWHasError(t *testing.T) {
	for _, ok := range []string{"[]", "{}", "null", "", "  "} {
		if gwHasError([]byte(ok)) {
			t.Fatalf("%q should not be an error", ok)
		}
	}
	if !gwHasError([]byte(`{"VALID_TOKEN_REQUIRED":"Invalid CSRF token"}`)) {
		t.Fatal("real error not detected")
	}
}
