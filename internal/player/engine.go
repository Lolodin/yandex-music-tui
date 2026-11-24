// Package player — движок воспроизведения поверх mpv: очередь, gapless, кроссфейд на двух деках,
// нормализация по R128, bit-perfect и обратная связь для «Моей волны».
package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ymusic/internal/api"
	"ymusic/internal/config"
	"ymusic/internal/mpv"
	"ymusic/internal/stream"
)

type AudioParams struct {
	SampleRate   int    `json:"samplerate"`
	Format       string `json:"format"`
	Channels     string `json:"channels"`
	ChannelCount int    `json:"channel-count"`
}

// State — снимок состояния для интерфейса.
type State struct {
	HasTrack  bool
	Track     api.Track
	Loading   bool
	Paused    bool
	Buffering bool
	Pos, Dur  float64
	Volume    int

	Quality   string // фактическое качество файла
	Requested string // запрошенное качество
	Codec     string
	Bitrate   int
	FromCache bool
	Committed bool
	DLDone    bool
	DLPercent float64 // 0..100, <0 — неизвестно

	SrcFormat    stream.SourceFormat
	HasSrcFormat bool

	GainDB   float64
	HasR128  bool
	LUFS, TP float64
	// NoGain — у mpv нет опции volume-gain (mpv < 0.37), нормализация не применяется.
	NoGain bool

	In, Out AudioParams
	AO      string
	Device  string

	BitPerfect  bool
	PathClean   bool
	Transition  string
	Crossfading bool
	Normalize   bool

	Shuffle  bool
	Repeat   Repeat
	Source   string
	QueuePos int
	QueueLen int
	QueueVer int
	Wave     bool

	Status    string
	StatusErr bool
	StatusAt  time.Time
}

const (
	// maxFailures — после стольких треков подряд, которые не заиграли, воспроизведение останавливается.
	maxFailures = 5
	// progressReset — столько секунд нормального воспроизведения сбрасывают счётчик ошибок.
	progressReset = 3.0
	// nextDebounce — задержка подготовки следующего трека: серия переключений (настройки, перемешивание)
	// схлопывается в одну подготовку.
	nextDebounce = 300 * time.Millisecond
	// aoInitFailed — file_error mpv, когда не открылся аудиовывод. Пропускать треки бессмысленно.
	aoInitFailed = "audio output initialization failed"
)

type slot struct {
	pos      int
	track    api.Track
	src      *stream.Source
	gain     float64
	entryID  int64
	deck     int
	loaded   bool
	startAt  float64
	reported bool
	format   stream.SourceFormat
	hasFmt   bool
	// seekTo — перемотка, которую mpv не принял, пока файл открывался; tick повторяет её до успеха.
	seekTo    float64
	hasSeekTo bool
	seekTries int
}

type deck struct {
	idx int
	m   *mpv.Client
}

type xfade struct {
	from    int
	dur     float64
	elapsed float64
	last    time.Time
}

type wave struct {
	sessionID string
	batchID   string
	played    []string
	fetching  bool
}

type deckEvent struct {
	d      *deck
	ev     mpv.Event
	closed bool
}

type Engine struct {
	api  *api.Client
	srv  *stream.Server
	logw io.Writer

	mu       sync.Mutex
	cfg      config.Audio
	quality  string
	aos      []string
	decks    [2]*deck
	active   int
	queue    Queue
	cur      *slot
	next     *slot
	gen      uint64
	paused   bool
	pos, dur float64
	loading  bool
	buffer   bool
	xf       *xfade
	skipping bool
	in, out  AudioParams
	ao, dev  string
	wave     *wave
	failures int
	tickN    int
	closed   bool
	// nextGen — поколение подготовки следующего трека: dropNext и новый scheduleNext отменяют прежние.
	nextGen uint64
	noGain  bool

	onPanic atomic.Pointer[func(any)]

	status    string
	statusErr bool
	statusAt  time.Time

	events  chan deckEvent
	updates chan State
	subs    []chan State
	quit    chan struct{}
}

func New(cfg config.Audio, quality string, client *api.Client, srv *stream.Server, logw io.Writer) (*Engine, error) {
	aos, err := mpv.AudioOutputs(cfg.MpvPath)
	if err != nil {
		return nil, fmt.Errorf("mpv (%s) не найден или не запускается: %w", cfg.MpvPath, err)
	}
	e := &Engine{
		api:     client,
		srv:     srv,
		logw:    logw,
		cfg:     cfg,
		quality: quality,
		aos:     aos,
		events:  make(chan deckEvent, 256),
		updates: make(chan State, 1),
		quit:    make(chan struct{}),
	}
	e.mu.Lock()
	err = e.startDecks()
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go e.loop()
	return e, nil
}

// Updates — канал снимков состояния (всегда хранит только последний).
func (e *Engine) Updates() <-chan State { return e.updates }

// Subscribe — отдельный канал состояний для ещё одного потребителя (трей, MPRIS).
// Как и Updates, хранит только последний снимок.
func (e *Engine) Subscribe() <-chan State {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := make(chan State, 1)
	e.subs = append(e.subs, ch)
	e.emit()
	return ch
}

func (e *Engine) Close() {
	close(e.quit)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	e.gen++ // фоновые подготовки треков больше ничего не запустят
	e.closeDecks()
}

func (e *Engine) crossfadeMode() bool {
	return e.cfg.Transition == config.TransitionCrossfade && !e.cfg.BitPerfect
}

func (e *Engine) hasAO(name string) bool {
	for _, a := range e.aos {
		if a == name {
			return true
		}
	}
	return false
}

// AudioOutputs — какие выводы поддерживает установленный mpv.
func (e *Engine) AudioOutputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.aos...)
}

func (e *Engine) mpvArgs() []string {
	a := []string{
		"--volume-max=100",
		"--replaygain=no",
		"--cache=yes",
		// Файл и так лежит на диске у локального прокси: большой кэш mpv только раздувает память.
		"--demuxer-max-bytes=8MiB",
		"--demuxer-max-back-bytes=4MiB",
		"--network-timeout=30",
	}
	if e.crossfadeMode() {
		a = append(a, "--gapless-audio=no")
	} else {
		a = append(a, "--gapless-audio=weak", "--prefetch-playlist=yes")
	}
	dev := e.cfg.Device
	if e.cfg.BitPerfect {
		ao := ""
		switch {
		case strings.Contains(dev, "/"):
			ao = dev[:strings.Index(dev, "/")]
		case e.hasAO("alsa"):
			ao = "alsa"
		case e.hasAO("pipewire"):
			ao = "pipewire"
		}
		if ao != "" {
			a = append(a, "--ao="+ao)
		}
		a = append(a,
			"--audio-exclusive=yes",
			"--volume=100",
			"--audio-normalize-downmix=no",
			"--audio-pitch-correction=no",
		)
	} else {
		if strings.Contains(dev, "/") {
			a = append(a, "--ao="+dev[:strings.Index(dev, "/")])
		}
		a = append(a, fmt.Sprintf("--volume=%d", e.cfg.Volume))
	}
	if dev != "" && dev != "auto" {
		a = append(a, "--audio-device="+dev)
	}
	return append(a, e.cfg.ExtraArgs...)
}

func (e *Engine) startDecks() error {
	if e.closed {
		return errors.New("плеер остановлен")
	}
	e.closeDecks()
	n := 1
	if e.crossfadeMode() {
		n = 2
	}
	for i := 0; i < n; i++ {
		m, err := mpv.Start(e.cfg.MpvPath, e.mpvArgs(), e.logw)
		if err != nil {
			e.closeDecks()
			return err
		}
		d := &deck{idx: i, m: m}
		e.decks[i] = d
		go e.forward(d)
		if i == 0 {
			// volume-gain появилась в mpv 0.37; на старом mpv нормализация молча не работала бы.
			var g float64
			e.noGain = m.Get("volume-gain", &g) != nil
			if e.noGain {
				log.Printf("mpv без volume-gain: нормализация недоступна (нужен mpv ≥ 0.37)")
			}
		}
	}
	e.active = 0
	e.xf = nil
	return nil
}

func (e *Engine) closeDecks() {
	for i, d := range e.decks {
		if d != nil {
			e.decks[i] = nil
			d.m.Close()
		}
	}
}

func (e *Engine) ensureDecks() error {
	need := 1
	if e.crossfadeMode() {
		need = 2
	}
	for i := 0; i < need; i++ {
		if e.decks[i] == nil {
			return e.startDecks()
		}
	}
	return nil
}

func (e *Engine) forward(d *deck) {
	for ev := range d.m.Events() {
		select {
		case e.events <- deckEvent{d: d, ev: ev}:
		case <-e.quit:
			return
		}
	}
	select {
	case e.events <- deckEvent{d: d, closed: true}:
	case <-e.quit:
	}
}

func (e *Engine) loop() {
	defer e.recoverPanic()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.quit:
			return
		case de := <-e.events:
			e.locked(func() {
				e.handle(de)
				e.emit()
			})
		case <-t.C:
			e.locked(e.tick)
		}
	}
}

// locked выполняет f под e.mu; при панике мьютекс отпускается.
func (e *Engine) locked(f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f()
}

// SetPanicHandler задаёт реакцию на панику в фоновых горутинах движка: интерфейс должен восстановить
// терминал и завершиться. Без обработчика паника не перехватывается.
func (e *Engine) SetPanicHandler(h func(any)) { e.onPanic.Store(&h) }

func (e *Engine) recoverPanic() {
	r := recover()
	if r == nil {
		return
	}
	log.Printf("паника в плеере: %v\n%s", r, debug.Stack())
	if h := e.onPanic.Load(); h != nil && *h != nil {
		(*h)(r)
		return
	}
	panic(r)
}

// --- статусы ---

func (e *Engine) setStatus(s string) {
	e.status, e.statusErr, e.statusAt = s, false, time.Now()
}

func (e *Engine) setErr(s string) {
	log.Print(s)
	e.status, e.statusErr, e.statusAt = s, true, time.Now()
}

// Notify — сообщение в строке статуса от интерфейса.
func (e *Engine) Notify(msg string, isErr bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if isErr {
		e.setErr(msg)
	} else {
		e.setStatus(msg)
	}
	e.emit()
}

// --- громкость и нормализация ---

// gainFor — усиление в дБ, чтобы трек звучал на целевой громкости, без превышения −1 dBTP.
func (e *Engine) gainFor(t api.Track) float64 {
	if !e.cfg.Normalize || e.cfg.BitPerfect || e.noGain || t.R128 == nil || (t.R128.I == 0 && t.R128.TP == 0) {
		return 0
	}
	g := e.cfg.TargetLUFS - t.R128.I
	if limit := -1.0 - t.R128.TP; g > limit {
		g = limit
	}
	return math.Max(-20, math.Min(12, g))
}

func (e *Engine) baseVolume() float64 {
	if e.cfg.BitPerfect {
		return 100
	}
	return float64(e.cfg.Volume)
}

// volumeFor — значение свойства volume для амплитуды a (0..1). Громкость mpv кубическая.
func (e *Engine) volumeFor(a float64) float64 {
	return e.baseVolume() * math.Cbrt(math.Max(0, math.Min(1, a)))
}

func (e *Engine) fileOpts(s *slot, startAt float64) map[string]string {
	o := map[string]string{}
	if s.gain != 0 {
		o["volume-gain"] = fmt.Sprintf("%.2f", s.gain)
	}
	if startAt > 0 {
		o["start"] = fmt.Sprintf("%.3f", startAt)
	}
	if e.queue.repeat == RepeatOne {
		o["loop-file"] = "inf"
	}
	return o
}

// --- запуск треков ---

func trackDur(t api.Track) float64 { return float64(t.DurationMs) / 1000 }

// playPos — жёсткий запуск трека из очереди (с подготовкой источника в фоне).
func (e *Engine) playPos(p int, startAt float64, userSkip bool) {
	t, ok := e.queue.At(p)
	if !ok {
		return
	}
	if e.cur != nil && userSkip {
		e.reportEnd(e.cur, false, e.pos)
	}
	e.gen++
	gen := e.gen
	e.stopXfade()
	e.dropNext()
	e.queue.SetPos(p)
	cur := &slot{pos: p, track: t, gain: e.gainFor(t), startAt: startAt}
	e.cur = cur
	e.pos, e.dur = startAt, trackDur(t)
	e.loading = true
	quality := e.quality

	go func() {
		defer e.recoverPanic()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		src, err := e.srv.Prepare(ctx, t, quality)
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		defer e.emit()
		if gen != e.gen {
			return
		}
		e.loading = false
		if err != nil {
			msg := fmt.Sprintf("%s: %v", t.FullTitle(), err)
			e.failures++
			if e.failures < maxFailures {
				if np, ok := e.queue.NextPos(true); ok && np != p {
					e.setErr(msg)
					e.playPos(np, 0, false)
					return
				}
			}
			e.halt(msg)
			return
		}
		cur.src = src
		if err := e.ensureDecks(); err != nil {
			e.setErr("mpv: " + err.Error())
			e.cur = nil
			return
		}
		if e.crossfadeMode() {
			if o := e.decks[1-e.active]; o != nil {
				_, _ = o.m.Command("stop")
			}
		}
		d := e.decks[e.active]
		_ = d.m.Set("volume", e.baseVolume())
		_ = d.m.Set("pause", false)
		e.paused = false
		// cur.startAt, а не startAt: пока трек готовился, могли перемотать (seekTo).
		id, err := d.m.LoadFile(src.URL(), "replace", e.fileOpts(cur, cur.startAt))
		if err != nil {
			e.setErr("mpv loadfile: " + err.Error())
			e.cur = nil
			return
		}
		cur.entryID, cur.deck, cur.loaded = id, d.idx, true
		e.pin()
		e.onStarted(cur)
		e.scheduleNext()
	}()
}

// scheduleNext готовит следующий трек: в gapless добавляет его в плейлист mpv,
// в режиме кроссфейда — запоминает, а загрузку на вторую деку делает tick ближе к концу трека.
func (e *Engine) scheduleNext() {
	if e.cur == nil || e.next != nil || e.queue.repeat == RepeatOne {
		return
	}
	if e.wave != nil && e.queue.Len()-e.queue.Pos() <= 3 {
		e.fetchWave()
	}
	p, ok := e.queue.NextPos(false)
	if !ok {
		return
	}
	t, _ := e.queue.At(p)
	e.nextGen++
	gen, ngen, cur, quality := e.gen, e.nextGen, e.cur, e.quality
	go func() {
		defer e.recoverPanic()
		time.Sleep(nextDebounce)
		e.mu.Lock()
		stale := gen != e.gen || ngen != e.nextGen
		e.mu.Unlock()
		if stale {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		src, err := e.srv.Prepare(ctx, t, quality)
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		// ngen: пока шла подготовка, выбор следующего мог смениться (перемешивание, повтор, настройки).
		if gen != e.gen || ngen != e.nextGen || e.cur != cur || e.next != nil {
			return
		}
		if err != nil {
			log.Printf("подготовка следующего %s: %v", t.ID, err)
			return
		}
		s := &slot{pos: p, track: t, src: src, gain: e.gainFor(t)}
		if e.crossfadeMode() {
			s.startAt = e.introSkip(t)
			e.next = s
			e.pin()
			return
		}
		d := e.decks[e.active]
		if d == nil {
			return
		}
		id, err := d.m.LoadFile(src.URL(), "append", e.fileOpts(s, 0))
		if err != nil {
			log.Printf("mpv append: %v", err)
			return
		}
		s.entryID, s.deck, s.loaded = id, d.idx, true
		e.next = s
		e.pin()
	}()
}

// dropNext отменяет подготовленный следующий трек (смена порядка, повтора, настроек).
func (e *Engine) dropNext() {
	e.nextGen++
	n := e.next
	e.next = nil
	if n == nil || !n.loaded {
		return
	}
	if e.crossfadeMode() {
		if d := e.decks[n.deck]; d != nil && n.deck != e.active {
			_, _ = d.m.Command("stop")
		}
		return
	}
	if d := e.decks[e.active]; d != nil {
		// Удаляет из плейлиста mpv всё, кроме текущего файла.
		_, _ = d.m.Command("playlist-clear")
	}
}

func (e *Engine) onStarted(s *slot) {
	s.reported = false
	e.pos, e.dur = s.startAt, trackDur(s.track)
	if w := e.wave; w != nil {
		key := s.track.Key()
		w.played = append(w.played, key)
		sid, bid := w.sessionID, w.batchID
		go e.feedback(sid, bid, api.RotorEvent{Type: "trackStarted", TrackKey: key})
	}
}

func (e *Engine) reportEnd(s *slot, finished bool, played float64) {
	if s == nil || s.reported || e.wave == nil || !s.loaded {
		return
	}
	s.reported = true
	typ := "skip"
	if finished {
		typ = "trackFinished"
	}
	sec := math.Max(0, played)
	w := e.wave
	go e.feedback(w.sessionID, w.batchID, api.RotorEvent{Type: typ, TrackKey: s.track.Key(), TotalPlayedSeconds: &sec})
}

func (e *Engine) feedback(sid, bid string, ev api.RotorEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.api.RotorFeedback(ctx, sid, bid, ev); err != nil {
		log.Printf("rotor feedback %s: %v", ev.Type, err)
	}
}

// --- кроссфейд ---

// fadeWindow — когда начинать кроссфейд и сколько он длится.
func (e *Engine) fadeWindow(s *slot) (start, length float64) {
	dur := e.dur
	if dur <= 0 {
		dur = trackDur(s.track)
	}
	cf := math.Min(e.cfg.CrossfadeSec, dur/3)
	start = dur - cf
	if f := s.track.Fade; e.cfg.SmartCrossfade && f != nil && f.OutStart > dur/2 && f.OutStart < dur-0.5 {
		start = f.OutStart
		cf = math.Max(1, math.Min(e.cfg.CrossfadeSec, dur-f.OutStart))
	}
	return start, cf
}

// introSkip — пропуск тишины в начале следующего трека по разметке Яндекса.
func (e *Engine) introSkip(t api.Track) float64 {
	if f := t.Fade; e.cfg.SmartCrossfade && f != nil && f.InStart > 0 && f.InStart < 15 {
		return f.InStart
	}
	return 0
}

func (e *Engine) maybeCrossfade() {
	if !e.crossfadeMode() || e.cur == nil || !e.cur.loaded || e.xf != nil || e.paused || e.next == nil {
		return
	}
	other := e.decks[1-e.active]
	if other == nil {
		return
	}
	start, length := e.fadeWindow(e.cur)
	n := e.next
	if !n.loaded && e.pos >= start-12 {
		_ = other.m.Set("volume", 0)
		opts := e.fileOpts(n, n.startAt)
		opts["pause"] = "yes"
		id, err := other.m.LoadFile(n.src.URL(), "replace", opts)
		if err != nil {
			log.Printf("предзагрузка: %v", err)
			e.next = nil
			return
		}
		n.entryID, n.deck, n.loaded = id, other.idx, true
	}
	if n.loaded && e.pos >= start {
		e.beginXfade(length)
	}
}

func (e *Engine) beginXfade(length float64) {
	from := e.active
	to := e.next.deck
	_ = e.decks[to].m.Set("pause", false)
	e.xf = &xfade{from: from, dur: length, last: time.Now()}
	e.active = to
	out := e.cur
	e.reportEnd(out, true, e.pos)
	e.cur, e.next = e.next, nil
	e.queue.SetPos(e.cur.pos)
	e.onStarted(e.cur)
	e.scheduleNext()
}

func (e *Engine) stepXfade() {
	xf := e.xf
	now := time.Now()
	if !e.paused {
		xf.elapsed += now.Sub(xf.last).Seconds()
	}
	xf.last = now
	t := math.Min(1, xf.elapsed/xf.dur)
	if d := e.decks[xf.from]; d != nil {
		_ = d.m.Set("volume", e.volumeFor(math.Cos(t*math.Pi/2)))
	}
	if d := e.decks[e.active]; d != nil {
		_ = d.m.Set("volume", e.volumeFor(math.Sin(t*math.Pi/2)))
	}
	if t >= 1 {
		e.stopXfade()
	}
}

// stopXfade завершает кроссфейд немедленно: уходящая дека останавливается, входящая — на полной громкости.
func (e *Engine) stopXfade() {
	xf := e.xf
	if xf == nil {
		return
	}
	e.xf = nil
	if d := e.decks[xf.from]; d != nil {
		_, _ = d.m.Command("stop")
		_ = d.m.Set("volume", 0)
	}
	if d := e.decks[e.active]; d != nil {
		_ = d.m.Set("volume", e.baseVolume())
	}
}

// --- события mpv ---

func (e *Engine) handle(de deckEvent) {
	d := de.d
	if de.closed {
		if e.decks[d.idx] == d {
			e.decks[d.idx] = nil
			e.setErr("mpv неожиданно завершился — будет перезапущен при следующем треке (см. лог)")
			e.cur, e.next, e.xf = nil, nil, nil
		}
		return
	}
	if e.decks[d.idx] != d {
		return
	}
	ev := de.ev
	switch ev.Event {
	case "start-file":
		if !e.crossfadeMode() && d.idx == e.active && e.next != nil && ev.EntryID == e.next.entryID {
			e.advanceGapless()
		}
	case "end-file":
		e.onEndFile(d, ev)
	case "file-loaded":
		e.refreshAudio(d)
	case "audio-reconfig":
		e.refreshAudio(d)
	}
}

func (e *Engine) advanceGapless() {
	if out := e.cur; out != nil {
		if e.skipping {
			e.reportEnd(out, false, e.pos)
		} else {
			e.reportEnd(out, true, trackDur(out.track))
		}
	}
	e.skipping = false
	e.cur, e.next = e.next, nil
	e.queue.SetPos(e.cur.pos)
	e.onStarted(e.cur)
	e.scheduleNext()
}

func (e *Engine) onEndFile(d *deck, ev mpv.Event) {
	if e.xf != nil && d.idx == e.xf.from {
		return
	}
	if e.cur == nil || d.idx != e.active || ev.EntryID != e.cur.entryID {
		if n := e.next; n != nil && n.loaded && ev.EntryID == n.entryID && d.idx == n.deck && ev.Reason == "error" {
			log.Printf("следующий трек не открылся: %s", ev.FileErr)
			e.next = nil
		}
		return
	}
	switch ev.Reason {
	case "eof":
		if !e.crossfadeMode() && e.next != nil {
			return // gapless-переход придёт событием start-file
		}
		e.reportEnd(e.cur, true, trackDur(e.cur.track))
		e.advanceAfterEnd()
	case "error":
		msg := ev.FileErr
		if msg == "" {
			msg = "ошибка"
		}
		if msg == aoInitFailed {
			// Дело не в треке, а в устройстве: перебирать очередь бессмысленно.
			e.halt("Не открылся аудиовывод — проверьте устройство на вкладке «Настройки» (" + msg + ")")
			return
		}
		e.failures++
		msg = fmt.Sprintf("%s: не воспроизводится (%s)", e.cur.track.FullTitle(), msg)
		if e.failures < maxFailures {
			e.setErr(msg)
			e.advanceAfterEnd()
		} else {
			e.halt(msg)
		}
	}
}

// halt останавливает воспроизведение после ошибок: следующий трек убирается из плейлиста mpv,
// иначе mpv сам перейдёт к нему. Продолжить можно кнопкой «Играть» или выбором трека.
func (e *Engine) halt(msg string) {
	e.stopXfade()
	e.dropNext()
	for _, d := range e.decks {
		if d != nil {
			_, _ = d.m.Command("stop")
		}
	}
	e.cur = nil
	e.loading = false
	if e.failures >= maxFailures {
		msg = fmt.Sprintf("Остановлено: %d треков подряд не заиграли. %s", e.failures, msg)
	}
	e.setErr(msg)
}

func (e *Engine) advanceAfterEnd() {
	if p, ok := e.queue.NextPos(false); ok {
		e.playPos(p, 0, false)
		return
	}
	e.cur = nil
	e.next = nil
	if e.wave != nil {
		e.setStatus("Волна: загружаю следующие треки…")
		e.fetchWave()
		return
	}
	e.setStatus("Очередь закончилась")
}

func (e *Engine) refreshAudio(d *deck) {
	if d.idx != e.active {
		return
	}
	var in, out AudioParams
	if d.m.Get("audio-params", &in) == nil {
		e.in = in
	}
	if d.m.Get("audio-out-params", &out) == nil {
		e.out = out
	}
	var ao, dev string
	if d.m.Get("current-ao", &ao) == nil {
		e.ao = ao
	}
	if d.m.Get("audio-device", &dev) == nil {
		e.dev = dev
	}
}

func (e *Engine) tick() {
	e.tickN++
	if e.xf != nil {
		e.stepXfade()
	}
	if e.tickN%5 != 0 {
		return
	}
	if d := e.decks[e.active]; d != nil && e.cur != nil && e.cur.loaded {
		c := e.cur
		if c.hasSeekTo {
			// Перемотка, которую mpv не принял, пока файл открывался. Пока она не выполнена,
			// позиция в интерфейсе — целевая, а не та, с которой файл начал играть.
			if _, err := d.m.Command("seek", c.seekTo, "absolute"); err == nil {
				c.hasSeekTo = false
			} else if c.seekTries++; c.seekTries > 40 {
				log.Printf("перемотка на %.1f с не удалась: %v", c.seekTo, err)
				c.hasSeekTo = false
			}
		}
		var pos, dur float64
		if !c.hasSeekTo && d.m.Get("time-pos", &pos) == nil {
			e.pos = pos
			// Ошибки сбрасываются, только когда трек действительно играет, а не просто открылся.
			if e.failures > 0 && !e.paused && pos >= e.cur.startAt+progressReset {
				e.failures = 0
			}
		}
		if d.m.Get("duration", &dur) == nil && dur > 0 {
			e.dur = dur
		}
		var pfc bool
		if d.m.Get("paused-for-cache", &pfc) == nil {
			e.buffer = pfc
		}
		if !e.cur.hasFmt && e.cur.src != nil {
			e.cur.format, e.cur.hasFmt = e.cur.src.Format()
		}
		if e.in.SampleRate == 0 {
			e.refreshAudio(d)
		}
	}
	e.pin()
	e.maybeCrossfade()
	e.emit()
}

// pin закрепляет у прокси источники играющего и следующего трека: их не вытеснят новые подготовки.
func (e *Engine) pin() {
	var keep []*stream.Source
	if e.cur != nil {
		keep = append(keep, e.cur.src)
	}
	if e.next != nil {
		keep = append(keep, e.next.src)
	}
	e.srv.Keep(keep...)
}

// --- публикация состояния ---

func normFmt(f string) string { return strings.TrimSuffix(f, "p") }

func formatWidth(f string) int {
	switch normFmt(f) {
	case "u8":
		return 8
	case "s16":
		return 16
	case "s24":
		return 24
	case "s32":
		return 32
	case "float":
		return 100
	case "double":
		return 101
	}
	return 0
}

func (e *Engine) pathClean() bool {
	if !e.cfg.BitPerfect || e.cur == nil || e.in.SampleRate == 0 || e.out.SampleRate == 0 {
		return false
	}
	if e.ao != "alsa" {
		return false
	}
	// ALSA default/plughw/sysdefault — это плагины (на десктопе обычно PipeWire/PulseAudio с микшером
	// и ресемплингом), которых mpv не видит. Чистый путь — только прямое устройство hw:.
	if !strings.HasPrefix(e.dev, "alsa/hw:") {
		return false
	}
	if e.in.SampleRate != e.out.SampleRate || e.in.ChannelCount != e.out.ChannelCount {
		return false
	}
	if e.cur.gain != 0 {
		return false
	}
	fi, fo := normFmt(e.in.Format), normFmt(e.out.Format)
	if fi == fo {
		return true
	}
	// s16 → s32 и т.п. — дополнение нулями, биты исходника не меняются.
	wi, wo := formatWidth(fi), formatWidth(fo)
	return wi > 0 && wi < 100 && wo >= wi && wo < 100
}

func (e *Engine) emit() {
	st := State{
		Volume:      e.cfg.Volume,
		Requested:   e.quality,
		Paused:      e.paused,
		Buffering:   e.buffer,
		Loading:     e.loading,
		In:          e.in,
		Out:         e.out,
		AO:          e.ao,
		Device:      e.dev,
		BitPerfect:  e.cfg.BitPerfect,
		PathClean:   e.pathClean(),
		Transition:  e.cfg.Transition,
		Crossfading: e.xf != nil,
		Normalize:   e.cfg.Normalize && !e.cfg.BitPerfect,
		NoGain:      e.noGain,
		Shuffle:     e.queue.shuffle,
		Repeat:      e.queue.repeat,
		Source:      e.queue.source,
		QueuePos:    e.queue.Pos(),
		QueueLen:    e.queue.Len(),
		QueueVer:    e.queue.version,
		Wave:        e.wave != nil,
		Status:      e.status,
		StatusErr:   e.statusErr,
		StatusAt:    e.statusAt,
		DLPercent:   -1,
	}
	if e.cfg.BitPerfect {
		st.Volume = 100
	}
	if c := e.cur; c != nil {
		st.HasTrack = true
		st.Track = c.track
		st.Pos, st.Dur = e.pos, e.dur
		st.GainDB = c.gain
		if r := c.track.R128; r != nil && (r.I != 0 || r.TP != 0) {
			st.HasR128, st.LUFS, st.TP = true, r.I, r.TP
		}
		if s := c.src; s != nil {
			st.Quality, st.Codec, st.Bitrate, st.FromCache = s.Quality, s.Codec, s.Bitrate, s.FromCache
			st.Committed = s.Committed()
			w, size, done := s.Progress()
			st.DLDone = done
			if size > 0 {
				st.DLPercent = 100 * float64(w) / float64(size)
			}
		}
		st.SrcFormat, st.HasSrcFormat = c.format, c.hasFmt
	}
	push := func(ch chan State) {
		select {
		case <-ch:
		default:
		}
		ch <- st
	}
	push(e.updates)
	for _, ch := range e.subs {
		push(ch)
	}
}

// --- управление из интерфейса ---

// PlayTracks запускает список треков с позиции start.
func (e *Engine) PlayTracks(tracks []api.Track, start int, source string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur != nil {
		e.reportEnd(e.cur, false, e.pos)
	}
	e.wave = nil
	e.failures = 0
	e.queue.Set(tracks, start, source)
	e.playPos(e.queue.Pos(), 0, false)
	e.emit()
}

// JumpTo — перейти к позиции в текущей очереди.
func (e *Engine) JumpTo(p int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures = 0
	e.playPos(p, 0, true)
	e.emit()
}

// PlayWave запускает «Мою волну» (или её вариант) по сидам.
func (e *Engine) PlayWave(seeds []string, title string) {
	e.mu.Lock()
	e.setStatus("Запускаю «" + title + "»…")
	e.emit()
	e.mu.Unlock()
	go func() {
		defer e.recoverPanic()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s, err := e.api.RotorSessionNew(ctx, seeds, nil)
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		defer e.emit()
		if err != nil {
			e.setErr("Волна: " + err.Error())
			return
		}
		tracks := s.Tracks()
		if len(tracks) == 0 {
			e.setErr("Волна вернула пустой список")
			return
		}
		if e.cur != nil {
			e.reportEnd(e.cur, false, e.pos)
		}
		w := &wave{sessionID: s.RadioSessionID, batchID: s.BatchID}
		e.wave = w
		e.failures = 0
		e.queue.SetShuffle(false)
		e.queue.Set(tracks, 0, title)
		e.setStatus("")
		go e.feedback(w.sessionID, w.batchID, api.RotorEvent{Type: "radioStarted"})
		e.playPos(0, 0, false)
	}()
}

func (e *Engine) fetchWave() {
	w := e.wave
	if w == nil || w.fetching {
		return
	}
	w.fetching = true
	q := w.played
	if len(q) > 50 {
		q = q[len(q)-50:]
	}
	q = append([]string(nil), q...)
	for p := e.queue.Pos() + 1; p < e.queue.Len(); p++ {
		if t, ok := e.queue.At(p); ok {
			q = append(q, t.Key())
		}
	}
	go func() {
		defer e.recoverPanic()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s, err := e.api.RotorSessionTracks(ctx, w.sessionID, q)
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		defer e.emit()
		w.fetching = false
		if e.wave != w {
			return
		}
		if err != nil {
			e.setErr("Волна: " + err.Error())
			return
		}
		if s.BatchID != "" {
			w.batchID = s.BatchID
		}
		added := e.queue.Append(s.Tracks())
		if added == 0 {
			return
		}
		if e.cur == nil {
			if p, ok := e.queue.NextPos(false); ok {
				e.playPos(p, 0, false)
			}
			return
		}
		e.scheduleNext()
	}()
}

func (e *Engine) TogglePause() {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setPaused(!e.paused)
}

// PlayPause — пауза/продолжение; если ничего не играет, запускает текущую позицию очереди.
// Возвращает false, если играть нечего (очередь пуста).
func (e *Engine) PlayPause() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	if e.cur == nil {
		if e.queue.Len() == 0 {
			return false
		}
		e.failures = 0
		e.playPos(e.queue.Pos(), 0, false)
		return true
	}
	e.setPaused(!e.paused)
	return true
}

// SetPaused — явная пауза или продолжение (для MPRIS Play/Pause).
func (e *Engine) SetPaused(p bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setPaused(p)
}

func (e *Engine) setPaused(p bool) {
	d := e.decks[e.active]
	if d == nil || e.cur == nil || e.paused == p {
		return
	}
	e.paused = p
	_ = d.m.Set("pause", p)
	if e.xf != nil {
		if fd := e.decks[e.xf.from]; fd != nil {
			_ = fd.m.Set("pause", p)
		}
	}
}

func (e *Engine) Next() {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.failures = 0
	if e.cur == nil {
		if e.queue.Len() > 0 {
			if p, ok := e.queue.NextPos(true); ok {
				e.playPos(p, 0, false)
			}
		}
		return
	}
	e.stopXfade()
	n := e.next
	if n != nil && n.loaded {
		if e.crossfadeMode() {
			// Следующий трек уже загружен на вторую деку — переключаемся мгновенно.
			e.reportEnd(e.cur, false, e.pos)
			if old := e.decks[e.active]; old != nil {
				_, _ = old.m.Command("stop")
			}
			nd := e.decks[n.deck]
			if nd == nil {
				e.playPos(n.pos, 0, false)
				return
			}
			e.active = n.deck
			_ = nd.m.Set("volume", e.baseVolume())
			_ = nd.m.Set("pause", false)
			e.paused = false
			e.cur, e.next = n, nil
			e.queue.SetPos(n.pos)
			e.onStarted(n)
			e.scheduleNext()
			return
		}
		if d := e.decks[e.active]; d != nil {
			// Следующий уже в плейлисте mpv: переход через playlist-next, без повторной загрузки.
			e.skipping = true
			_ = d.m.Set("pause", false)
			e.paused = false
			if _, err := d.m.Command("playlist-next", "force"); err == nil {
				return
			}
			e.skipping = false
		}
	}
	if p, ok := e.queue.NextPos(true); ok {
		e.playPos(p, 0, true)
	} else if e.wave != nil {
		e.reportEnd(e.cur, false, e.pos)
		e.fetchWave()
	}
}

func (e *Engine) Prev() {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	if e.cur != nil && e.pos > 3 {
		e.seekTo(0)
		return
	}
	if p, ok := e.queue.PrevPos(); ok {
		e.failures = 0
		e.playPos(p, 0, true)
	}
}

// Seek — перемотка относительно текущей позиции, в секундах.
func (e *Engine) Seek(delta float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.seekTo(e.pos + delta)
}

// SeekTo — перемотка на абсолютную позицию, в секундах.
func (e *Engine) SeekTo(sec float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.seekTo(sec)
}

func (e *Engine) seekTo(sec float64) {
	c := e.cur
	if c == nil {
		return
	}
	sec = math.Max(0, sec)
	if !c.loaded {
		// Трек ещё готовится (например, сразу после «Вперёд»): он начнётся с нужного места.
		c.startAt, e.pos = sec, sec
		return
	}
	d := e.decks[e.active]
	if d == nil {
		return
	}
	e.stopXfade()
	if _, err := d.m.Command("seek", sec, "absolute"); err != nil {
		// Файл ещё открывается (например, сразу после «Вперёд»): tick повторит перемотку.
		c.seekTo, c.hasSeekTo, c.seekTries = sec, true, 0
	} else {
		c.hasSeekTo = false
	}
	e.pos = sec
}

func (e *Engine) ChangeVolume(delta int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setVolume(e.cfg.Volume + delta)
}

// SetVolume — громкость 0..100.
func (e *Engine) SetVolume(v int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setVolume(v)
}

func (e *Engine) setVolume(v int) {
	if e.cfg.BitPerfect {
		e.setStatus("Bit-perfect: громкость регулируется на ЦАП/усилителе")
		return
	}
	e.cfg.Volume = max(0, min(100, v))
	if e.xf == nil {
		if d := e.decks[e.active]; d != nil {
			_ = d.m.Set("volume", e.baseVolume())
		}
	}
}

func (e *Engine) Volume() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.Volume
}

func (e *Engine) ToggleShuffle() {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setShuffle(!e.queue.shuffle)
}

func (e *Engine) SetShuffle(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setShuffle(on)
}

func (e *Engine) setShuffle(on bool) {
	if on == e.queue.shuffle {
		return
	}
	if e.wave != nil {
		e.setStatus("В волне перемешивание не нужно")
		return
	}
	e.queue.SetShuffle(on)
	if e.cur != nil {
		e.cur.pos = e.queue.Pos()
	}
	e.dropNext()
	e.scheduleNext()
}

func (e *Engine) CycleRepeat() {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setRepeat((e.queue.repeat + 1) % 3)
}

func (e *Engine) SetRepeat(r Repeat) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.setRepeat(r)
}

func (e *Engine) setRepeat(r Repeat) {
	if r == e.queue.repeat {
		return
	}
	e.queue.repeat = r
	loop := "no"
	if r == RepeatOne {
		loop = "inf"
	}
	if d := e.decks[e.active]; d != nil {
		_ = d.m.Set("loop-file", loop)
	}
	e.dropNext()
	e.scheduleNext()
}

// SetQuality меняет качество и перезапускает текущий трек с той же позиции.
func (e *Engine) SetQuality(q string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	e.quality = q
	if e.cur != nil {
		e.playPos(e.cur.pos, e.pos, false)
	}
}

// ApplyConfig применяет настройки звука; при смене вывода/режима mpv перезапускается.
func (e *Engine) ApplyConfig(cfg config.Audio) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.emit()
	old := e.cfg
	// Громкостью владеет движок (клавиши +/-, MPRIS); в cfg из интерфейса она может быть устаревшей.
	cfg.Volume = old.Volume
	e.cfg = cfg
	if old.MpvPath != cfg.MpvPath {
		aos, err := mpv.AudioOutputs(cfg.MpvPath)
		if err != nil {
			e.cfg = old
			return fmt.Errorf("mpv (%s): %w", cfg.MpvPath, err)
		}
		e.aos = aos
	}
	restart := old.MpvPath != cfg.MpvPath || old.Device != cfg.Device ||
		old.BitPerfect != cfg.BitPerfect || old.Transition != cfg.Transition ||
		strings.Join(old.ExtraArgs, "\x00") != strings.Join(cfg.ExtraArgs, "\x00")
	if restart {
		cur, pos := e.cur, e.pos
		e.gen++
		e.cur, e.next = nil, nil
		e.in, e.out = AudioParams{}, AudioParams{}
		if err := e.startDecks(); err != nil {
			e.setErr("mpv: " + err.Error())
			return err
		}
		if cur != nil {
			e.playPos(cur.pos, pos, false)
		}
		return nil
	}
	if c := e.cur; c != nil {
		c.gain = e.gainFor(c.track)
		if d := e.decks[e.active]; d != nil && c.loaded && !e.noGain {
			_ = d.m.Set("volume-gain", c.gain)
		}
	}
	e.dropNext()
	e.scheduleNext()
	return nil
}

// Devices — устройства вывода текущего mpv.
func (e *Engine) Devices() []mpv.Device {
	e.mu.Lock()
	d := e.decks[0]
	e.mu.Unlock()
	if d == nil {
		return nil
	}
	var list []mpv.Device
	_ = d.m.Get("audio-device-list", &list)
	return list
}

// Queue — снимок очереди для экрана «Очередь».
func (e *Engine) Queue() ([]api.Track, int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.queue.Items(), e.queue.Pos(), e.queue.source
}

// CurrentTrack — играющий трек.
func (e *Engine) CurrentTrack() (api.Track, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur == nil {
		return api.Track{}, false
	}
	return e.cur.track, true
}
