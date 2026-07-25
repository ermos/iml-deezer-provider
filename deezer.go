package main

import (
	"encoding/json"
	"strconv"
)

// This service is ARL-authenticated (gw-light) and metadata-only — see gw.go.
// The shapes below are the wire contract consumed by the generic HTTP
// provider (httpprovider).

// Track is the wire shape consumed by the provider's /search, /resolve,
// /artist/tracks and /album/tracks.
type Track struct {
	ProviderTrackID  string `json:"providerTrackId"`
	Title            string `json:"title"`
	Artist           string `json:"artist"`
	Album            string `json:"album"`
	AlbumArtist      string `json:"albumArtist"`
	TrackNo          int    `json:"trackNo,omitempty"`
	DiscNo           int    `json:"discNo,omitempty"`
	Year             int    `json:"year,omitempty"`
	Duration         int    `json:"duration,omitempty"`
	ProviderArtistID string `json:"providerArtistId,omitempty"`
	CoverImageURL    string `json:"coverImageUrl,omitempty"`
	ArtistImageURL   string `json:"artistImageUrl,omitempty"`
	ISRC             string `json:"isrc,omitempty"`
	// No Suffix: metadata is browse-only; audio comes from /download.
}

// Artist is the wire shape consumed by the provider's /artists.
type Artist struct {
	ProviderArtistID string `json:"providerArtistId"`
	Name             string `json:"name"`
	AlbumCount       int    `json:"albumCount,omitempty"`
	ImageURL         string `json:"imageUrl,omitempty"`
}

// Album is the wire shape consumed by the provider's /artist/albums.
type Album struct {
	ProviderAlbumID string `json:"providerAlbumId"`
	Title           string `json:"title"`
	Year            int    `json:"year,omitempty"`
	CoverImageURL   string `json:"coverImageUrl,omitempty"`
}

func atoi(n json.Number) int {
	v, _ := strconv.Atoi(n.String())
	return v
}

func atoiStr(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
