package ui

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"ymusic/internal/config"
	"ymusic/internal/mpv"
)

type settingID int

const (
	setQuality settingID = iota
	setTransition
	setCrossfadeSec
	setSmartCrossfade
	setNormalize
	setTargetLUFS
	setBitPerfect
	setDevice
	setCacheEnabled
	setCacheLimit
)

func onOff(b bool) string {
	if b {
		return "вкл"
	}
	return "выкл"
}

func (m *Model) settingValue(id settingID) string {
	c := m.cfg
	switch id {
	case setQuality:
		return config.QualityLabel(c.Quality)
	case setTransition:
		if c.Audio.BitPerfect {
			return "Gapless (в bit-perfect кроссфейд недоступен)"
		}
		if c.Audio.Transition == config.TransitionCrossfade {
			return "Кроссфейд"
		}
		return "Gapless (без пауз между треками)"
	case setCrossfadeSec:
		return fmt.Sprintf("%.0f с", c.Audio.CrossfadeSec)
	case setSmartCrossfade:
		return onOff(c.Audio.SmartCrossfade)
	case setNormalize:
		if c.Audio.BitPerfect {
			return onOff(c.Audio.Normalize) + " (не действует в bit-perfect)"
		}
		return onOff(c.Audio.Normalize)
	case setTargetLUFS:
		return fmt.Sprintf("%.0f LUFS", c.Audio.TargetLUFS)
	case setBitPerfect:
		return onOff(c.Audio.BitPerfect)
	case setDevice:
		if c.Audio.Device == "auto" {
			return "auto (по умолчанию)"
		}
		for _, d := range m.devices {
			if d.Name == c.Audio.Device {
				return d.Description + "  [" + d.Name + "]"
			}
		}
		return c.Audio.Device
	case setCacheEnabled:
		return onOff(c.Cache.Enabled)
	case setCacheLimit:
		return fmt.Sprintf("%.0f ГБ", c.Cache.MaxGB)
	}
	return ""
}

var settingTitles = map[settingID]string{
	setQuality:        "Качество",
	setTransition:     "Переходы между треками",
	setCrossfadeSec:   "Длительность кроссфейда",
	setSmartCrossfade: "Умный кроссфейд (разметка Яндекса)",
	setNormalize:      "Нормализация громкости (R128)",
	setTargetLUFS:     "Целевая громкость",
	setBitPerfect:     "Bit-perfect вывод",
	setDevice:         "Устройство вывода",
	setCacheEnabled:   "Кэш треков",
	setCacheLimit:     "Лимит кэша",
}

func (m *Model) settingsItems() []item {
	set := func(id settingID) item { return item{kind: kSetting, setting: id} }
	aos := strings.Join(m.eng.AudioOutputs(), ", ")
	items := []item{
		header("Звук"),
		set(setQuality),
		set(setTransition),
		set(setCrossfadeSec),
		set(setSmartCrossfade),
		set(setNormalize),
		set(setTargetLUFS),
		header("Вывод"),
		set(setBitPerfect),
		set(setDevice),
		item{kind: kInfo, title: fmt.Sprintf("mpv: %s · выводы: %s", m.cfg.Audio.MpvPath, aos)},
	}
	if !strings.Contains(","+strings.ReplaceAll(aos, " ", "")+",", ",alsa,") {
		items = append(items, item{kind: kInfo, title: "У этого mpv нет ALSA: для настоящего bit-perfect поставьте mpv из репозитория (apt install mpv) и укажите mpv_path"})
	}
	items = append(items,
		header("Рабочий стол"),
		item{kind: kInfo, title: fmt.Sprintf("Трей: %s · MPRIS (медиаклавиши, шторка GNOME): %s — меняется в секции [desktop] конфига, применяется после перезапуска",
			onOff(m.cfg.Desktop.Tray), onOff(m.cfg.Desktop.MPRIS))},
		header("Кэш"),
		set(setCacheEnabled),
		set(setCacheLimit),
		item{kind: kInfo, title: fmt.Sprintf("Занято: %.2f ГБ · %s", float64(m.cache.Usage())/(1<<30), m.cfg.Cache.Dir)},
		header("Аккаунт"),
	)
	if m.account != nil {
		plus := "нет"
		if m.account.Plus.HasPlus {
			plus = "есть"
		}
		items = append(items, item{kind: kInfo, title: fmt.Sprintf("%s · Плюс: %s", m.account.Account.Login, plus)})
	} else {
		items = append(items, item{kind: kInfo, title: "офлайн-режим: доступен только кэш"})
	}
	items = append(items, item{kind: kInfo, title: "Конфиг: " + m.cfg.Path()})
	return items
}

type devicesMsg []mpv.Device

func (m *Model) loadDevices() tea.Cmd {
	return func() tea.Msg { return devicesMsg(m.eng.Devices()) }
}

// changeSetting меняет значение; dir = +1 / -1.
func (m *Model) changeSetting(id settingID, dir int) tea.Cmd {
	c := m.cfg
	audio := c.Audio
	var cmds []tea.Cmd
	switch id {
	case setQuality:
		if dir > 0 {
			c.Quality = config.NextQuality(c.Quality)
		} else {
			c.Quality = config.NextQuality(config.NextQuality(c.Quality))
		}
		m.eng.SetQuality(c.Quality)
	case setTransition:
		if audio.Transition == config.TransitionCrossfade {
			audio.Transition = config.TransitionGapless
		} else {
			audio.Transition = config.TransitionCrossfade
		}
	case setCrossfadeSec:
		audio.CrossfadeSec = math.Max(1, math.Min(15, audio.CrossfadeSec+float64(dir)))
	case setSmartCrossfade:
		audio.SmartCrossfade = !audio.SmartCrossfade
	case setNormalize:
		audio.Normalize = !audio.Normalize
	case setTargetLUFS:
		audio.TargetLUFS = math.Max(-23, math.Min(-8, audio.TargetLUFS+float64(dir)))
	case setBitPerfect:
		audio.BitPerfect = !audio.BitPerfect
		cmds = append(cmds, m.loadDevices())
	case setDevice:
		names := []string{"auto"}
		for _, d := range m.devices {
			if d.Name != "auto" {
				names = append(names, d.Name)
			}
		}
		idx := 0
		for i, n := range names {
			if n == audio.Device {
				idx = i
			}
		}
		idx = (idx + dir + len(names)) % len(names)
		audio.Device = names[idx]
	case setCacheEnabled:
		c.Cache.Enabled = !c.Cache.Enabled
		m.cache.SetLimits(c.Cache.Enabled, c.Cache.MaxGB)
	case setCacheLimit:
		step := 1.0
		if c.Cache.MaxGB+float64(dir) > 10 {
			step = 5
		}
		c.Cache.MaxGB = math.Max(1, math.Min(500, c.Cache.MaxGB+float64(dir)*step))
		m.cache.SetLimits(c.Cache.Enabled, c.Cache.MaxGB)
	}
	if !reflect.DeepEqual(audio, c.Audio) {
		if err := m.eng.ApplyConfig(audio); err != nil {
			m.eng.Notify("Настройки звука: "+err.Error(), true)
		} else {
			c.Audio = audio
		}
	}
	m.saveConfig()
	return tea.Batch(cmds...)
}

func (m *Model) saveConfig() {
	m.cfg.Audio.Volume = m.eng.Volume()
	if err := m.cfg.Save(); err != nil {
		m.eng.Notify("Не удалось сохранить конфиг: "+err.Error(), true)
	}
}
