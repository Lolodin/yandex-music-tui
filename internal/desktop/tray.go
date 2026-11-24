package desktop

import (
	"fyne.io/systray"

	"ymusic/internal/api"
	"ymusic/internal/player"
)

func (c *controller) startTray() {
	icons := map[iconKind][]byte{
		iconIdle:    drawIcon(iconIdle),
		iconPlaying: drawIcon(iconPlaying),
		iconPaused:  drawIcon(iconPaused),
	}
	// Иконку и подсказку задаём до старта: библиотека отдаст их при регистрации на шине.
	systray.SetIcon(icons[iconIdle])
	systray.SetTitle("Яндекс Музыка")
	systray.SetTooltip("Яндекс Музыка — ничего не играет")

	start, _ := systray.RunWithExternalLoop(func() { c.trayMenu(icons) }, nil)
	start()
}

func (c *controller) trayMenu(icons map[iconKind][]byte) {
	mTrack := systray.AddMenuItem("Ничего не играет", "")
	mTrack.Disable()
	systray.AddSeparator()
	mPrev := systray.AddMenuItem("⏮  Назад", "Предыдущий трек")
	mPlay := systray.AddMenuItem("⏯  Пауза", "Пауза / продолжить")
	mNext := systray.AddMenuItem("⏭  Вперёд", "Следующий трек")
	systray.AddSeparator()
	mLike := systray.AddMenuItemCheckbox("♥  Нравится", "Добавить в «Мне нравится»", false)
	if !c.online {
		mLike.Disable()
	}
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Закрыть плеер")

	// Библиотека трея отдаёт клик неблокирующей отправкой: если в этот момент никто не ждёт на канале,
	// клик теряется. Поэтому у каждого пункта свой слушатель, который сразу возвращается к ожиданию.
	on := func(item *systray.MenuItem, action func()) {
		go func() {
			for range item.ClickedCh {
				go action()
			}
		}()
	}
	on(mPrev, c.eng.Prev)
	on(mPlay, c.playPause)
	on(mNext, c.eng.Next)
	on(mLike, c.toggleLike)
	on(mQuit, c.quit)

	go c.followTray(icons, mTrack, mPlay, mLike)
}

// followTray обновляет иконку, подсказку и пункты меню по состоянию плеера.
// Трогает их только при изменениях: каждое обновление — сигнал по D-Bus.
func (c *controller) followTray(icons map[iconKind][]byte, mTrack, mPlay, mLike *systray.MenuItem) {
	var (
		lastTrack api.ID
		lastIcon  = iconIdle
		lastPlay  = ""
		lastLiked bool
		hadTrack  bool
	)
	for st := range c.eng.Subscribe() {
		if st.HasTrack != hadTrack || st.Track.ID != lastTrack {
			hadTrack, lastTrack = st.HasTrack, st.Track.ID
			if st.HasTrack {
				label := trackLabel(st.Track)
				mTrack.SetTitle(label)
				systray.SetTooltip(label)
			} else {
				mTrack.SetTitle("Ничего не играет")
				systray.SetTooltip("Яндекс Музыка — ничего не играет")
			}
		}
		icon, play := stateIcon(st)
		if icon != lastIcon {
			lastIcon = icon
			systray.SetIcon(icons[icon])
		}
		if play != lastPlay {
			lastPlay = play
			mPlay.SetTitle(play)
		}
		liked := st.HasTrack && c.likes.Liked(st.Track.ID)
		if liked != lastLiked {
			lastLiked = liked
			if liked {
				mLike.Check()
			} else {
				mLike.Uncheck()
			}
		}
	}
}

// stateIcon — иконка и подпись кнопки воспроизведения для состояния плеера.
func stateIcon(st player.State) (iconKind, string) {
	switch {
	case !st.HasTrack:
		return iconIdle, "▶  Играть"
	case st.Paused:
		return iconPaused, "▶  Продолжить"
	default:
		return iconPlaying, "⏸  Пауза"
	}
}
