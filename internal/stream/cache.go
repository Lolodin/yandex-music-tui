// Package stream — локальный HTTP-прокси для mpv: расшифровка encraw на лету, Range-запросы и кэш на диске.
package stream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ymusic/internal/api"
	"ymusic/internal/config"
)

// Meta — описание файла в кэше; лежит рядом с аудио как <id>.<quality>.json.
type Meta struct {
	Track   api.Track `json:"track"`
	Quality string    `json:"quality"`
	Codec   string    `json:"codec"`
	Bitrate int       `json:"bitrate"`
	Size    int64     `json:"size"`
	SavedAt time.Time `json:"savedAt"`
}

type Entry struct {
	Meta
	Path string
}

type Cache struct {
	dir    string
	tmpDir string

	mu       sync.Mutex
	enabled  bool
	maxBytes int64
}

func NewCache(dir string, enabled bool, maxGB float64) (*Cache, error) {
	c := &Cache{dir: filepath.Join(dir, "tracks"), tmpDir: filepath.Join(dir, "tmp")}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.tmpDir, 0o755); err != nil {
		return nil, err
	}
	// Недокачанные файлы прошлого запуска.
	if parts, _ := filepath.Glob(filepath.Join(c.tmpDir, "*.part")); len(parts) > 0 {
		for _, p := range parts {
			os.Remove(p)
		}
	}
	c.SetLimits(enabled, maxGB)
	return c, nil
}

func (c *Cache) SetLimits(enabled bool, maxGB float64) {
	c.mu.Lock()
	c.enabled = enabled
	c.maxBytes = int64(maxGB * (1 << 30))
	c.mu.Unlock()
	if enabled {
		go c.Evict()
	}
}

func (c *Cache) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

func safeID(id api.ID) string {
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(string(id))
}

func (c *Cache) base(id api.ID, quality string) string {
	return filepath.Join(c.dir, safeID(id)+"."+quality)
}

// AudioExt — расширение файла по кодеку.
func AudioExt(codec string) string {
	switch {
	case codec == "flac":
		return ".flac"
	case strings.HasSuffix(codec, "-mp4"):
		return ".m4a"
	case codec == "mp3":
		return ".mp3"
	case strings.Contains(codec, "aac"):
		return ".aac"
	}
	return ".bin"
}

func (c *Cache) readMeta(jsonPath string) (Entry, bool) {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return Entry{}, false
	}
	var m Meta
	if json.Unmarshal(data, &m) != nil {
		return Entry{}, false
	}
	audio := strings.TrimSuffix(jsonPath, ".json") + AudioExt(m.Codec)
	st, err := os.Stat(audio)
	if err != nil || (m.Size > 0 && st.Size() != m.Size) {
		return Entry{}, false
	}
	return Entry{Meta: m, Path: audio}, true
}

// Lookup — лучший файл трека в кэше с качеством не ниже minQuality ("" — любое).
func (c *Cache) Lookup(id api.ID, minQuality string) (Entry, bool) {
	metas, _ := filepath.Glob(filepath.Join(c.dir, safeID(id)+".*.json"))
	var best Entry
	found := false
	for _, mp := range metas {
		e, ok := c.readMeta(mp)
		if !ok {
			continue
		}
		if minQuality != "" && config.QualityRank(e.Quality) < config.QualityRank(minQuality) {
			continue
		}
		if !found || config.QualityRank(e.Quality) > config.QualityRank(best.Quality) {
			best, found = e, true
		}
	}
	return best, found
}

// Exact — файл ровно этого качества.
func (c *Cache) Exact(id api.ID, quality string) (Entry, bool) {
	return c.readMeta(c.base(id, quality) + ".json")
}

func (c *Cache) TempFile(id api.ID) (*os.File, error) {
	return os.CreateTemp(c.tmpDir, safeID(id)+"-*.part")
}

// Commit переносит докачанный файл в кэш. Если кэш выключен — возвращает "" и файл остаётся временным.
func (c *Cache) Commit(partPath string, m Meta) (string, error) {
	// 30-секундные превью (нет подписки) в кэш не кладём.
	if !c.Enabled() || m.Quality == "preview" {
		return "", nil
	}
	base := c.base(m.Track.ID, m.Quality)
	final := base + AudioExt(m.Codec)
	if err := os.Rename(partPath, final); err != nil {
		return "", err
	}
	m.SavedAt = time.Now()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return final, err
	}
	if err := os.WriteFile(base+".json", data, 0o644); err != nil {
		return final, err
	}
	go c.Evict()
	return final, nil
}

// Touch обновляет mtime: вытеснение идёт по давности последнего прослушивания.
func (c *Cache) Touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// List — все треки в кэше, свежие сверху.
func (c *Cache) List() []Entry {
	metas, _ := filepath.Glob(filepath.Join(c.dir, "*.json"))
	out := make([]Entry, 0, len(metas))
	for _, mp := range metas {
		if e, ok := c.readMeta(mp); ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out
}

// Usage — суммарный размер кэша в байтах.
func (c *Cache) Usage() int64 {
	var total int64
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// Evict удаляет давно не слушанные треки, пока кэш не уложится в лимит.
func (c *Cache) Evict() {
	c.mu.Lock()
	limit := c.maxBytes
	c.mu.Unlock()
	type file struct {
		audio string
		size  int64
		mtime time.Time
	}
	var files []file
	var total int64
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= limit {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if total <= limit {
			break
		}
		if os.Remove(f.audio) == nil {
			total -= f.size
			os.Remove(strings.TrimSuffix(f.audio, filepath.Ext(f.audio)) + ".json")
		}
	}
}
