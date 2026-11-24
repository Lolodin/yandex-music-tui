package stream

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ymusic/internal/api"
)

// Если mpv просит данные дальше скачанного больше чем на это значение (перемотка, поиск moov в конце MP4),
// диапазон берётся с CDN напрямую, не дожидаясь последовательной загрузки.
const directFetchAhead = 1 << 20

// Сколько последних источников держать зарегистрированными. Источники, которые сейчас раздаются
// или докачиваются, не вытесняются, даже если их больше.
const maxSources = 8

// Server раздаёт треки mpv по http://127.0.0.1:<port>/s/<token>.<ext>.
type Server struct {
	api   *api.Client
	cache *Cache
	cdn   *http.Client
	ln    net.Listener
	base  string

	mu      sync.Mutex
	sources map[string]*Source
	order   []string

	offline atomic.Bool
}

func NewServer(client *api.Client, cache *Cache) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		api:     client,
		cache:   cache,
		cdn:     &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 20 * time.Second}},
		ln:      ln,
		base:    "http://" + ln.Addr().String(),
		sources: map[string]*Source{},
	}
	go func() { _ = http.Serve(ln, s) }()
	return s, nil
}

func (s *Server) Close() { s.ln.Close() }

func (s *Server) SetOffline(v bool) { s.offline.Store(v) }
func (s *Server) Offline() bool     { return s.offline.Load() }
func (s *Server) Cache() *Cache     { return s.cache }

// Source — подготовленный к проигрыванию трек.
type Source struct {
	Token   string
	Track   api.Track
	Quality string // фактическое качество файла
	Codec   string
	Bitrate int
	// FromCache — файл с самого начала читается из кэша (без сети).
	FromCache bool

	srv       *Server
	cachePath string
	requested string

	mu     sync.Mutex
	info   *api.DownloadInfo
	infoAt time.Time
	dl     *download

	active atomic.Int32 // открытые HTTP-ответы mpv
}

func (src *Source) URL() string {
	return src.srv.base + "/s/" + src.Token + AudioExt(src.Codec)
}

// Progress — сколько скачано (для индикатора загрузки). done=true, когда файл целиком на диске.
func (src *Source) Progress() (written, size int64, done bool) {
	if src.FromCache {
		return 1, 1, true
	}
	src.mu.Lock()
	d := src.dl
	src.mu.Unlock()
	if d == nil {
		return 0, -1, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.written, d.size, d.done && d.err == nil
}

// Committed — файл сохранён в кэш.
func (src *Source) Committed() bool {
	if src.FromCache {
		return true
	}
	src.mu.Lock()
	d := src.dl
	src.mu.Unlock()
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.committed
}

func newToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Prepare выбирает источник: уже подготовленный для этого трека → кэш нужного качества →
// get-file-info → любой файл из кэша (без сети).
func (s *Server) Prepare(ctx context.Context, t api.Track, quality string) (*Source, error) {
	// Тот же трек уже подготовлен (например, «следующий» после смены настроек или порядка) —
	// отдаём его же: без нового get-file-info и без второй загрузки.
	if src := s.reuse(t.ID, quality); src != nil {
		return src, nil
	}
	cached := func(e Entry) *Source {
		src := s.cachedSource(t, e)
		src.requested = quality
		return s.register(src)
	}
	if e, ok := s.cache.Lookup(t.ID, quality); ok {
		return cached(e), nil
	}
	if s.Offline() {
		if e, ok := s.cache.Lookup(t.ID, ""); ok {
			return cached(e), nil
		}
		return nil, errors.New("офлайн: трека нет в кэше")
	}
	info, err := s.api.FileInfo(ctx, t.ID, quality)
	if err != nil {
		if e, ok := s.cache.Lookup(t.ID, ""); ok {
			log.Printf("file-info %s: %v — играю из кэша", t.ID, err)
			return cached(e), nil
		}
		return nil, err
	}
	// Запросили lossless, а у трека есть только hq, который уже лежит в кэше.
	if e, ok := s.cache.Exact(t.ID, info.Quality); ok && e.Codec == info.Codec {
		return cached(e), nil
	}
	src := &Source{
		Track:     t,
		Quality:   info.Quality,
		Codec:     info.Codec,
		Bitrate:   info.Bitrate,
		srv:       s,
		requested: quality,
		info:      info,
		infoAt:    time.Now(),
	}
	return s.register(src), nil
}

func (s *Server) cachedSource(t api.Track, e Entry) *Source {
	if t.Title == "" {
		t = e.Track
	}
	return &Source{
		Track:     t,
		Quality:   e.Quality,
		Codec:     e.Codec,
		Bitrate:   e.Bitrate,
		FromCache: true,
		srv:       s,
		cachePath: e.Path,
	}
}

func (s *Server) register(src *Source) *Source {
	src.Token = newToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[src.Token] = src
	s.order = append(s.order, src.Token)
	s.evict()
	return src
}

// evict убирает самые старые источники сверх maxSources. Источник, который сейчас раздаётся mpv
// или ещё докачивается, не трогается: иначе оборвался бы играющий трек. Только что
// зарегистрированный (последний) не вытесняется никогда.
func (s *Server) evict() {
	for i := 0; len(s.order) > maxSources && i < len(s.order)-1; {
		tok := s.order[i]
		old := s.sources[tok]
		if old != nil && old.busy() {
			i++
			continue
		}
		delete(s.sources, tok)
		s.order = append(s.order[:i], s.order[i+1:]...)
		if old != nil {
			old.release()
		}
	}
}

// touch переносит источник в конец очереди вытеснения (самый свежий).
func (s *Server) touch(i int) {
	tok := s.order[i]
	s.order = append(append(s.order[:i], s.order[i+1:]...), tok)
}

// reuse — уже зарегистрированный рабочий источник этого трека в том же запрошенном качестве.
func (s *Server) reuse(id api.ID, quality string) *Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.order) - 1; i >= 0; i-- {
		src := s.sources[s.order[i]]
		if src == nil || src.Track.ID != id || src.requested != quality || !src.usable() {
			continue
		}
		s.touch(i)
		return src
	}
	return nil
}

// Keep отмечает источники как нужные сейчас (играющий и следующий трек), чтобы их не вытеснили
// новые подготовки, даже когда mpv уже прочитал файл целиком и соединений нет.
func (s *Server) Keep(srcs ...*Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range srcs {
		if src == nil {
			continue
		}
		for i, tok := range s.order {
			if tok == src.Token {
				s.touch(i)
				break
			}
		}
	}
}

// busy — источник сейчас раздаётся или его файл ещё докачивается.
func (src *Source) busy() bool {
	if src.active.Load() > 0 {
		return true
	}
	src.mu.Lock()
	d := src.dl
	src.mu.Unlock()
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.done
}

// usable — источник можно отдать повторно: файл кэша на месте, загрузка не провалилась.
func (src *Source) usable() bool {
	if src.FromCache {
		_, err := os.Stat(src.cachePath)
		return err == nil
	}
	src.mu.Lock()
	d := src.dl
	src.mu.Unlock()
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err == nil
}

func (src *Source) release() {
	src.mu.Lock()
	d := src.dl
	src.mu.Unlock()
	if d != nil {
		d.cancel()
		d.mu.Lock()
		committed, p := d.committed, d.path
		d.mu.Unlock()
		if !committed && p != "" {
			// Подождём, пока горутина загрузки отпустит файл, и удалим временный файл.
			go func() {
				<-d.finished
				os.Remove(p)
			}()
		}
	}
}

// downloadInfo возвращает ссылку на файл; refresh — перезапросить (ссылка протухла).
func (src *Source) downloadInfo(ctx context.Context, refresh bool) (*api.DownloadInfo, error) {
	src.mu.Lock()
	info, at := src.info, src.infoAt
	src.mu.Unlock()
	if info != nil && !refresh && time.Since(at) < 10*time.Minute {
		return info, nil
	}
	fresh, err := src.srv.api.FileInfo(ctx, src.Track.ID, src.requested)
	if err != nil {
		return nil, err
	}
	src.mu.Lock()
	src.info, src.infoAt = fresh, time.Now()
	src.mu.Unlock()
	return fresh, nil
}

func (src *Source) ensureDownload() *download {
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.dl == nil {
		ctx, cancel := context.WithCancel(context.Background())
		src.dl = &download{src: src, size: -1, notify: make(chan struct{}), cancel: cancel, finished: make(chan struct{})}
		go src.dl.run(ctx)
	}
	return src.dl
}

// ctrAt — AES-128-CTR с нулевым начальным счётчиком, сдвинутый на offset байт (для Range-запросов).
func ctrAt(keyHex string, offset int64) (cipher.Stream, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("ключ расшифровки: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/aes.BlockSize))
	stream := cipher.NewCTR(block, iv)
	if skip := offset % aes.BlockSize; skip > 0 {
		tmp := make([]byte, skip)
		stream.XORKeyStream(tmp, tmp)
	}
	return stream, nil
}

func isExpired(status int) bool {
	return status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone || status == http.StatusUnauthorized
}

// openRange открывает поток с CDN с позиции offset, уже расшифрованный.
// Возвращает тело, полный размер файла и признак того, что сервер проигнорировал Range.
func (src *Source) openRange(ctx context.Context, offset, end int64) (io.ReadCloser, int64, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		info, err := src.downloadInfo(ctx, attempt > 0)
		if err != nil {
			return nil, 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
		if err != nil {
			return nil, 0, err
		}
		if offset > 0 || end >= 0 {
			rng := fmt.Sprintf("bytes=%d-", offset)
			if end >= 0 {
				rng += strconv.FormatInt(end, 10)
			}
			req.Header.Set("Range", rng)
		}
		resp, err := src.srv.cdn.Do(req)
		if err != nil {
			return nil, 0, err
		}
		if isExpired(resp.StatusCode) {
			resp.Body.Close()
			lastErr = fmt.Errorf("CDN %d", resp.StatusCode)
			continue
		}
		var total int64 = -1
		switch resp.StatusCode {
		case http.StatusOK:
			if offset > 0 {
				resp.Body.Close()
				return nil, 0, errors.New("CDN не поддерживает Range")
			}
			total = resp.ContentLength
		case http.StatusPartialContent:
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				if i := strings.LastIndexByte(cr, '/'); i >= 0 {
					total, _ = strconv.ParseInt(cr[i+1:], 10, 64)
				}
			}
		default:
			resp.Body.Close()
			return nil, 0, fmt.Errorf("CDN %d", resp.StatusCode)
		}
		if !info.Encrypted() {
			return resp.Body, total, nil
		}
		stream, err := ctrAt(info.Key, offset)
		if err != nil {
			resp.Body.Close()
			return nil, 0, err
		}
		return readCloser{cipher.StreamReader{S: stream, R: resp.Body}, resp.Body}, total, nil
	}
	return nil, 0, lastErr
}

type readCloser struct {
	io.Reader
	io.Closer
}

// download — последовательная загрузка всего файла во временный файл (и затем в кэш).
type download struct {
	src    *Source
	cancel context.CancelFunc

	mu        sync.Mutex
	notify    chan struct{}
	size      int64
	written   int64
	done      bool
	committed bool
	err       error
	path      string
	finished  chan struct{}
}

func (d *download) broadcast() {
	close(d.notify)
	d.notify = make(chan struct{})
}

func (d *download) fail(err error) {
	d.mu.Lock()
	d.err = err
	d.done = true
	d.broadcast()
	d.mu.Unlock()
}

func (d *download) run(ctx context.Context) {
	defer close(d.finished)
	src := d.src
	f, err := src.srv.cache.TempFile(src.Track.ID)
	if err != nil {
		d.fail(err)
		return
	}
	d.mu.Lock()
	d.path = f.Name()
	d.mu.Unlock()

	buf := make([]byte, 128<<10)
	var written int64
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil {
			break
		}
		body, total, err := src.openRange(ctx, written, -1)
		if err != nil {
			lastErr = err
			log.Printf("загрузка %s: %v (попытка %d)", src.Track.ID, err, attempt+1)
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
			continue
		}
		d.mu.Lock()
		if total > 0 {
			d.size = total
		}
		d.broadcast()
		d.mu.Unlock()
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					body.Close()
					f.Close()
					d.fail(werr)
					return
				}
				written += int64(n)
				d.mu.Lock()
				d.written = written
				d.broadcast()
				d.mu.Unlock()
			}
			if rerr == io.EOF {
				body.Close()
				d.finish(f, written)
				return
			}
			if rerr != nil {
				lastErr = rerr
				body.Close()
				break
			}
		}
	}
	f.Close()
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	d.fail(fmt.Errorf("загрузка %s: %w", src.Track.ID, lastErr))
}

func (d *download) finish(f *os.File, written int64) {
	src := d.src
	if err := f.Close(); err != nil {
		d.fail(err)
		return
	}
	d.mu.Lock()
	if d.size < 0 {
		d.size = written
	}
	size, p := d.size, d.path
	d.mu.Unlock()
	if written != size {
		d.fail(fmt.Errorf("загрузка %s: получено %d из %d байт", src.Track.ID, written, size))
		return
	}
	final, err := src.srv.cache.Commit(p, Meta{
		Track: src.Track, Quality: src.Quality, Codec: src.Codec, Bitrate: src.Bitrate, Size: size,
	})
	if err != nil {
		log.Printf("кэш %s: %v", src.Track.ID, err)
	}
	d.mu.Lock()
	if final != "" {
		d.path = final
		d.committed = true
	}
	d.done = true
	d.broadcast()
	d.mu.Unlock()
}

// waitSize ждёт заголовков ответа CDN (размер файла).
func (d *download) waitSize(ctx context.Context) (int64, error) {
	for {
		d.mu.Lock()
		size, err, ch := d.size, d.err, d.notify
		d.mu.Unlock()
		if size >= 0 {
			return size, nil
		}
		if err != nil {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ch:
		}
	}
}

func contentType(codec string) string {
	switch {
	case codec == "flac":
		return "audio/flac"
	case strings.HasSuffix(codec, "-mp4"):
		return "audio/mp4"
	case codec == "mp3":
		return "audio/mpeg"
	case strings.Contains(codec, "aac"):
		return "audio/aac"
	}
	return "application/octet-stream"
}

// parseRange разбирает одиночный диапазон "bytes=a-b". ok=false — диапазон некорректен.
func parseRange(h string, size int64) (start, end int64, partial, ok bool) {
	if h == "" {
		return 0, size - 1, false, true
	}
	if !strings.HasPrefix(h, "bytes=") || strings.Contains(h, ",") {
		return 0, 0, false, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, false
	}
	var err error
	switch {
	case a == "": // последние b байт
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		start, end = max(size-n, 0), size-1
	default:
		start, err = strconv.ParseInt(a, 10, 64)
		if err != nil {
			return 0, 0, false, false
		}
		end = size - 1
		if b != "" {
			if end, err = strconv.ParseInt(b, 10, 64); err != nil {
				return 0, 0, false, false
			}
			end = min(end, size-1)
		}
	}
	if start < 0 || start >= size || end < start {
		return 0, 0, false, false
	}
	return start, end, true, true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	token := strings.TrimSuffix(name, path.Ext(name))
	s.mu.Lock()
	src := s.sources[token]
	s.mu.Unlock()
	if src == nil || !strings.HasPrefix(r.URL.Path, "/s/") {
		http.NotFound(w, r)
		return
	}
	src.active.Add(1)
	defer src.active.Add(-1)
	if src.FromCache {
		s.cache.Touch(src.cachePath)
		w.Header().Set("Content-Type", contentType(src.Codec))
		http.ServeFile(w, r, src.cachePath)
		return
	}
	d := src.ensureDownload()
	size, err := d.waitSize(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	start, end, partial, ok := parseRange(r.Header.Get("Range"), size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentType(src.Codec))
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	if err := s.serveBody(r.Context(), w, src, d, start, end); err != nil && r.Context().Err() == nil {
		log.Printf("раздача %s: %v", src.Track.ID, err)
	}
}

// serveBody отдаёт [start, end] из растущего файла; далёкие диапазоны — напрямую с CDN.
func (s *Server) serveBody(ctx context.Context, w io.Writer, src *Source, d *download, start, end int64) error {
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	buf := make([]byte, 128<<10)
	pos := start
	for pos <= end {
		d.mu.Lock()
		avail, done, derr, p, ch := d.written, d.done, d.err, d.path, d.notify
		d.mu.Unlock()
		if pos < avail {
			if f == nil {
				var err error
				if f, err = os.Open(p); err != nil {
					// Файл могли только что перенести в кэш — возьмём новый путь.
					d.mu.Lock()
					p = d.path
					d.mu.Unlock()
					if f, err = os.Open(p); err != nil {
						return err
					}
				}
			}
			n := min(avail, end+1) - pos
			for n > 0 {
				chunk := buf[:min(int64(len(buf)), n)]
				m, err := f.ReadAt(chunk, pos)
				if m > 0 {
					if _, werr := w.Write(chunk[:m]); werr != nil {
						return werr
					}
					pos += int64(m)
					n -= int64(m)
				}
				if err != nil && err != io.EOF {
					return err
				}
				if m == 0 {
					break
				}
			}
			continue
		}
		if done {
			if derr != nil {
				return derr
			}
			return nil
		}
		if pos-avail > directFetchAhead {
			body, _, err := src.openRange(ctx, pos, end)
			if err != nil {
				return err
			}
			defer body.Close()
			_, err = io.CopyBuffer(w, body, buf)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
	return nil
}
