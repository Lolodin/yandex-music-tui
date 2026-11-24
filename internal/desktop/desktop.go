// Package desktop — интеграция с рабочим столом Linux: иконка в трее (StatusNotifierItem)
// и MPRIS (медиаклавиши, плеер в шторке GNOME/KDE). Всё через D-Bus, без cgo и GTK.
package desktop

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"ymusic/internal/api"
	"ymusic/internal/likes"
	"ymusic/internal/player"
)

type Options struct {
	Tray   bool
	MPRIS  bool
	Online bool // false — офлайн-режим, лайки недоступны
}

type controller struct {
	eng    *player.Engine
	likes  *likes.Store
	quit   func()
	online bool
}

// Start поднимает трей и MPRIS. Если сессионной шины D-Bus нет (SSH, tty), тихо ничего не делает.
// Остановки не требуется: при выходе процесса имена на шине освобождаются сами.
func Start(eng *player.Engine, store *likes.Store, quit func(), opts Options) {
	if !opts.Tray && !opts.MPRIS {
		return
	}
	if _, err := dbus.SessionBus(); err != nil {
		log.Printf("D-Bus недоступен, трей и MPRIS отключены: %v", err)
		return
	}
	c := &controller{eng: eng, likes: store, quit: quit, online: opts.Online}
	if opts.MPRIS {
		if err := c.startMPRIS(); err != nil {
			log.Printf("MPRIS: %v", err)
		}
	}
	if opts.Tray {
		c.startTray()
	}
}

// playPause — пауза/продолжение; если очередь пуста, запускает «Мою волну».
func (c *controller) playPause() {
	if !c.eng.PlayPause() && c.online {
		c.eng.PlayWave([]string{"user:onyourwave"}, "Моя волна")
	}
}

// play — продолжить (MPRIS Play): не ставит на паузу, если уже играет.
func (c *controller) play(st player.State) {
	switch {
	case st.HasTrack:
		c.eng.SetPaused(false)
	default:
		c.playPause()
	}
}

func (c *controller) toggleLike() {
	t, ok := c.eng.CurrentTrack()
	if !ok || !c.online {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		liked, err := c.likes.Toggle(ctx, t)
		switch {
		case err != nil:
			c.eng.Notify("Лайк не сохранён: "+err.Error(), true)
		case liked:
			c.eng.Notify("♥ "+t.FullTitle(), false)
		default:
			c.eng.Notify("Убрано из «Мне нравится»: "+t.FullTitle(), false)
		}
	}()
}

func trackLabel(t api.Track) string {
	s := t.FullTitle()
	if a := t.ArtistNames(); a != "" {
		s = a + " — " + s
	}
	if r := []rune(s); len(r) > 70 {
		s = string(r[:69]) + "…"
	}
	return s
}

// coverURL — ссылка на обложку: coverUri приходит как "avatars.yandex.net/.../%%".
func coverURL(t api.Track) string {
	uri := t.CoverURI
	if uri == "" && len(t.Albums) > 0 {
		uri = t.Albums[0].CoverURI
	}
	if uri == "" {
		return ""
	}
	return "https://" + strings.Replace(uri, "%%", "400x400", 1)
}
