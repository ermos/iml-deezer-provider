package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// gwLightURL is Deezer's internal gw-light API used by the web player. With an
// account ARL it authenticates the session and serves catalog METADATA only
// (search, track data) at higher quotas than the per-IP public API. This client
// never requests or decrypts audio — there is no download path here.
const gwLightURL = "https://www.deezer.com/ajax/gw-light.php"

// gwClient is an ARL-authenticated, metadata-only Deezer client.
type gwClient struct {
	http *http.Client
	arl  string

	mu       sync.Mutex
	apiToken string
	tokenAt  time.Time
}

func newGWClient(arl string) *gwClient {
	// The public-suffix list lets the jar correctly handle Domain=.deezer.com
	// cookies (the session sid Deezer sets during auth, to which the api_token is
	// bound).
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if u, err := url.Parse(gwLightURL); err == nil {
		jar.SetCookies(u, []*http.Cookie{{Name: "arl", Value: arl}})
	}
	return &gwClient{
		http: &http.Client{Timeout: 20 * time.Second, Jar: jar},
		arl:  arl,
	}
}

type gwEnvelope struct {
	Error   json.RawMessage `json:"error"`
	Results json.RawMessage `json:"results"`
}

func (c *gwClient) call(ctx context.Context, method string, payload any, out any) error {
	token, err := c.ensureToken(ctx, method)
	if err != nil {
		return err
	}
	err = c.rawCall(ctx, method, token, payload, out)
	// The session/CSRF token can expire; refresh once and retry.
	if err != nil && strings.Contains(err.Error(), "VALID_TOKEN_REQUIRED") {
		c.invalidateToken()
		token, err2 := c.ensureToken(ctx, method)
		if err2 != nil {
			return err2
		}
		return c.rawCall(ctx, method, token, payload, out)
	}
	return err
}

func (c *gwClient) invalidateToken() {
	c.mu.Lock()
	c.apiToken = ""
	c.mu.Unlock()
}

func (c *gwClient) rawCall(ctx context.Context, method, token string, payload any, out any) error {
	body := []byte("{}")
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	u := gwLightURL + "?method=" + method + "&input=3&api_version=1.0&api_token=" + token
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// The arl and session (sid) cookies are carried by the client's cookie jar.
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("deezer gw: status %d", resp.StatusCode)
	}
	var env gwEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if gwHasError(env.Error) {
		return fmt.Errorf("deezer gw: %s", strings.TrimSpace(string(env.Error)))
	}
	if out != nil && len(env.Results) > 0 {
		return json.Unmarshal(env.Results, out)
	}
	return nil
}

// gwHasError reports whether the gw-light "error" field carries an actual error
// (it is an empty array [] on success).
func gwHasError(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "[]" && s != "{}" && s != "null"
}

// ensureToken fetches and caches the API token via deezer.getUserData, which
// also validates the ARL session.
func (c *gwClient) ensureToken(ctx context.Context, method string) (string, error) {
	if method == "deezer.getUserData" {
		return "", nil // bootstrap call uses an empty token
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apiToken != "" && time.Since(c.tokenAt) < 30*time.Minute {
		return c.apiToken, nil
	}
	var ud struct {
		CheckForm string `json:"checkForm"`
		User      struct {
			UserID json.Number `json:"USER_ID"`
		} `json:"USER"`
	}
	if err := c.rawCall(ctx, "deezer.getUserData", "", nil, &ud); err != nil {
		return "", err
	}
	if ud.CheckForm == "" || ud.User.UserID.String() == "" || ud.User.UserID.String() == "0" {
		return "", fmt.Errorf("deezer gw: invalid or expired ARL")
	}
	c.apiToken = ud.CheckForm
	c.tokenAt = time.Now()
	return c.apiToken, nil
}

// gwTrack is the subset of a gw-light track we consume.
type gwTrack struct {
	SngID       json.Number `json:"SNG_ID"`
	Title       string      `json:"SNG_TITLE"`
	ArtistName  string      `json:"ART_NAME"`
	ArtistID    json.Number `json:"ART_ID"`
	ArtPicture  string      `json:"ART_PICTURE"`
	AlbumTitle  string      `json:"ALB_TITLE"`
	AlbPicture  string      `json:"ALB_PICTURE"`
	Duration    json.Number `json:"DURATION"`
	TrackNumber json.Number `json:"TRACK_NUMBER"`
	DiskNumber  json.Number `json:"DISK_NUMBER"`
}

func (t gwTrack) toTrack() Track {
	return Track{
		ProviderTrackID:  t.SngID.String(),
		Title:            t.Title,
		Artist:           t.ArtistName,
		Album:            t.AlbumTitle,
		AlbumArtist:      t.ArtistName,
		TrackNo:          atoi(t.TrackNumber),
		DiscNo:           atoi(t.DiskNumber),
		Duration:         atoi(t.Duration),
		ProviderArtistID: t.ArtistID.String(),
		CoverImageURL:    deezerImageURL("cover", t.AlbPicture),
		ArtistImageURL:   deezerImageURL("artist", t.ArtPicture),
	}
}

// deezerImageURL builds a Deezer public CDN image URL from a picture hash.
func deezerImageURL(kind, hash string) string {
	if hash == "" {
		return ""
	}
	return "https://e-cdns-images.dzcdn.net/images/" + kind + "/" + hash + "/500x500-000000-80-0-0.jpg"
}

// Search implements catalog via the gw-light search endpoint.
func (c *gwClient) Search(ctx context.Context, query string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 20
	}
	payload := map[string]any{"query": query, "start": 0, "nb": limit, "top_tracks": true}
	var res struct {
		Track struct {
			Data []gwTrack `json:"data"`
		} `json:"TRACK"`
	}
	if err := c.call(ctx, "deezer.pageSearch", payload, &res); err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(res.Track.Data))
	for _, t := range res.Track.Data {
		if t.SngID.String() == "" {
			continue
		}
		out = append(out, t.toTrack())
	}
	return out, nil
}

// Resolve implements catalog via song.getListData.
func (c *gwClient) Resolve(ctx context.Context, id string) (Track, error) {
	payload := map[string]any{"sng_ids": []string{id}}
	var res struct {
		Data []gwTrack `json:"data"`
	}
	if err := c.call(ctx, "song.getListData", payload, &res); err != nil {
		return Track{}, err
	}
	if len(res.Data) == 0 || res.Data[0].SngID.String() == "" {
		return Track{}, fmt.Errorf("track %q not found", id)
	}
	return res.Data[0].toTrack(), nil
}

type gwArtist struct {
	ArtID   json.Number `json:"ART_ID"`
	Name    string      `json:"ART_NAME"`
	Picture string      `json:"ART_PICTURE"`
	NbAlbum json.Number `json:"NB_ALBUM"`
}

type gwAlbum struct {
	AlbID   json.Number `json:"ALB_ID"`
	Title   string      `json:"ALB_TITLE"`
	Picture string      `json:"ALB_PICTURE"`
	Release string      `json:"PHYSICAL_RELEASE_DATE"`
	Digital string      `json:"DIGITAL_RELEASE_DATE"`
}

// SearchArtists reads the ARTIST section of the search (album count, id, image).
func (c *gwClient) SearchArtists(ctx context.Context, query string, limit int) ([]Artist, error) {
	if limit <= 0 {
		limit = 20
	}
	payload := map[string]any{"query": query, "start": 0, "nb": limit, "artist_suggest": true}
	var res struct {
		Artist struct {
			Data []gwArtist `json:"data"`
		} `json:"ARTIST"`
	}
	if err := c.call(ctx, "deezer.pageSearch", payload, &res); err != nil {
		return nil, err
	}
	out := make([]Artist, 0, len(res.Artist.Data))
	for _, a := range res.Artist.Data {
		if a.Name == "" {
			continue
		}
		out = append(out, Artist{
			ProviderArtistID: a.ArtID.String(),
			Name:             a.Name,
			AlbumCount:       atoi(a.NbAlbum),
			ImageURL:         deezerImageURL("artist", a.Picture),
		})
	}
	return out, nil
}

// ArtistAlbums lists an artist's discography (the ALBUMS section of pageArtist).
func (c *gwClient) ArtistAlbums(ctx context.Context, artistID string, limit int) ([]Album, error) {
	if limit <= 0 {
		limit = 100
	}
	payload := map[string]any{"art_id": artistID, "lang": "en", "tab": 0, "nb": limit, "start": 0, "header": true}
	var res struct {
		Albums struct {
			Data []gwAlbum `json:"data"`
		} `json:"ALBUMS"`
	}
	if err := c.call(ctx, "deezer.pageArtist", payload, &res); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(res.Albums.Data))
	for _, a := range res.Albums.Data {
		if a.AlbID.String() == "" {
			continue
		}
		year := 0
		for _, d := range []string{a.Digital, a.Release} {
			if len(d) >= 4 {
				if y := atoiStr(d[:4]); y > 0 {
					year = y
					break
				}
			}
		}
		out = append(out, Album{
			ProviderAlbumID: a.AlbID.String(),
			Title:           a.Title,
			Year:            year,
			CoverImageURL:   deezerImageURL("cover", a.Picture),
		})
	}
	return out, nil
}

// ArtistTracks returns an artist's top tracks (TOP section of pageArtist).
func (c *gwClient) ArtistTracks(ctx context.Context, artistID string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 50
	}
	payload := map[string]any{"art_id": artistID, "lang": "en", "tab": 0, "nb": limit, "header": true}
	var res struct {
		Top struct {
			Data []gwTrack `json:"data"`
		} `json:"TOP"`
	}
	if err := c.call(ctx, "deezer.pageArtist", payload, &res); err != nil {
		return nil, err
	}
	return gwTracks(res.Top.Data), nil
}

// AlbumTracks lists an album's tracks via song.getListByAlbum.
func (c *gwClient) AlbumTracks(ctx context.Context, albumID string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 200
	}
	payload := map[string]any{"alb_id": albumID, "nb": limit, "start": 0}
	var res struct {
		Data []gwTrack `json:"data"`
	}
	if err := c.call(ctx, "song.getListByAlbum", payload, &res); err != nil {
		return nil, err
	}
	return gwTracks(res.Data), nil
}

func gwTracks(in []gwTrack) []Track {
	out := make([]Track, 0, len(in))
	for _, t := range in {
		if t.SngID.String() == "" {
			continue
		}
		out = append(out, t.toTrack())
	}
	return out
}
