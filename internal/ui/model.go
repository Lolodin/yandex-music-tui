// Package ui — терминальный интерфейс на bubbletea.
package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"ymusic/internal/api"
	"ymusic/internal/config"
	"ymusic/internal/likes"
	"ymusic/internal/mpv"
	"ymusic/internal/player"
	"ymusic/internal/stream"
)

type tab int

const (
	tabWave tab = iota
	tabQueue
	tabLikes
	tabPlaylists
	tabAlbums
	tabSearch
	tabCache
	tabSettings
	numTabs
)

var tabNames = [numTabs]string{"Волна", "Очередь", "Мне нравится", "Плейлисты", "Альбомы", "Поиск", "Кэш", "Настройки"}

type Model struct {
	cfg     *config.Config
	api     *api.Client
	eng     *player.Engine
	cache   *stream.Cache
	account *api.AccountStatus

	tab      tab
	stacks   [numTabs][]*view
	st       player.State
	likes    *likes.Store
	likesVer int
	devices  []mpv.Device
	search   textinput.Model
	typing   bool
	showHelp bool
	width    int
	height   int
	nextID   int
	queueVer int
	queueCur api.ID
}

// New — account == nil означает офлайн-режим.
func New(cfg *config.Config, client *api.Client, eng *player.Engine, cache *stream.Cache, account *api.AccountStatus, likesStore *likes.Store) *Model {
	ti := textinput.New()
	ti.Placeholder = "трек, исполнитель, альбом…"
	ti.Prompt = "🔎 "
	ti.CharLimit = 200
	m := &Model{
		cfg:     cfg,
		api:     client,
		eng:     eng,
		cache:   cache,
		account: account,
		likes:   likesStore,
		search:  ti,
	}
	m.stacks[tabWave] = []*view{m.newView("Моя волна", nil)}
	m.stacks[tabWave][0].items = waveItems()
	m.stacks[tabQueue] = []*view{m.newView("Очередь", nil)}
	m.stacks[tabLikes] = []*view{m.newView("Мне нравится", m.likesLoader())}
	m.stacks[tabPlaylists] = []*view{m.newView("Плейлисты", m.playlistsLoader())}
	m.stacks[tabAlbums] = []*view{m.newView("Альбомы", m.albumsLoader())}
	m.stacks[tabSearch] = []*view{m.newView("Поиск", nil)}
	m.stacks[tabCache] = []*view{m.newView("Кэш", m.cacheLoader())}
	m.stacks[tabSettings] = []*view{m.newView("Настройки", nil)}
	m.refreshSettings()
	if account == nil {
		m.tab = tabCache
	}
	m.fixCursor(m.stacks[tabWave][0])
	return m
}

func (m *Model) newView(title string, load func() ([]item, error)) *view {
	m.nextID++
	return &view{id: m.nextID, title: title, load: load}
}

func (m *Model) offline() bool { return m.account == nil }

func (m *Model) cur() *view {
	s := m.stacks[m.tab]
	return s[len(s)-1]
}

func (m *Model) findView(id int) *view {
	for _, s := range m.stacks {
		for _, v := range s {
			if v.id == id {
				return v
			}
		}
	}
	return nil
}

type stateMsg player.State
type tickMsg time.Time

func (m *Model) waitState() tea.Cmd {
	ch := m.eng.Updates()
	return func() tea.Msg { return stateMsg(<-ch) }
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.waitState(), tick(), m.loadDevices()}
	if !m.offline() {
		cmds = append(cmds, m.loadLikes())
	} else {
		cmds = append(cmds, loadCmd(m.stacks[tabCache][0]))
	}
	return tea.Batch(cmds...)
}

// activateTab переключает вкладку и загружает её содержимое при первом заходе.
func (m *Model) activateTab(t tab) tea.Cmd {
	m.tab = t
	m.typing = false
	m.search.Blur()
	v := m.cur()
	switch t {
	case tabQueue:
		m.refreshQueue(true)
		return nil
	case tabSettings:
		m.refreshSettings()
		return m.loadDevices()
	case tabCache:
		return loadCmd(v)
	case tabSearch:
		if len(m.stacks[tabSearch]) == 1 && len(v.items) == 0 {
			m.typing = true
			m.search.Focus()
		}
	}
	if v.load != nil && v.items == nil && !v.loading && v.err == "" {
		if m.offline() {
			v.err = "офлайн-режим: доступен только кэш"
			return nil
		}
		return loadCmd(v)
	}
	return nil
}

func (m *Model) refreshSettings() {
	v := m.stacks[tabSettings][0]
	v.items = m.settingsItems()
	m.fixCursor(v)
}

func (m *Model) refreshQueue(follow bool) {
	tracks, pos, source := m.eng.Queue()
	v := m.stacks[tabQueue][0]
	v.items = trackItems(tracks)
	if source != "" {
		v.title = "Очередь · " + source
	}
	if len(v.items) == 0 {
		v.items = []item{{kind: kInfo, title: "Очередь пуста — выберите трек, плейлист или волну"}}
	}
	if follow && pos < len(tracks) {
		v.cursor = pos
	}
	m.fixCursor(v)
}

func (m *Model) push(title string, load func() ([]item, error)) tea.Cmd {
	v := m.newView(title, load)
	m.stacks[m.tab] = append(m.stacks[m.tab], v)
	return loadCmd(v)
}

func (m *Model) pop() {
	if len(m.stacks[m.tab]) > 1 {
		m.stacks[m.tab] = m.stacks[m.tab][:len(m.stacks[m.tab])-1]
	}
}

func (m *Model) fixCursor(v *view) {
	if len(v.items) == 0 {
		v.cursor, v.offset = 0, 0
		return
	}
	v.cursor = max(0, min(v.cursor, len(v.items)-1))
	if !v.items[v.cursor].selectable() {
		m.moveCursor(v, 1)
		if !v.items[v.cursor].selectable() {
			m.moveCursor(v, -1)
		}
	}
}

// moveCursor двигает курсор, перепрыгивая заголовки.
func (m *Model) moveCursor(v *view, delta int) {
	n := len(v.items)
	if n == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	target := max(0, min(n-1, v.cursor+delta))
	for i := target; i >= 0 && i < n; i += step {
		if v.items[i].selectable() {
			v.cursor = i
			return
		}
	}
	for i := target; i >= 0 && i < n; i -= step {
		if v.items[i].selectable() {
			v.cursor = i
			return
		}
	}
}

func (m *Model) selected() (item, bool) {
	v := m.cur()
	if v.cursor < 0 || v.cursor >= len(v.items) {
		return item{}, false
	}
	it := v.items[v.cursor]
	return it, it.selectable()
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.search.Width = max(10, msg.Width-6)
		return m, nil

	case stateMsg:
		prev := m.st
		m.st = player.State(msg)
		if m.st.QueueVer != m.queueVer || m.st.Track.ID != m.queueCur {
			follow := m.st.Track.ID != prev.Track.ID
			m.queueVer, m.queueCur = m.st.QueueVer, m.st.Track.ID
			m.refreshQueue(follow && m.tab != tabQueue)
		}
		return m, m.waitState()

	case tickMsg:
		// Лайк могли поставить из трея — список «Мне нравится» перечитается при следующем заходе.
		if v := m.likes.Version(); v != m.likesVer {
			m.likesVer = v
			if lv := m.stacks[tabLikes][0]; !lv.loading && !(m.tab == tabLikes && len(m.stacks[tabLikes]) == 1) {
				lv.items = nil
			}
		}
		return m, tick()

	case viewLoadedMsg:
		v := m.findView(msg.id)
		if v == nil {
			return m, nil
		}
		v.loading = false
		if msg.err != nil {
			v.err = msg.err.Error()
			if api.IsNetwork(msg.err) {
				v.err = "нет сети: " + v.err
			}
			return m, nil
		}
		v.items = msg.items
		if v.items == nil {
			v.items = []item{}
		}
		if len(v.items) == 0 {
			v.items = []item{{kind: kInfo, title: "Пусто"}}
		}
		m.fixCursor(v)
		return m, nil

	case likeDoneMsg:
		if msg.err != nil {
			m.eng.Notify("Лайк не сохранён: "+msg.err.Error(), true)
			return m, nil
		}
		if msg.liked {
			m.eng.Notify("♥ "+msg.title, false)
		} else {
			m.eng.Notify("Убрано из «Мне нравится»: "+msg.title, false)
		}
		// Список «Мне нравится» перезагрузится при следующем заходе.
		lv := m.stacks[tabLikes][0]
		if !lv.loading {
			lv.items = nil
		}
		return m, nil

	case dislikeDoneMsg:
		if msg.err != nil {
			m.eng.Notify("Дизлайк не сохранён: "+msg.err.Error(), true)
		} else {
			m.eng.Notify("Больше не будет звучать: "+msg.title, false)
		}
		return m, nil

	case devicesMsg:
		m.devices = msg
		if m.tab == tabSettings {
			m.refreshSettings()
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	if m.typing {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) quit() tea.Cmd {
	return tea.Quit
}

// SaveConfig сохраняет настройки (громкость, качество) при выходе.
func (m *Model) SaveConfig() { m.saveConfig() }

func (m *Model) handleKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return m.quit()
	}
	if m.showHelp {
		m.showHelp = false
		return nil
	}

	// Ввод поискового запроса.
	if m.typing {
		switch key {
		case "esc":
			m.typing = false
			m.search.Blur()
			return nil
		case "enter":
			q := strings.TrimSpace(m.search.Value())
			m.typing = false
			m.search.Blur()
			if q == "" {
				return nil
			}
			if m.offline() {
				m.eng.Notify("Поиск недоступен офлайн", true)
				return nil
			}
			m.stacks[tabSearch] = m.stacks[tabSearch][:1]
			v := m.stacks[tabSearch][0]
			v.title = "Поиск: " + q
			v.load = m.searchLoader(q)
			v.items, v.cursor, v.offset = nil, 0, 0
			return loadCmd(v)
		case "tab", "shift+tab":
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(k)
			return cmd
		}
	}

	v := m.cur()
	page := max(1, m.listHeight()-1)
	switch key {
	case "q":
		return m.quit()
	case "?":
		m.showHelp = true
	case "tab":
		return m.activateTab((m.tab + 1) % numTabs)
	case "shift+tab":
		return m.activateTab((m.tab + numTabs - 1) % numTabs)
	case "1", "2", "3", "4", "5", "6", "7", "8":
		return m.activateTab(tab(key[0] - '1'))
	case "/":
		cmd := m.activateTab(tabSearch)
		m.typing = true
		m.search.Focus()
		return cmd
	case "i":
		if m.tab == tabSearch {
			m.typing = true
			m.search.Focus()
		}

	// Навигация.
	case "up", "k":
		m.moveCursor(v, -1)
	case "down", "j":
		m.moveCursor(v, 1)
	case "pgup", "ctrl+b":
		m.moveCursor(v, -page)
	case "pgdown", "ctrl+f":
		m.moveCursor(v, page)
	case "home", "g":
		v.cursor = 0
		m.fixCursor(v)
	case "end", "G":
		v.cursor = len(v.items) - 1
		m.fixCursor(v)
	case "esc", "backspace", "h":
		m.pop()
	case "R":
		if v.load != nil && !m.offline() || m.tab == tabCache {
			return loadCmd(v)
		}
	case "enter", "o":
		return m.activate()
	case "right", "l":
		if m.tab == tabSettings {
			if it, ok := m.selected(); ok && it.kind == kSetting {
				cmd := m.changeSetting(it.setting, 1)
				m.refreshSettings()
				return cmd
			}
		}
		if key == "right" {
			m.eng.Seek(5)
		} else {
			return m.toggleLikeCurrent()
		}
	case "left":
		if m.tab == tabSettings {
			if it, ok := m.selected(); ok && it.kind == kSetting {
				cmd := m.changeSetting(it.setting, -1)
				m.refreshSettings()
				return cmd
			}
		}
		m.eng.Seek(-5)

	// Плеер.
	case " ":
		m.eng.TogglePause()
	case "n", ">":
		m.eng.Next()
	case "p", "<", "b":
		m.eng.Prev()
	case "]", "shift+right":
		m.eng.Seek(30)
	case "[", "shift+left":
		m.eng.Seek(-30)
	case "+", "=":
		m.eng.ChangeVolume(5)
	case "-", "_":
		m.eng.ChangeVolume(-5)
	case "s":
		m.eng.ToggleShuffle()
	case "r":
		m.eng.CycleRepeat()
	case "c":
		m.cfg.Quality = config.NextQuality(m.cfg.Quality)
		m.eng.SetQuality(m.cfg.Quality)
		m.eng.Notify("Качество: "+config.QualityLabel(m.cfg.Quality), false)
		m.saveConfig()
		if m.tab == tabSettings {
			m.refreshSettings()
		}
	case "L":
		if it, ok := m.selected(); ok && it.kind == kTrack && !m.offline() {
			return m.likeCmd(it.track)
		}
	case "d":
		if t, ok := m.eng.CurrentTrack(); ok && !m.offline() {
			m.eng.Next()
			return m.dislikeCmd(t)
		}
	case "w":
		return m.waveFromSelected()
	}
	return nil
}

func (m *Model) toggleLikeCurrent() tea.Cmd {
	t, ok := m.eng.CurrentTrack()
	if !ok || m.offline() {
		return nil
	}
	return m.likeCmd(t)
}

func (m *Model) waveFromSelected() tea.Cmd {
	if m.offline() {
		return nil
	}
	it, ok := m.selected()
	if !ok {
		return nil
	}
	switch it.kind {
	case kTrack:
		m.eng.PlayWave([]string{"track:" + string(it.track.ID)}, "Волна по треку «"+it.track.FullTitle()+"»")
	case kArtist:
		m.eng.PlayWave([]string{"artist:" + string(it.artist.ID)}, "Волна по исполнителю "+it.artist.Name)
	case kAlbum:
		m.eng.PlayWave([]string{"album:" + string(it.album.ID)}, "Волна по альбому «"+it.album.Title+"»")
	}
	return nil
}

// activate — Enter на элементе списка.
func (m *Model) activate() tea.Cmd {
	it, ok := m.selected()
	if !ok {
		return nil
	}
	v := m.cur()
	switch it.kind {
	case kTrack:
		if m.tab == tabQueue {
			m.eng.JumpTo(v.cursor)
			return nil
		}
		if !it.track.IsAvailable() && it.cached == nil {
			m.eng.Notify("Трек недоступен для прослушивания", true)
			return nil
		}
		// Играем все доступные треки текущего списка, начиная с выбранного.
		var tracks []api.Track
		start := 0
		for i, other := range v.items {
			if other.kind != kTrack || (!other.track.IsAvailable() && other.cached == nil) {
				continue
			}
			if i == v.cursor {
				start = len(tracks)
			}
			tracks = append(tracks, other.track)
		}
		if len(tracks) == 0 {
			return nil
		}
		m.eng.PlayTracks(tracks, start, strings.TrimPrefix(v.title, "Поиск: "))
	case kWave:
		m.eng.PlayWave(it.seeds, it.title)
		return m.activateTab(tabQueue)
	case kPlaylist:
		return m.push(it.playlist.Title, m.playlistTracksLoader(it.playlist))
	case kAlbum:
		return m.push(it.album.Title, m.albumTracksLoader(it.album))
	case kArtist:
		return m.push(it.artist.Name, m.artistTracksLoader(it.artist))
	case kSetting:
		cmd := m.changeSetting(it.setting, 1)
		m.refreshSettings()
		return cmd
	}
	return nil
}
