package deezer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const deezerAPIBase = "https://api.deezer.com"

// Settings regroupe la configuration Deezer (équivalent de DeezerSettings).
type Settings struct {
	ARL         string
	ARLFallback string
	Quality     string
}

// Song est un placeholder du modèle de domaine. Il n'est pas utilisé par
// DownloadTrack mais conservé pour rester fidèle au contrat de la classe de base.
type Song struct {
	ID     string
	Title  string
	Artist string
}

// DownloadResult correspond au DownloadResult C# : flux (à fermer par l'appelant),
// extension de fichier et qualité réellement obtenue.
type DownloadResult struct {
	Stream    io.ReadCloser
	Extension string
	Quality   string
}

// TrackDownloadInfo contient l'URL de téléchargement résolue et ses métadonnées.
type TrackDownloadInfo struct {
	DownloadURL string
	Format      string
	Title       string
	Artist      string
	TrackID     string // ID utilisé pour le déchiffrement (peut différer si alternative)
}

// DownloadService est le port de DeezerDownloadService : authentification ARL,
// résolution de l'URL de téléchargement et déchiffrement à la volée.
type DownloadService struct {
	httpClient *http.Client
	requestMu  sync.Mutex // équivalent du SemaphoreSlim(1,1), non réentrant

	arl              string
	arlFallback      string
	preferredQuality string

	apiToken     string
	licenseToken string

	lastRequestTime    time.Time
	minRequestInterval time.Duration

	logger *slog.Logger
}

// NewDownloadService construit le service. httpClient et logger peuvent être nil.
func NewDownloadService(httpClient *http.Client, settings Settings, logger *slog.Logger) *DownloadService {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &DownloadService{
		httpClient:         httpClient,
		arl:                settings.ARL,
		arlFallback:        settings.ARLFallback,
		preferredQuality:   settings.Quality,
		minRequestInterval: 200 * time.Millisecond,
		logger:             logger,
	}
}

// ProviderName renvoie l'identifiant du fournisseur.
func (s *DownloadService) ProviderName() string { return "deezer" }

// --- Implémentation type BaseDownloadService ---------------------------------

// IsAvailable indique si le service est utilisable (ARL configuré et valide).
func (s *DownloadService) IsAvailable(ctx context.Context) bool {
	if s.arl == "" {
		s.logger.Warn("Deezer ARL not configured")
		return false
	}
	if err := s.initialize(ctx, ""); err != nil {
		s.logger.Warn("Deezer service not available", "err", err)
		return false
	}
	return true
}

// ExtractExternalIDFromAlbumID extrait l'ID externe d'un albumID préfixé.
func (s *DownloadService) ExtractExternalIDFromAlbumID(albumID string) (string, bool) {
	const prefix = "ext-deezer-album-"
	if strings.HasPrefix(albumID, prefix) {
		return albumID[len(prefix):], true
	}
	return "", false
}

// GetTargetQuality renvoie la qualité cible (FLAC par défaut).
func (s *DownloadService) GetTargetQuality() string {
	if s.preferredQuality != "" {
		return s.preferredQuality
	}
	return "FLAC"
}

// DownloadTrack résout l'URL, télécharge le fichier chiffré et renvoie un flux déchiffré.
func (s *DownloadService) DownloadTrack(ctx context.Context, trackID string, song *Song) (*DownloadResult, error) {
	_ = song // non utilisé, conservé pour fidélité au contrat

	info, err := s.getTrackDownloadInfo(ctx, trackID)
	if err != nil {
		return nil, err
	}

	s.logger.Info("Track token obtained", "title", info.Title, "artist", info.Artist)
	s.logger.Info("Using format", "format", info.Format)

	extension := ".mp3"
	if strings.EqualFold(info.Format, "FLAC") {
		extension = ".flac"
	}

	downloadedQuality := info.Format
	switch strings.ToUpper(info.Format) {
	case "FLAC":
		downloadedQuality = "FLAC"
	case "MP3_320":
		downloadedQuality = "MP3_320"
	case "MP3_128":
		downloadedQuality = "MP3_128"
	}

	// Téléchargement du fichier chiffré (le retry ne couvre que le transport,
	// comme en C# où EnsureSuccessStatusCode est appelé hors du retry).
	resp, err := retryWithBackoff(ctx, s.logger, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.DownloadURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		req.Header.Set("Accept", "*/*")
		return s.httpClient.Do(req)
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: status %d", resp.StatusCode)
	}

	// NB : comme le C# original, le déchiffrement utilise trackID (le paramètre),
	// pas info.TrackID — à surveiller si une piste alternative a été retenue.
	stream := NewDecryptedReader(resp.Body, trackID)

	return &DownloadResult{
		Stream:    stream,
		Extension: extension,
		Quality:   downloadedQuality,
	}, nil
}

// --- Méthodes API Deezer -----------------------------------------------------

func (s *DownloadService) initialize(ctx context.Context, arlOverride string) error {
	arl := arlOverride
	if arl == "" {
		arl = s.arl
	}
	if arl == "" {
		return errors.New("ARL token required for Deezer downloads")
	}

	_, err := retryWithBackoff(ctx, s.logger, func() (bool, error) {
		const u = "https://www.deezer.com/ajax/gw-light.php?method=deezer.getUserData&input=3&api_version=1.0&api_token=null"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("{}"))
		if err != nil {
			return false, err
		}
		req.Header.Set("Cookie", "arl="+arl)
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if err := ensureSuccess(resp); err != nil {
			return false, err
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}

		var parsed struct {
			Results *struct {
				CheckForm *string `json:"checkForm"`
				User      struct {
					Options struct {
						LicenseToken string `json:"license_token"`
					} `json:"OPTIONS"`
				} `json:"USER"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return false, err
		}
		if parsed.Results == nil || parsed.Results.CheckForm == nil {
			return false, errors.New("invalid ARL token")
		}

		s.apiToken = *parsed.Results.CheckForm
		if parsed.Results.User.Options.LicenseToken != "" {
			s.licenseToken = parsed.Results.User.Options.LicenseToken
		}
		s.logger.Info("Deezer token refreshed successfully")
		return true, nil
	})
	return err
}

func (s *DownloadService) getTrackDownloadInfo(ctx context.Context, trackID string) (*TrackDownloadInfo, error) {
	return s.getTrackDownloadInfoInternal(ctx, trackID, s.arl, false)
}

// queueResult porte soit un résultat, soit une instruction de retry, afin
// d'orchestrer la récursion HORS du verrou (le mutex est non réentrant).
type queueResult struct {
	info       *TrackDownloadInfo
	retryAltID string
	retryARL   string
}

func (s *DownloadService) getTrackDownloadInfoInternal(ctx context.Context, trackID, arl string, isRetryWithFallback bool) (*TrackDownloadInfo, error) {
	if err := s.initialize(ctx, arl); err != nil {
		return nil, err
	}

	res, err := queueRequest(ctx, s, func() (queueResult, error) {
		// Récupère les infos de piste.
		var track trackResponse
		if err := s.getJSON(ctx, fmt.Sprintf("%s/track/%s", deezerAPIBase, trackID), &track); err != nil {
			return queueResult{}, err
		}

		decryptionTrackID := trackID
		title := track.Title
		artist := ""
		if track.Artist != nil {
			artist = track.Artist.Name
		}
		isReadable := track.Readable

		// Piste indisponible : chercher une alternative.
		if !isReadable {
			s.logger.Warn("Track not readable, searching for alternative",
				"trackId", trackID, "title", title, "artist", artist)

			altID := s.findAlternativeTrackWithFallbacks(ctx, trackID, title, artist, arl)
			if altID != "" {
				s.logger.Info("Found alternative track", "alternativeId", altID)
				decryptionTrackID = altID

				var alt trackResponse
				if err := s.getJSON(ctx, fmt.Sprintf("%s/track/%s", deezerAPIBase, altID), &alt); err != nil {
					return queueResult{}, err
				}
				track = alt
			} else {
				return queueResult{}, fmt.Errorf("track is not available in your region and no alternative found for: %s - %s", title, artist)
			}
		}

		if track.TrackToken == nil {
			return queueResult{}, errors.New("track not found or track_token missing")
		}
		trackToken := *track.TrackToken

		// TRACK_TOKEN lié à la session via l'API privée, si disponible.
		if pageData := s.getTrackPageData(ctx, decryptionTrackID, arl); pageData != nil && pageData.TrackToken != "" {
			s.logger.Info("Using session-bound TRACK_TOKEN from private API", "trackId", decryptionTrackID)
			trackToken = pageData.TrackToken
		}

		// Demande de l'URL via l'API media.
		reqBody, err := json.Marshal(mediaRequest{
			LicenseToken: s.licenseToken,
			Media:        []mediaSpec{{Type: "FULL", Formats: buildFormatsList(s.preferredQuality)}},
			TrackTokens:  []string{trackToken},
		})
		if err != nil {
			return queueResult{}, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://media.deezer.com/v1/get_url", bytes.NewReader(reqBody))
		if err != nil {
			return queueResult{}, err
		}
		req.Header.Set("Cookie", "arl="+arl)
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return queueResult{}, err
		}
		defer resp.Body.Close()
		if err := ensureSuccess(resp); err != nil {
			return queueResult{}, err
		}

		mediaBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return queueResult{}, err
		}

		var media mediaResponse
		if err := json.Unmarshal(mediaBody, &media); err != nil {
			return queueResult{}, err
		}
		if len(media.Data) == 0 {
			return queueResult{}, errors.New("no download URL available")
		}

		first := media.Data[0]
		hasMedia := len(first.Media) > 0

		// Aucun media et premier essai : tenter les fallbacks.
		if !hasMedia && !isRetryWithFallback {
			s.logger.Warn("Track returned no media sources, trying fallbacks",
				"trackId", trackID, "readable", isReadable)

			altID := s.findAlternativeTrackWithFallbacks(ctx, trackID, title, artist, arl)
			if altID != "" && altID != trackID {
				s.logger.Info("Retrying with alternative track", "alternativeId", altID)
				return queueResult{retryAltID: altID}, nil
			}
			if s.arlFallback != "" && arl != s.arlFallback {
				s.logger.Warn("No alternative found, trying fallback ARL")
				return queueResult{retryARL: s.arlFallback}, nil
			}
			return queueResult{}, errors.New("no media sources available - track may be unavailable in your region")
		}
		if !hasMedia {
			return queueResult{}, errors.New("no media sources available - track may be unavailable in your region")
		}

		// Dictionnaire des formats disponibles.
		availableFormats := make(map[string]string)
		for _, m := range first.Media {
			if m.Format != "" && len(m.Sources) > 0 && m.Sources[0].URL != "" {
				availableFormats[m.Format] = m.Sources[0].URL
			}
		}
		if len(availableFormats) == 0 {
			return queueResult{}, errors.New("no download URL found in media sources - track may be region locked")
		}
		s.logger.Info("Available formats from Deezer", "formats", strings.Join(sortedKeys(availableFormats), ", "))

		// Sélection de la meilleure qualité disponible.
		var selectedFormat, downloadURL string
		for _, q := range []string{"FLAC", "MP3_320", "MP3_128"} {
			if u, ok := availableFormats[q]; ok {
				selectedFormat, downloadURL = q, u
				break
			}
		}
		if downloadURL == "" {
			return queueResult{}, errors.New("no compatible format found in available media sources")
		}
		s.logger.Info("Selected quality", "format", selectedFormat)
		if selectedFormat == "" {
			selectedFormat = "MP3_128"
		}

		return queueResult{info: &TrackDownloadInfo{
			DownloadURL: downloadURL,
			Format:      selectedFormat,
			Title:       title,
			Artist:      artist,
			TrackID:     decryptionTrackID,
		}}, nil
	})
	if err != nil {
		return nil, err
	}

	switch {
	case res.info != nil:
		return res.info, nil
	case res.retryAltID != "":
		return s.getTrackDownloadInfoInternal(ctx, res.retryAltID, arl, true)
	case res.retryARL != "":
		return s.getTrackDownloadInfoInternal(ctx, trackID, res.retryARL, true)
	default:
		return nil, errors.New("getTrackDownloadInfoInternal: lambda returned no info and no retry instruction")
	}
}

// --- Recherche d'alternatives ------------------------------------------------

type trackPageData struct {
	FallbackID string
	ISRC       string
	TrackToken string
}

// getTrackPageData interroge l'API privée (deezer.pageTrack). Toute erreur est
// loguée et donne un retour nil (jamais propagée), comme le try/catch C#.
func (s *DownloadService) getTrackPageData(ctx context.Context, trackID, arl string) *trackPageData {
	warn := func(err error) { s.logger.Warn("Error getting track page data", "trackId", trackID, "err", err) }

	u := fmt.Sprintf("https://www.deezer.com/ajax/gw-light.php?method=deezer.pageTrack&input=3&api_version=1.0&api_token=%s", s.apiToken)
	reqBody, _ := json.Marshal(map[string]string{"SNG_ID": trackID})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		warn(err)
		return nil
	}
	req.Header.Set("Cookie", "arl="+arl)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		warn(err)
		return nil
	}
	defer resp.Body.Close()
	if err := ensureSuccess(resp); err != nil {
		warn(err)
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		warn(err)
		return nil
	}

	var parsed struct {
		Error   json.RawMessage `json:"error"`
		Results *struct {
			Data *struct {
				Fallback *struct {
					SngID *string `json:"SNG_ID"`
				} `json:"FALLBACK"`
				ISRC       *string `json:"ISRC"`
				TrackToken *string `json:"TRACK_TOKEN"`
			} `json:"DATA"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		warn(err)
		return nil
	}

	// Un tableau "error" vide n'est pas une vraie erreur ; tout le reste l'est.
	if len(parsed.Error) > 0 && !bytes.Equal(bytes.TrimSpace(parsed.Error), []byte("[]")) {
		s.logger.Warn("pageTrack API error", "trackId", trackID, "error", string(parsed.Error))
		return nil
	}
	if parsed.Results == nil {
		s.logger.Warn("pageTrack returned no results", "trackId", trackID)
		return nil
	}

	data := &trackPageData{}
	if d := parsed.Results.Data; d != nil {
		if d.Fallback != nil && d.Fallback.SngID != nil {
			data.FallbackID = *d.Fallback.SngID
		}
		if d.ISRC != nil {
			data.ISRC = *d.ISRC
		}
		if d.TrackToken != nil {
			data.TrackToken = *d.TrackToken
		}
	}
	return data
}

// findTrackByISRC recherche une piste par code ISRC. Renvoie "" si rien de lisible.
func (s *DownloadService) findTrackByISRC(ctx context.Context, isrc string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/track/isrc:%s", deezerAPIBase, isrc), nil)
	if err != nil {
		s.logger.Warn("Error searching track by ISRC", "isrc", isrc, "err", err)
		return ""
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Warn("Error searching track by ISRC", "isrc", isrc, "err", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var parsed struct {
		Error    json.RawMessage `json:"error"`
		Readable bool            `json:"readable"`
		ID       json.Number     `json:"id"`
		Title    string          `json:"title"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if len(parsed.Error) > 0 { // toute erreur présente => invalide
		return ""
	}
	if !parsed.Readable || parsed.ID == "" {
		return ""
	}

	id := parsed.ID.String()
	title := parsed.Title
	if title == "" {
		title = "Unknown"
	}
	s.logger.Info("Found track by ISRC", "isrc", isrc, "title", title, "id", id)
	return id
}

// findAlternativeTrackWithFallbacks essaie : FALLBACK_ID, puis ISRC, puis titre/artiste.
func (s *DownloadService) findAlternativeTrackWithFallbacks(ctx context.Context, originalTrackID, title, artist, arl string) string {
	pageData := s.getTrackPageData(ctx, originalTrackID, arl)

	if pageData != nil && pageData.FallbackID != "" && pageData.FallbackID != "0" && pageData.FallbackID != originalTrackID {
		s.logger.Info("Using Deezer FALLBACK_ID", "fallbackId", pageData.FallbackID, "trackId", originalTrackID)
		return pageData.FallbackID
	}

	if pageData != nil && pageData.ISRC != "" {
		if isrcID := s.findTrackByISRC(ctx, pageData.ISRC); isrcID != "" && isrcID != originalTrackID {
			s.logger.Info("Found alternative via ISRC", "isrc", pageData.ISRC, "alternativeId", isrcID)
			return isrcID
		}
	}

	s.logger.Info("Trying title/artist search fallback", "title", title, "artist", artist)
	return s.findAlternativeTrack(ctx, title, artist)
}

// findAlternativeTrack recherche une piste lisible par titre/artiste.
func (s *DownloadService) findAlternativeTrack(ctx context.Context, title, artist string) string {
	normalized := normalizeTitle(title)
	q := url.QueryEscape(normalized + " " + artist)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/search/track?q=%s&limit=10", deezerAPIBase, q), nil)
	if err != nil {
		s.logger.Warn("Error searching for alternative track", "err", err)
		return ""
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Warn("Error searching for alternative track", "err", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.logger.Warn("Error searching for alternative track", "status", resp.StatusCode)
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var parsed struct {
		Data *[]struct {
			ID       json.Number `json:"id"`
			Title    string      `json:"title"`
			Readable bool        `json:"readable"`
			Artist   *struct {
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if parsed.Data == nil {
		return ""
	}
	tracks := *parsed.Data

	// 1) Correspondance exacte (titre normalisé ou exact + artiste).
	for _, t := range tracks {
		if !t.Readable || t.Title == "" {
			continue
		}
		trackArtist := ""
		if t.Artist != nil {
			trackArtist = t.Artist.Name
		}
		titleMatch := strings.EqualFold(normalizeTitle(t.Title), normalized) || strings.EqualFold(t.Title, title)
		if titleMatch && strings.EqualFold(artist, trackArtist) {
			id := t.ID.String()
			s.logger.Info("Found alternative", "title", t.Title, "artist", trackArtist, "id", id)
			return id
		}
	}

	// 2) Correspondance souple (titre contient le titre normalisé + artiste).
	lowerNorm := strings.ToLower(normalized)
	for _, t := range tracks {
		if !t.Readable || t.Title == "" {
			continue
		}
		trackArtist := ""
		if t.Artist != nil {
			trackArtist = t.Artist.Name
		}
		if strings.Contains(strings.ToLower(t.Title), lowerNorm) && strings.EqualFold(artist, trackArtist) {
			id := t.ID.String()
			s.logger.Info("Found alternative (lenient match)", "title", t.Title, "artist", trackArtist, "id", id)
			return id
		}
	}

	return ""
}

// normalizeTitle retire un suffixe entre parenthèses : "Song (From X)" -> "Song".
func normalizeTitle(title string) string {
	if i := strings.IndexByte(title, '('); i > 0 {
		return strings.TrimSpace(title[:i])
	}
	return title
}

// --- Utilitaires -------------------------------------------------------------

type formatSpec struct {
	Cipher string `json:"cipher"`
	Format string `json:"format"`
}

type mediaSpec struct {
	Type    string       `json:"type"`
	Formats []formatSpec `json:"formats"`
}

type mediaRequest struct {
	LicenseToken string      `json:"license_token"`
	Media        []mediaSpec `json:"media"`
	TrackTokens  []string    `json:"track_tokens"`
}

type mediaResponse struct {
	Data []struct {
		Media []struct {
			Format  string `json:"format"`
			Sources []struct {
				URL string `json:"url"`
			} `json:"sources"`
		} `json:"media"`
	} `json:"data"`
}

type trackResponse struct {
	Title  string `json:"title"`
	Artist *struct {
		Name string `json:"name"`
	} `json:"artist"`
	Readable   bool    `json:"readable"`
	TrackToken *string `json:"track_token"`
}

// buildFormatsList construit la liste de formats demandés selon la qualité préférée.
func buildFormatsList(preferred string) []formatSpec {
	all := []formatSpec{
		{Cipher: "BF_CBC_STRIPE", Format: "FLAC"},
		{Cipher: "BF_CBC_STRIPE", Format: "MP3_320"},
		{Cipher: "BF_CBC_STRIPE", Format: "MP3_128"},
	}
	if preferred == "" {
		return all
	}
	switch strings.ToUpper(preferred) {
	case "FLAC":
		return all
	case "MP3_320":
		return []formatSpec{
			{Cipher: "BF_CBC_STRIPE", Format: "MP3_320"},
			{Cipher: "BF_CBC_STRIPE", Format: "MP3_128"},
		}
	case "MP3_128":
		return []formatSpec{{Cipher: "BF_CBC_STRIPE", Format: "MP3_128"}}
	default:
		return all
	}
}

// retryableError marque une erreur retentable (HTTP 429/503).
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

// ensureSuccess transforme un statut HTTP non-2xx en erreur, retentable pour 429/503.
func ensureSuccess(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err := fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
		return retryableError{err}
	}
	return err
}

// retryWithBackoff réessaie l'action (backoff exponentiel) uniquement sur erreur
// retentable, sinon propage immédiatement — fidèle au RetryWithBackoffAsync C#.
func retryWithBackoff[T any](ctx context.Context, logger *slog.Logger, action func() (T, error)) (T, error) {
	const maxRetries = 3
	const initialDelay = time.Second

	var zero T
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		result, err := action()
		if err == nil {
			return result, nil
		}

		var re retryableError
		if !errors.As(err, &re) {
			return zero, err // non retentable
		}

		lastErr = err
		if attempt < maxRetries-1 {
			delay := time.Duration(float64(initialDelay) * math.Pow(2, float64(attempt)))
			logger.Warn("Retry attempt",
				"attempt", attempt+1, "maxRetries", maxRetries,
				"delayMs", delay.Milliseconds(), "err", err.Error())
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return zero, ctx.Err()
			}
		}
	}
	return zero, lastErr
}

// queueRequest sérialise les appels et impose un intervalle minimal entre requêtes.
// Fonction libre (et non méthode) car les méthodes Go ne peuvent pas être génériques.
func queueRequest[T any](ctx context.Context, s *DownloadService, action func() (T, error)) (T, error) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	var zero T
	if since := time.Since(s.lastRequestTime); since < s.minRequestInterval {
		select {
		case <-time.After(s.minRequestInterval - since):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	s.lastRequestTime = time.Now()
	return action()
}

// getJSON effectue un GET et décode la réponse JSON dans v.
func (s *DownloadService) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := ensureSuccess(resp); err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
