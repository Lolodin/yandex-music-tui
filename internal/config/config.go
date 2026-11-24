// Package config хранит настройки клиента в ~/.config/ymusic/config.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Качество потока, которое запрашивается у get-file-info.
const (
	QualityLossless = "lossless"
	QualityHigh     = "hq"
	QualityNormal   = "nq"
)

// Режим перехода между треками.
const (
	TransitionGapless   = "gapless"
	TransitionCrossfade = "crossfade"
)

type Config struct {
	Token   string `toml:"token"`
	Quality string `toml:"quality"`

	Audio   Audio   `toml:"audio"`
	Cache   Cache   `toml:"cache"`
	Desktop Desktop `toml:"desktop"`

	path string
}

type Audio struct {
	// Путь к mpv. Для bit-perfect нужен mpv с ALSA (из репозитория дистрибутива, не snap).
	MpvPath string `toml:"mpv_path"`
	// Устройство вывода в формате mpv (--audio-device), например "alsa/hw:CARD=DAC,DEV=0". "auto" — по умолчанию.
	Device string `toml:"device"`
	// Bit-perfect: эксклюзивный вывод без микшера, громкости, нормализации и кроссфейда.
	BitPerfect bool `toml:"bit_perfect"`
	// Нормализация громкости по данным R128 от Яндекса.
	Normalize  bool    `toml:"normalize"`
	TargetLUFS float64 `toml:"target_lufs"`
	// gapless | crossfade
	Transition   string  `toml:"transition"`
	CrossfadeSec float64 `toml:"crossfade_sec"`
	// Брать точки затухания трека из метаданных Яндекса (fade.outStart / fade.inStart).
	SmartCrossfade bool `toml:"smart_crossfade"`
	Volume         int  `toml:"volume"`
	// Дополнительные аргументы mpv, например ["--audio-buffer=0.5"].
	ExtraArgs []string `toml:"mpv_args"`
}

// Desktop — интеграция с рабочим столом (применяется при запуске).
type Desktop struct {
	// Иконка в системном трее с меню управления.
	Tray bool `toml:"tray"`
	// MPRIS: медиаклавиши и плеер в шторке GNOME/KDE.
	MPRIS bool `toml:"mpris"`
}

type Cache struct {
	Enabled bool    `toml:"enabled"`
	Dir     string  `toml:"dir"`
	MaxGB   float64 `toml:"max_gb"`
}

func defaults() *Config {
	return &Config{
		Quality: QualityLossless,
		Audio: Audio{
			MpvPath:        "mpv",
			Device:         "auto",
			Normalize:      true,
			TargetLUFS:     -14,
			Transition:     TransitionGapless,
			CrossfadeSec:   6,
			SmartCrossfade: true,
			Volume:         80,
		},
		Cache:   Cache{Enabled: true, MaxGB: 5},
		Desktop: Desktop{Tray: true, MPRIS: true},
	}
}

// DefaultPath — ~/.config/ymusic/config.toml.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "ymusic", "config.toml")
}

// DefaultCacheDir — ~/.cache/ymusic.
func DefaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(dir, "ymusic")
}

// Load читает конфиг; отсутствующий файл — не ошибка, возвращаются значения по умолчанию.
func Load(path string) (*Config, error) {
	c := defaults()
	c.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.normalize()
			return c, nil
		}
		return nil, err
	}
	if _, err := toml.Decode(string(data), c); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	c.normalize()
	return c, nil
}

func (c *Config) normalize() {
	switch c.Quality {
	case QualityLossless, QualityHigh, QualityNormal:
	default:
		c.Quality = QualityLossless
	}
	if c.Audio.Transition != TransitionCrossfade {
		c.Audio.Transition = TransitionGapless
	}
	if c.Audio.MpvPath == "" {
		c.Audio.MpvPath = "mpv"
	}
	if c.Audio.Device == "" {
		c.Audio.Device = "auto"
	}
	if c.Audio.CrossfadeSec < 1 || c.Audio.CrossfadeSec > 15 {
		c.Audio.CrossfadeSec = 6
	}
	if c.Audio.TargetLUFS < -30 || c.Audio.TargetLUFS > -5 {
		c.Audio.TargetLUFS = -14
	}
	if c.Audio.Volume < 0 || c.Audio.Volume > 100 {
		c.Audio.Volume = 80
	}
	if c.Cache.Dir == "" {
		c.Cache.Dir = DefaultCacheDir()
	}
	if c.Cache.MaxGB <= 0 {
		c.Cache.MaxGB = 5
	}
}

// Save пишет конфиг с правами 0600: в нём лежит OAuth-токен.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func (c *Config) Path() string { return c.path }

// QualityRank — чем больше, тем лучше качество.
func QualityRank(q string) int {
	switch q {
	case QualityLossless:
		return 3
	case QualityHigh:
		return 2
	case QualityNormal:
		return 1
	}
	return 0
}

// NextQuality — циклическое переключение lossless → hq → nq → lossless.
func NextQuality(q string) string {
	switch q {
	case QualityLossless:
		return QualityHigh
	case QualityHigh:
		return QualityNormal
	}
	return QualityLossless
}

// QualityLabel — подпись для интерфейса.
func QualityLabel(q string) string {
	switch q {
	case QualityLossless:
		return "Lossless (FLAC)"
	case QualityHigh:
		return "Высокое (до 320 kbps)"
	case QualityNormal:
		return "Обычное (экономия трафика)"
	}
	return q
}
