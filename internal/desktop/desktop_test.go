package desktop

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"ymusic/internal/api"
	"ymusic/internal/config"
	"ymusic/internal/likes"
	"ymusic/internal/player"
	"ymusic/internal/stream"
	"ymusic/internal/testutil"
)

// TestDesktopDBus — сквозная проверка на настоящей сессионной шине: MPRIS-клиент и клики по меню трея
// (так же, как это делают GNOME Shell и медиаклавиши). Звук — в ao=null.
// Тест обращается только к своему экземпляру: если параллельно запущен настоящий ymusic, общее имя
// org.mpris.MediaPlayer2.ymusic занято им, а тестовый получает имя с суффиксом .instance<pid>.
func TestDesktopDBus(t *testing.T) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("нет сессионной шины D-Bus")
	}
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv не установлен")
	}
	var tracks []api.Track
	for i, title := range []string{"Первый", "Второй", "Третий"} {
		tracks = append(tracks, api.Track{
			ID: api.ID(string(rune('1' + i))), Title: title, DurationMs: 30000,
			Artists: []api.Artist{{Name: "Тестовый артист"}}, Albums: []api.Album{{ID: "77", Title: "Альбом"}},
		})
	}
	cache := testutil.Cache(t, tracks, 30)
	srv, err := stream.NewServer(nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.SetOffline(true)
	eng, err := player.New(config.Audio{
		MpvPath: "mpv", Device: "auto", Transition: config.TransitionGapless, CrossfadeSec: 6,
		Volume: 80, ExtraArgs: []string{"--ao=null"},
	}, "lossless", nil, srv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	quit := make(chan struct{}, 4)
	c := start(eng, likes.New(nil), func() { quit <- struct{}{} }, Options{Tray: true, MPRIS: true, Online: false})
	if c.mprisName == "" {
		t.Fatal("MPRIS не опубликован на шине")
	}
	t.Logf("MPRIS тестового экземпляра: %s", c.mprisName)
	eng.PlayTracks(tracks, 0, "тест")

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mp := conn.Object(c.mprisName, "/org/mpris/MediaPlayer2")

	prop := func(name string) any {
		v, err := mp.GetProperty("org.mpris.MediaPlayer2.Player." + name)
		if err != nil {
			return nil
		}
		return v.Value()
	}
	title := func() string {
		md, _ := prop("Metadata").(map[string]dbus.Variant)
		s, _ := md["xesam:title"].Value().(string)
		return s
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("не дождались: %s (status=%v title=%q)", what, prop("PlaybackStatus"), title())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	call := func(method string, args ...any) {
		t.Helper()
		if err := mp.Call("org.mpris.MediaPlayer2.Player."+method, 0, args...).Err; err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	// --- MPRIS ---
	waitFor("Playing «Первый»", func() bool { return prop("PlaybackStatus") == "Playing" && title() == "Первый" })
	md, _ := prop("Metadata").(map[string]dbus.Variant)
	if a, _ := md["xesam:artist"].Value().([]string); len(a) != 1 || a[0] != "Тестовый артист" {
		t.Fatalf("xesam:artist = %v", md["xesam:artist"])
	}
	call("PlayPause")
	waitFor("Paused", func() bool { return prop("PlaybackStatus") == "Paused" })
	call("Play")
	waitFor("Playing после Play", func() bool { return prop("PlaybackStatus") == "Playing" })
	call("Next")
	waitFor("«Второй» после Next", func() bool { return title() == "Второй" })
	call("Seek", int64(10_000_000))
	waitFor("позиция ≥ 10 с после Seek", func() bool { p, _ := prop("Position").(int64); return p >= 10_000_000 })
	if err := mp.SetProperty("org.mpris.MediaPlayer2.Player.Volume", dbus.MakeVariant(0.3)); err != nil {
		t.Fatal(err)
	}
	waitFor("громкость 30", func() bool { return eng.Volume() == 30 })
	if err := mp.SetProperty("org.mpris.MediaPlayer2.Player.LoopStatus", dbus.MakeVariant("Playlist")); err != nil {
		t.Fatal(err)
	}
	waitFor("LoopStatus=Playlist", func() bool { return prop("LoopStatus") == "Playlist" })
	call("Previous") // позиция > 3 с — перемотка в начало того же трека
	waitFor("начало «Второй»", func() bool { p, _ := prop("Position").(int64); return title() == "Второй" && p < 3_000_000 })

	// --- Трей: элемент зарегистрирован у StatusNotifierWatcher, меню отвечает и кликается ---
	sb, _ := dbus.SessionBus()
	unique := sb.Names()[0]
	watcher := conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher")
	waitFor("регистрация в StatusNotifierWatcher", func() bool {
		v, err := watcher.GetProperty("org.kde.StatusNotifierWatcher.RegisteredStatusNotifierItems")
		if err != nil {
			t.Skipf("в сессии нет StatusNotifierWatcher (трей не поддерживается): %v", err)
		}
		items, _ := v.Value().([]string)
		for _, it := range items {
			if strings.HasPrefix(it, unique) {
				return true
			}
		}
		return false
	})

	menu := conn.Object(unique, "/StatusNotifierMenu")
	labels := func() map[string]int32 {
		var rev uint32
		var layout struct {
			ID       int32
			Props    map[string]dbus.Variant
			Children []dbus.Variant
		}
		if err := menu.Call("com.canonical.dbusmenu.GetLayout", 0, int32(0), int32(-1), []string{}).Store(&rev, &layout); err != nil {
			t.Fatalf("GetLayout: %v", err)
		}
		out := map[string]int32{}
		for _, ch := range layout.Children {
			f, _ := ch.Value().([]any)
			if len(f) < 2 {
				continue
			}
			id, _ := f[0].(int32)
			props, _ := f[1].(map[string]dbus.Variant)
			if l, ok := props["label"].Value().(string); ok {
				out[l] = id
			}
		}
		return out
	}
	click := func(label string) {
		t.Helper()
		id, ok := labels()[label]
		if !ok {
			t.Fatalf("в меню нет пункта %q: %v", label, labels())
		}
		err := menu.Call("com.canonical.dbusmenu.Event", 0, id, "clicked", dbus.MakeVariant(""), uint32(time.Now().Unix())).Err
		if err != nil {
			t.Fatalf("клик %q: %v", label, err)
		}
	}
	waitFor("название трека в меню", func() bool { _, ok := labels()["Тестовый артист — Второй"]; return ok })
	t.Logf("меню трея: %v", labels())
	click("⏭  Вперёд")
	waitFor("«Третий» после клика в трее", func() bool { return title() == "Третий" })
	click("⏸  Пауза")
	waitFor("пауза из трея", func() bool { return prop("PlaybackStatus") == "Paused" })
	waitFor("пункт «Продолжить»", func() bool { _, ok := labels()["▶  Продолжить"]; return ok })
	click("▶  Продолжить")
	waitFor("продолжение из трея", func() bool { return prop("PlaybackStatus") == "Playing" })
	// «Назад» после 3 секунд трека — в начало трека, в первые 3 секунды — на предыдущий.
	call("SetPosition", trackPath("3"), int64(8_000_000))
	waitFor("позиция 8 с", func() bool { p, _ := prop("Position").(int64); return p >= 8_000_000 })
	click("⏮  Назад")
	waitFor("начало «Третий» после «Назад»", func() bool { p, _ := prop("Position").(int64); return title() == "Третий" && p < 3_000_000 })
	click("⏮  Назад")
	waitFor("«Второй» после второго «Назад»", func() bool { return title() == "Второй" })
	click("Выход")
	select {
	case <-quit:
	case <-time.After(3 * time.Second):
		t.Fatal("«Выход» не вызвал завершение")
	}
}
