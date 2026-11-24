// Package api — клиент неофициального API Яндекс Музыки (api.music.yandex.net).
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	baseURL = "https://api.music.yandex.net"

	androidClient = "YandexMusicAndroid/24023621"
	// get-file-info проверяет подпись вместе с заголовком X-Yandex-Music-Client десктопного клиента.
	desktopClient  = "YandexMusicDesktopAppWindows/5.95.0"
	desktopSignKey = "kzqU4XhfCaY6B6JTHODeq5"
	userAgent      = "ymusic-tui"
)

// Все кодеки, которые понимает mpv; сервер сам выбирает лучший под запрошенное качество.
var fileInfoCodecs = []string{"flac", "flac-mp4", "aac-mp4", "he-aac-mp4", "aac", "he-aac", "mp3"}

// Error — ошибка API с HTTP-статусом.
type Error struct {
	Status  int
	Name    string
	Message string
}

func (e *Error) Error() string {
	msg := e.Name
	if e.Message != "" {
		if msg != "" {
			msg += ": "
		}
		msg += e.Message
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("API %d: %s", e.Status, msg)
}

// IsUnauthorized — токен невалиден или истёк.
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// IsNetwork — ошибка сети (нет соединения, таймаут), а не ответ сервера.
func IsNetwork(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return false
	}
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &ne) || errors.As(err, &ue)
}

type Client struct {
	http  *http.Client
	token string

	mu  sync.RWMutex
	uid int64
}

func New(token string) *Client {
	return &Client{
		http:  &http.Client{Timeout: 20 * time.Second},
		token: token,
	}
}

// HTTP — клиент для загрузки файлов с CDN (без общего таймаута: файлы большие).
func (c *Client) HTTP() *http.Client {
	return &http.Client{Transport: c.http.Transport}
}

func (c *Client) UID() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.uid
}

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func parseError(status int, body []byte) error {
	e := &Error{Status: status}
	var env struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
		Result           *struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &env) == nil {
		var obj struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		}
		var s string
		switch {
		case len(env.Error) > 0 && json.Unmarshal(env.Error, &obj) == nil && obj.Name != "":
			e.Name, e.Message = obj.Name, obj.Message
		case len(env.Error) > 0 && json.Unmarshal(env.Error, &s) == nil:
			e.Name, e.Message = s, env.ErrorDescription
		case env.Result != nil:
			e.Name, e.Message = env.Result.Name, env.Result.Message
		}
	}
	return e
}

type request struct {
	method  string
	path    string
	query   url.Values
	form    url.Values
	json    any
	headers map[string]string
}

func (c *Client) do(ctx context.Context, r request, out any) error {
	u := baseURL + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	contentType := ""
	switch {
	case r.json != nil:
		b, err := json.Marshal(r.json)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
		contentType = "application/json"
	case r.form != nil:
		body = strings.NewReader(r.form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Yandex-Music-Client", androidClient)
	req.Header.Set("Accept-Language", "ru")
	if c.token != "" {
		req.Header.Set("Authorization", "OAuth "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return parseError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("разбор ответа %s: %w", r.path, err)
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		if len(env.Error) > 0 {
			return parseError(resp.StatusCode, data)
		}
		// Некоторые ручки отдают объект без обёртки result.
		env.Result = data
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("разбор ответа %s: %w", r.path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, request{method: http.MethodGet, path: path, query: q}, out)
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	if form == nil {
		form = url.Values{}
	}
	return c.do(ctx, request{method: http.MethodPost, path: path, form: form}, out)
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, request{method: http.MethodPost, path: path, json: body}, out)
}

func (c *Client) userPath(format string, args ...any) string {
	return fmt.Sprintf("/users/%d", c.UID()) + fmt.Sprintf(format, args...)
}

// AccountStatus проверяет токен и запоминает uid пользователя.
func (c *Client) AccountStatus(ctx context.Context) (*AccountStatus, error) {
	var st AccountStatus
	if err := c.get(ctx, "/account/status", nil, &st); err != nil {
		return nil, err
	}
	if st.Account.UID == 0 {
		return nil, &Error{Status: http.StatusUnauthorized, Name: "unauthorized", Message: "токен не принят"}
	}
	c.mu.Lock()
	c.uid = st.Account.UID
	c.mu.Unlock()
	return &st, nil
}

// Tracks — полные объекты треков по идентификаторам ("id" или "id:albumId").
func (c *Client) Tracks(ctx context.Context, ids []string) ([]Track, error) {
	const batch = 250
	out := make([]Track, 0, len(ids))
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		var part []Track
		form := url.Values{"track-ids": {strings.Join(ids[start:end], ",")}, "with-positions": {"False"}}
		if err := c.postForm(ctx, "/tracks", form, &part); err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func shortsToIDs(shorts []TrackShort) []string {
	ids := make([]string, 0, len(shorts))
	for _, s := range shorts {
		if s.AlbumID != "" {
			ids = append(ids, string(s.ID)+":"+string(s.AlbumID))
		} else {
			ids = append(ids, string(s.ID))
		}
	}
	return ids
}

// LikedTrackIDs — идентификаторы треков из «Мне нравится».
func (c *Client) LikedTrackIDs(ctx context.Context) ([]TrackShort, error) {
	var res struct {
		Library struct {
			Tracks []TrackShort `json:"tracks"`
		} `json:"library"`
	}
	if err := c.get(ctx, c.userPath("/likes/tracks"), nil, &res); err != nil {
		return nil, err
	}
	return res.Library.Tracks, nil
}

// LikedTracks — «Мне нравится» с полными данными треков.
func (c *Client) LikedTracks(ctx context.Context) ([]Track, error) {
	shorts, err := c.LikedTrackIDs(ctx)
	if err != nil {
		return nil, err
	}
	return c.Tracks(ctx, shortsToIDs(shorts))
}

func (c *Client) UserPlaylists(ctx context.Context) ([]Playlist, error) {
	var res []Playlist
	err := c.get(ctx, c.userPath("/playlists/list"), nil, &res)
	return res, err
}

func (c *Client) LikedPlaylists(ctx context.Context) ([]Playlist, error) {
	var res []struct {
		Playlist *Playlist `json:"playlist"`
	}
	if err := c.get(ctx, c.userPath("/likes/playlists"), nil, &res); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(res))
	for _, r := range res {
		if r.Playlist != nil {
			out = append(out, *r.Playlist)
		}
	}
	return out, nil
}

// PlaylistTracks — треки плейлиста; недостающие полные объекты догружаются через /tracks.
func (c *Client) PlaylistTracks(ctx context.Context, uid, kind int64) ([]Track, error) {
	var pl Playlist
	q := url.Values{"rich-tracks": {"true"}}
	if err := c.get(ctx, fmt.Sprintf("/users/%d/playlists/%d", uid, kind), q, &pl); err != nil {
		return nil, err
	}
	var missing []TrackShort
	for _, s := range pl.Tracks {
		if s.Track == nil {
			missing = append(missing, s)
		}
	}
	byID := map[ID]Track{}
	if len(missing) > 0 {
		full, err := c.Tracks(ctx, shortsToIDs(missing))
		if err != nil {
			return nil, err
		}
		for _, t := range full {
			byID[t.ID] = t
		}
	}
	out := make([]Track, 0, len(pl.Tracks))
	for _, s := range pl.Tracks {
		if s.Track != nil {
			out = append(out, *s.Track)
		} else if t, ok := byID[s.ID]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (c *Client) LikedAlbums(ctx context.Context) ([]Album, error) {
	var res []struct {
		Album *Album `json:"album"`
	}
	if err := c.get(ctx, c.userPath("/likes/albums"), url.Values{"rich": {"true"}}, &res); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(res))
	for _, r := range res {
		if r.Album != nil {
			out = append(out, *r.Album)
		}
	}
	return out, nil
}

func (c *Client) AlbumTracks(ctx context.Context, id ID) ([]Track, error) {
	var al Album
	if err := c.get(ctx, "/albums/"+string(id)+"/with-tracks", nil, &al); err != nil {
		return nil, err
	}
	var out []Track
	for _, vol := range al.Volumes {
		out = append(out, vol...)
	}
	return out, nil
}

func (c *Client) ArtistTracks(ctx context.Context, id ID) ([]Track, error) {
	var res struct {
		Tracks []Track `json:"tracks"`
	}
	q := url.Values{"page": {"0"}, "page-size": {"100"}}
	err := c.get(ctx, "/artists/"+string(id)+"/tracks", q, &res)
	return res.Tracks, err
}

func (c *Client) Search(ctx context.Context, text string) (*SearchResult, error) {
	var res SearchResult
	q := url.Values{"text": {text}, "type": {"all"}, "page": {"0"}, "nocorrect": {"False"}, "playlist-in-best": {"True"}}
	if err := c.get(ctx, "/search", q, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// LikeTrack / UnlikeTrack / DislikeTrack принимают ключ "trackId:albumId".
func (c *Client) LikeTrack(ctx context.Context, key string) error {
	return c.postForm(ctx, c.userPath("/likes/tracks/add-multiple"), url.Values{"track-ids": {key}}, nil)
}

func (c *Client) UnlikeTrack(ctx context.Context, key string) error {
	return c.postForm(ctx, c.userPath("/likes/tracks/remove"), url.Values{"track-ids": {key}}, nil)
}

func (c *Client) DislikeTrack(ctx context.Context, key string) error {
	return c.postForm(ctx, c.userPath("/dislikes/tracks/add-multiple"), url.Values{"track-ids": {key}}, nil)
}

// signFileInfo — HMAC-SHA256 от ts+trackId+quality+codecs+transport, base64 без последнего символа.
func signFileInfo(ts int64, trackID, quality string, codecs []string, transport string) string {
	msg := strconv.FormatInt(ts, 10) + trackID + quality + strings.Join(codecs, "") + transport
	mac := hmac.New(sha256.New, []byte(desktopSignKey))
	mac.Write([]byte(msg))
	s := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return s[:len(s)-1]
}

// FileInfo — ссылка на аудиофайл в запрошенном качестве (lossless/hq/nq).
// Если трека нет в запрошенном качестве, сервер вернёт лучший доступный вариант.
func (c *Client) FileInfo(ctx context.Context, trackID ID, quality string) (*DownloadInfo, error) {
	const transport = "encraw"
	ts := time.Now().Unix()
	id := string(trackID)
	q := url.Values{
		"ts":         {strconv.FormatInt(ts, 10)},
		"trackId":    {id},
		"quality":    {quality},
		"codecs":     {strings.Join(fileInfoCodecs, ",")},
		"transports": {transport},
		"sign":       {signFileInfo(ts, id, quality, fileInfoCodecs, transport)},
	}
	var res struct {
		DownloadInfo *DownloadInfo `json:"downloadInfo"`
	}
	err := c.do(ctx, request{
		method:  http.MethodGet,
		path:    "/get-file-info",
		query:   q,
		headers: map[string]string{"X-Yandex-Music-Client": desktopClient},
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.DownloadInfo == nil || (res.DownloadInfo.URL == "" && len(res.DownloadInfo.URLs) == 0) {
		return nil, fmt.Errorf("трек %s: нет ссылки на файл (нужна подписка Плюс?)", id)
	}
	if res.DownloadInfo.URL == "" {
		res.DownloadInfo.URL = res.DownloadInfo.URLs[0]
	}
	return res.DownloadInfo, nil
}

// --- Моя волна (rotor session) ---

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// RotorSessionNew создаёт сессию радио по сидам, например "user:onyourwave".
func (c *Client) RotorSessionNew(ctx context.Context, seeds []string, queue []string) (*RotorSession, error) {
	body := map[string]any{
		"seeds":                   seeds,
		"includeTracksInResponse": true,
	}
	if len(queue) > 0 {
		body["queue"] = queue
	}
	var s RotorSession
	if err := c.postJSON(ctx, "/rotor/session/new", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RotorSessionTracks — следующая партия треков; queue — уже проигранные ключи "id:albumId".
func (c *Client) RotorSessionTracks(ctx context.Context, sessionID string, queue []string) (*RotorSession, error) {
	if queue == nil {
		queue = []string{}
	}
	var s RotorSession
	if err := c.postJSON(ctx, "/rotor/session/"+sessionID+"/tracks", map[string]any{"queue": queue}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RotorEvent — событие обратной связи для волны.
type RotorEvent struct {
	Type               string   // radioStarted | trackStarted | trackFinished | skip
	TrackKey           string   // "id:albumId"
	TotalPlayedSeconds *float64 // для trackFinished и skip
}

func (c *Client) RotorFeedback(ctx context.Context, sessionID, batchID string, ev RotorEvent) error {
	event := map[string]any{"type": ev.Type, "timestamp": nowISO()}
	if ev.TrackKey != "" {
		event["trackId"] = ev.TrackKey
	}
	if ev.TotalPlayedSeconds != nil {
		event["totalPlayedSeconds"] = *ev.TotalPlayedSeconds
	}
	body := map[string]any{"event": event}
	if batchID != "" {
		body["batchId"] = batchID
	}
	return c.postJSON(ctx, "/rotor/session/"+sessionID+"/feedback", body, nil)
}
