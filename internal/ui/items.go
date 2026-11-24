package ui

import (
	"context"
	"fmt"
	"log"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ymusic/internal/api"
	"ymusic/internal/stream"
)

type itemKind int

const (
	kTrack itemKind = iota
	kPlaylist
	kAlbum
	kArtist
	kWave
	kHeader
	kSetting
	kInfo
)

type item struct {
	kind     itemKind
	track    api.Track
	playlist api.Playlist
	album    api.Album
	artist   api.Artist
	title    string
	sub      string
	seeds    []string
	setting  settingID
	cached   *stream.Entry
}

func (it item) selectable() bool { return it.kind != kHeader && it.kind != kInfo }

type view struct {
	id      int
	title   string
	items   []item
	cursor  int
	offset  int
	loading bool
	err     string
	load    func() ([]item, error)
}

// viewLoadedMsg — результат асинхронной загрузки списка.
type viewLoadedMsg struct {
	id    int
	items []item
	err   error
}

func loadCmd(v *view) tea.Cmd {
	if v.load == nil {
		return nil
	}
	v.loading = true
	v.err = ""
	id, load := v.id, v.load
	return func() tea.Msg {
		items, err := load()
		return viewLoadedMsg{id: id, items: items, err: err}
	}
}

func ctx30() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func trackItems(tracks []api.Track) []item {
	out := make([]item, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, item{kind: kTrack, track: t})
	}
	return out
}

func header(s string) item { return item{kind: kHeader, title: s} }

var waveVariants = []struct {
	title string
	sub   string
	seeds []string
}{
	{"Моя волна", "персональный поток рекомендаций", []string{"user:onyourwave"}},
	{"Моя волна · Любимое", "больше знакомых треков", []string{"user:onyourwave", "settingDiversity:favorite"}},
	{"Моя волна · Незнакомое", "новые для вас исполнители", []string{"user:onyourwave", "settingDiversity:discover"}},
	{"Моя волна · Популярное", "то, что слушают сейчас", []string{"user:onyourwave", "settingDiversity:popular"}},
	{"Моя волна · Бодрое", "энергичное", []string{"user:onyourwave", "settingMoodEnergy:active"}},
	{"Моя волна · Весёлое", "", []string{"user:onyourwave", "settingMoodEnergy:fun"}},
	{"Моя волна · Спокойное", "", []string{"user:onyourwave", "settingMoodEnergy:calm"}},
	{"Моя волна · Грустное", "", []string{"user:onyourwave", "settingMoodEnergy:sad"}},
}

func waveItems() []item {
	out := []item{header("Запустить")}
	for _, w := range waveVariants {
		out = append(out, item{kind: kWave, title: w.title, sub: w.sub, seeds: w.seeds})
	}
	out = append(out,
		header("Подсказки"),
		item{kind: kInfo, title: "l — лайк играющего трека, d — дизлайк (трек пропускается и больше не попадётся)"},
		item{kind: kInfo, title: "w на треке или исполнителе в любом списке — волна по нему"},
	)
	return out
}

func (m *Model) likesLoader() func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		tracks, err := m.api.LikedTracks(ctx)
		if err != nil {
			return nil, err
		}
		return trackItems(tracks), nil
	}
}

func (m *Model) playlistsLoader() func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		own, err := m.api.UserPlaylists(ctx)
		if err != nil {
			return nil, err
		}
		liked, err := m.api.LikedPlaylists(ctx)
		if err != nil {
			liked = nil // не критично
		}
		items := []item{header("Мои плейлисты")}
		for _, p := range own {
			items = append(items, item{kind: kPlaylist, playlist: p})
		}
		if len(liked) > 0 {
			items = append(items, header("Сохранённые"))
			for _, p := range liked {
				items = append(items, item{kind: kPlaylist, playlist: p})
			}
		}
		return items, nil
	}
}

func (m *Model) albumsLoader() func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		albums, err := m.api.LikedAlbums(ctx)
		if err != nil {
			return nil, err
		}
		items := make([]item, 0, len(albums))
		for _, a := range albums {
			items = append(items, item{kind: kAlbum, album: a})
		}
		return items, nil
	}
}

func (m *Model) playlistTracksLoader(p api.Playlist) func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		tracks, err := m.api.PlaylistTracks(ctx, p.UID, p.Kind)
		if err != nil {
			return nil, err
		}
		return trackItems(tracks), nil
	}
}

func (m *Model) albumTracksLoader(a api.Album) func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		tracks, err := m.api.AlbumTracks(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return trackItems(tracks), nil
	}
}

func (m *Model) artistTracksLoader(a api.Artist) func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		tracks, err := m.api.ArtistTracks(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		return trackItems(tracks), nil
	}
}

func (m *Model) searchLoader(q string) func() ([]item, error) {
	return func() ([]item, error) {
		ctx, cancel := ctx30()
		defer cancel()
		res, err := m.api.Search(ctx, q)
		if err != nil {
			return nil, err
		}
		var items []item
		if len(res.Tracks.Results) > 0 {
			items = append(items, header("Треки"))
			items = append(items, trackItems(res.Tracks.Results)...)
		}
		if len(res.Artists.Results) > 0 {
			items = append(items, header("Исполнители"))
			for _, a := range res.Artists.Results {
				items = append(items, item{kind: kArtist, artist: a})
			}
		}
		if len(res.Albums.Results) > 0 {
			items = append(items, header("Альбомы"))
			for _, a := range res.Albums.Results {
				items = append(items, item{kind: kAlbum, album: a})
			}
		}
		if len(res.Playlists.Results) > 0 {
			items = append(items, header("Плейлисты"))
			for _, p := range res.Playlists.Results {
				items = append(items, item{kind: kPlaylist, playlist: p})
			}
		}
		if len(items) == 0 {
			items = append(items, item{kind: kInfo, title: fmt.Sprintf("По запросу «%s» ничего не найдено", q)})
		}
		return items, nil
	}
}

func (m *Model) cacheLoader() func() ([]item, error) {
	return func() ([]item, error) {
		entries := m.cache.List()
		items := make([]item, 0, len(entries)+1)
		if len(entries) == 0 {
			items = append(items, item{kind: kInfo, title: "Кэш пуст: треки сохраняются сюда при прослушивании"})
		}
		for i := range entries {
			e := entries[i]
			items = append(items, item{kind: kTrack, track: e.Track, cached: &e})
		}
		return items, nil
	}
}

// loadLikes загружает «Мне нравится» в общее хранилище (для отметок ♥ в списках, трее и плеере).
func (m *Model) loadLikes() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := ctx30()
		defer cancel()
		if err := m.likes.Load(ctx); err != nil {
			log.Printf("лайки: %v", err)
		}
		return nil
	}
}

type likeDoneMsg struct {
	title string
	liked bool
	err   error
}

func (m *Model) likeCmd(t api.Track) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := ctx30()
		defer cancel()
		liked, err := m.likes.Toggle(ctx, t)
		return likeDoneMsg{title: t.FullTitle(), liked: liked, err: err}
	}
}

type dislikeDoneMsg struct {
	title string
	err   error
}

func (m *Model) dislikeCmd(t api.Track) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := ctx30()
		defer cancel()
		return dislikeDoneMsg{title: t.FullTitle(), err: m.api.DislikeTrack(ctx, t.Key())}
	}
}
