package api

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ID принимает и число, и строку: API отдаёт идентификаторы по-разному в разных ручках.
type ID string

func (i *ID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*i = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*i = ID(s)
		return nil
	}
	*i = ID(string(b))
	return nil
}

func (i ID) String() string { return string(i) }

type Artist struct {
	ID      ID     `json:"id"`
	Name    string `json:"name"`
	Various bool   `json:"various"`
}

type Album struct {
	ID         ID        `json:"id"`
	Title      string    `json:"title"`
	Version    string    `json:"version"`
	Year       int       `json:"year"`
	TrackCount int       `json:"trackCount"`
	Artists    []Artist  `json:"artists"`
	CoverURI   string    `json:"coverUri"`
	Volumes    [][]Track `json:"volumes"`
}

// R128 — громкость трека по EBU R 128: интегральная (LUFS) и true peak (dBTP).
type R128 struct {
	I  float64 `json:"i"`
	TP float64 `json:"tp"`
}

// Fade — точки нарастания и затухания трека в секундах (используются для умного кроссфейда).
type Fade struct {
	InStart  float64 `json:"inStart"`
	InStop   float64 `json:"inStop"`
	OutStart float64 `json:"outStart"`
	OutStop  float64 `json:"outStop"`
}

type Track struct {
	ID             ID       `json:"id"`
	RealID         ID       `json:"realId"`
	Title          string   `json:"title"`
	Version        string   `json:"version"`
	Available      *bool    `json:"available"`
	DurationMs     int64    `json:"durationMs"`
	Artists        []Artist `json:"artists"`
	Albums         []Album  `json:"albums"`
	CoverURI       string   `json:"coverUri"`
	R128           *R128    `json:"r128"`
	Fade           *Fade    `json:"fade"`
	ContentWarning string   `json:"contentWarning"`
	Error          string   `json:"error"`
}

// AlbumID — альбом, к которому относится трек (первый из списка).
func (t *Track) AlbumID() ID {
	if len(t.Albums) > 0 {
		return t.Albums[0].ID
	}
	return ""
}

// Key — идентификатор вида "trackId:albumId", который ждут лайки и радио.
func (t *Track) Key() string {
	if a := t.AlbumID(); a != "" {
		return string(t.ID) + ":" + string(a)
	}
	return string(t.ID)
}

// IsAvailable — можно ли проиграть трек (поле отсутствует у части ответов — считаем доступным).
func (t *Track) IsAvailable() bool {
	return t.Available == nil || *t.Available
}

func (t *Track) FullTitle() string {
	if t.Version != "" {
		return t.Title + " (" + t.Version + ")"
	}
	return t.Title
}

func (t *Track) ArtistNames() string {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

func (t *Track) AlbumTitle() string {
	if len(t.Albums) > 0 {
		return t.Albums[0].Title
	}
	return ""
}

func (a *Album) ArtistNames() string {
	names := make([]string, 0, len(a.Artists))
	for _, ar := range a.Artists {
		names = append(names, ar.Name)
	}
	return strings.Join(names, ", ")
}

type Owner struct {
	UID   int64  `json:"uid"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

type TrackShort struct {
	ID      ID     `json:"id"`
	AlbumID ID     `json:"albumId"`
	Track   *Track `json:"track"`
}

type Playlist struct {
	UID        int64        `json:"uid"`
	Kind       int64        `json:"kind"`
	Title      string       `json:"title"`
	TrackCount int          `json:"trackCount"`
	Owner      *Owner       `json:"owner"`
	Tracks     []TrackShort `json:"tracks"`
}

func (p *Playlist) OwnerName() string {
	if p.Owner == nil {
		return ""
	}
	if p.Owner.Name != "" {
		return p.Owner.Name
	}
	return p.Owner.Login
}

// DownloadInfo — ответ get-file-info: ссылка на файл и параметры потока.
type DownloadInfo struct {
	TrackID   ID       `json:"trackId"`
	Quality   string   `json:"quality"`
	Codec     string   `json:"codec"`
	Bitrate   int      `json:"bitrate"`
	Transport string   `json:"transport"`
	URL       string   `json:"url"`
	URLs      []string `json:"urls"`
	Key       string   `json:"key"`
	Gain      bool     `json:"gain"`
}

// Encrypted — файл зашифрован AES-128-CTR (transport=encraw).
func (d *DownloadInfo) Encrypted() bool {
	return d.Transport == "encraw" && d.Key != ""
}

type Account struct {
	UID         int64  `json:"uid"`
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
	FullName    string `json:"fullName"`
}

type AccountStatus struct {
	Account Account `json:"account"`
	Plus    struct {
		HasPlus bool `json:"hasPlus"`
	} `json:"plus"`
}

type SearchResult struct {
	Tracks struct {
		Results []Track `json:"results"`
	} `json:"tracks"`
	Albums struct {
		Results []Album `json:"results"`
	} `json:"albums"`
	Artists struct {
		Results []Artist `json:"results"`
	} `json:"artists"`
	Playlists struct {
		Results []Playlist `json:"results"`
	} `json:"playlists"`
}

// RotorSession — сессия «Моей волны».
type RotorSession struct {
	RadioSessionID string `json:"radioSessionId"`
	BatchID        string `json:"batchId"`
	Sequence       []struct {
		Type  string `json:"type"`
		Track *Track `json:"track"`
		Liked bool   `json:"liked"`
	} `json:"sequence"`
}

func (s *RotorSession) Tracks() []Track {
	out := make([]Track, 0, len(s.Sequence))
	for _, it := range s.Sequence {
		if it.Track != nil {
			out = append(out, *it.Track)
		}
	}
	return out
}
