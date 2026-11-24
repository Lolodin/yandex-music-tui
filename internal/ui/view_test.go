package ui

import (
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"ymusic/internal/api"
	"ymusic/internal/config"
	"ymusic/internal/likes"
	"ymusic/internal/player"
	"ymusic/internal/stream"
	"ymusic/internal/testutil"
)

// TestRender — офлайн-сценарий: кэш → Enter → плеер; проверяем, что экран собирается и влезает по ширине.
func TestRender(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv не установлен")
	}
	dir := t.TempDir()
	names := [][2]string{{"Звезда по имени Солнце", "Кино"}, {"Очень длинное название трека, которое не влезет в узкий терминал целиком", "Исполнитель"}, {"Track", "Artist"}}
	var tracks []api.Track
	for i, n := range names {
		tracks = append(tracks, api.Track{ID: api.ID(string(rune('1' + i))), Title: n[0], DurationMs: 20000,
			Artists: []api.Artist{{Name: n[1]}}, Albums: []api.Album{{ID: "9", Title: "Альбом"}}, R128: &api.R128{I: -8.3, TP: 0.4}})
	}
	cache := testutil.Cache(t, tracks, 20)
	srv, _ := stream.NewServer(nil, cache)
	defer srv.Close()
	srv.SetOffline(true)
	cfg, _ := config.Load(filepath.Join(dir, "config.toml"))
	cfg.Audio.ExtraArgs = []string{"--ao=null"}
	eng, err := player.New(cfg.Audio, cfg.Quality, nil, srv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	m := New(cfg, nil, eng, cache, nil, likes.New(nil))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.Update(loadCmd(m.stacks[tabCache][0])())
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := <-eng.Updates()
		m.Update(stateMsg(st))
		if st.Pos > 0.5 && st.Out.SampleRate > 0 {
			break
		}
	}
	check := func(name string) {
		v := m.View()
		lines := strings.Split(v, "\n")
		if len(lines) != 24 {
			t.Errorf("%s: %d строк вместо 24", name, len(lines))
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > 100 {
				t.Errorf("%s: строка %d шириной %d", name, i, w)
			}
		}
		t.Logf("=== %s ===\n%s", name, ansi.Strip(v))
	}
	check("кэш + плеер")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("8")})
	m.Update(devicesMsg(eng.Devices()))
	check("настройки")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	check("волна")
}
