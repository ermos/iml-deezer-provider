// Command deezer-http is a standalone HTTP service backed by Deezer that
// implements the gossignol generic HTTP provider protocol. It exposes metadata
// (search, resolve) plus an audio /download endpoint that streams the decrypted
// track to the caller (passthrough, nothing written to disk). Register it in
// gossignol as a dynamic provider:
//
//	POST /admin/providers  name=deezer endpoint=http://deezer-http:8080
//
// The Deezer account ARL is supplied per-request via the X-Deezer-Arl header
// (gossignol forwards the provider's configured headers verbatim), so one service
// can serve several accounts. Configuration (environment):
//
//	ADDR           listen address (default ":8080")
//	DEEZER_QUALITY preferred quality: FLAC | MP3_320 | MP3_128 (default MP3_128).
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gossignol/deezer-http/deezer"
)

// arlHeader is the request header gossignol forwards (verbatim, on every request)
// carrying the Deezer account ARL. Set it in the provider's config "headers" blob.
const arlHeader = "X-Deezer-Arl"

// client bundles the per-ARL gw (search/metadata) and download sessions, both of
// which hold an authenticated cookie/token tied to that ARL.
type client struct {
	gw *gwClient
	dl *deezer.DownloadService
}

// registry caches one client per ARL. A request bearing an ARL we haven't seen
// is a cache miss and gets a fresh client (which bootstraps its own gw token);
// requests reusing a known ARL share its already-authenticated session.
// ponytail: unbounded map, fine for the handful of accounts a self-hosted
// instance serves; add an LRU/TTL if you ever expose it to many ARLs.
type registry struct {
	quality string
	logger  *slog.Logger

	mu    sync.Mutex
	byARL map[string]*client
}

func (r *registry) get(arl string) *client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.byARL[arl]; ok {
		return c
	}
	c := &client{
		gw: newGWClient(arl),
		dl: deezer.NewDownloadService(&http.Client{Timeout: 60 * time.Second}, deezer.Settings{
			ARL:     arl,
			Quality: r.quality,
		}, r.logger),
	}
	r.byARL[arl] = c
	return c
}

func main() {
	addr := getenv("ADDR", ":8080")

	quality := os.Getenv("DEEZER_QUALITY")
	if quality == "" {
		quality = "MP3_128"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := &registry{quality: quality, logger: logger, byARL: map[string]*client{}}

	// need wraps a handler that requires an ARL: read the header, 401 when absent,
	// and hand the per-ARL client (gw search + download) to the handler.
	need := func(h func(http.ResponseWriter, *http.Request, *client)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			arl := r.Header.Get(arlHeader)
			if arl == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing " + arlHeader + " header"})
				return
			}
			h(w, r, reg.get(arl))
		}
	}

	mux := chi.NewRouter()

	mux.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.Get("/search", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		q := r.URL.Query().Get("q")
		if q == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing q"})
			return
		}
		results, err := c.gw.Search(r.Context(), q, atoiDefault(r.URL.Query().Get("limit"), 20))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	}))

	mux.Get("/resolve", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		track, err := c.gw.Resolve(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": track})
	}))

	mux.Get("/download", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		res, err := c.dl.DownloadTrack(r.Context(), id, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		defer res.Stream.Close()

		if res.Extension == ".flac" {
			w.Header().Set("Content-Type", "audio/flac")
		} else {
			w.Header().Set("Content-Type", "audio/mpeg")
		}
		if _, err := io.Copy(w, res.Stream); err != nil && !errors.Is(err, r.Context().Err()) {
			log.Printf("stream interrupted for %s: %v", id, err)
		}
	}))

	mux.Get("/artists", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		q := r.URL.Query().Get("q")
		if q == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing q"})
			return
		}
		artists, err := c.gw.SearchArtists(r.Context(), q, atoiDefault(r.URL.Query().Get("limit"), 20))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"artists": artists})
	}))

	mux.Get("/artist/image", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing name"})
			return
		}
		artists, err := c.gw.SearchArtists(r.Context(), name, 5)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		image := ""
		for _, a := range artists {
			if strings.EqualFold(strings.TrimSpace(a.Name), strings.TrimSpace(name)) {
				image = a.ImageURL
				break
			}
		}
		if image == "" && len(artists) > 0 {
			image = artists[0].ImageURL // fall back to the top match
		}
		writeJSON(w, http.StatusOK, map[string]any{"imageUrl": image})
	}))

	mux.Get("/artist/albums", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		albums, err := c.gw.ArtistAlbums(r.Context(), id, atoiDefault(r.URL.Query().Get("limit"), 100))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"albums": albums})
	}))

	mux.Get("/artist/tracks", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		tracks, err := c.gw.ArtistTracks(r.Context(), id, atoiDefault(r.URL.Query().Get("limit"), 50))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": tracks})
	}))

	mux.Get("/album/tracks", need(func(w http.ResponseWriter, r *http.Request, c *client) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		tracks, err := c.gw.AlbumTracks(r.Context(), id, atoiDefault(r.URL.Query().Get("limit"), 200))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": tracks})
	}))

	log.Printf("deezer-http listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
