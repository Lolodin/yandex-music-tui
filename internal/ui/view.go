package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"ymusic/internal/config"
	"ymusic/internal/player"
)

var (
	cAccent = lipgloss.Color("#FFCC00")
	cDim    = lipgloss.Color("244")
	cErr    = lipgloss.Color("203")
	cGood   = lipgloss.Color("78")
	cWarn   = lipgloss.Color("214")

	sTabOn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(cAccent).Padding(0, 1)
	sTabOff = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Padding(0, 1)
	sSel    = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("231")).Bold(true)
	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sAccent = lipgloss.NewStyle().Foreground(cAccent)
	sErr    = lipgloss.NewStyle().Foreground(cErr)
	sGood   = lipgloss.NewStyle().Foreground(cGood).Bold(true)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sHead   = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sBold   = lipgloss.NewStyle().Bold(true)
)

const playerLines = 5 // разделитель + 4 строки плеера

func (m *Model) searchLine() bool { return m.tab == tabSearch }

func (m *Model) listHeight() int {
	h := m.height - 2 - playerLines
	if m.searchLine() {
		h--
	}
	return max(1, h)
}

func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		return ansi.Truncate(s, w, "…")
	}
	return s
}

func padRight(s string, w int) string {
	s = fit(s, w)
	if d := w - lipgloss.Width(s); d > 0 {
		s += strings.Repeat(" ", d)
	}
	return s
}

// leftRight — строка ширины w: left слева, right прижат вправо.
func leftRight(left, right string, w int) string {
	rw := lipgloss.Width(right)
	if rw >= w {
		return fit(right, w)
	}
	left = fit(left, w-rw-1)
	gap := w - lipgloss.Width(left) - rw
	return left + strings.Repeat(" ", max(1, gap)) + right
}

func fmtDur(sec float64) string {
	if sec < 0 || math.IsNaN(sec) {
		sec = 0
	}
	s := int(sec + 0.5)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func khz(sr int) string {
	if sr%1000 == 0 {
		return fmt.Sprintf("%d", sr/1000)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", float64(sr)/1000), "0"), ".")
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.renderTabs())
	b.WriteByte('\n')
	b.WriteString(m.renderCrumb())
	b.WriteByte('\n')
	if m.searchLine() {
		if m.typing {
			b.WriteString(fit(m.search.View(), m.width))
		} else {
			b.WriteString(sDim.Render(fit(" / или i — новый поиск · Enter на исполнителе/альбоме открывает его · w — волна по выбранному", m.width)))
		}
		b.WriteByte('\n')
	}
	h := m.listHeight()
	if m.showHelp {
		b.WriteString(m.renderHelp(h))
	} else {
		b.WriteString(m.renderList(h))
	}
	b.WriteByte('\n')
	b.WriteString(m.renderPlayer())
	return b.String()
}

func (m *Model) renderTabs() string {
	parts := []string{sAccent.Bold(true).Render(" ♫ ")}
	for i := tab(0); i < numTabs; i++ {
		label := fmt.Sprintf("%d %s", i+1, tabNames[i])
		if i == m.tab {
			parts = append(parts, sTabOn.Render(label))
		} else {
			parts = append(parts, sTabOff.Render(label))
		}
	}
	line := strings.Join(parts, "")
	if m.offline() {
		line += " " + sWarn.Render("офлайн")
	}
	return fit(line, m.width)
}

func (m *Model) renderCrumb() string {
	s := m.stacks[m.tab]
	titles := make([]string, len(s))
	for i, v := range s {
		titles[i] = v.title
	}
	v := m.cur()
	line := " " + sBold.Render(strings.Join(titles, " › "))
	count := 0
	for _, it := range v.items {
		if it.kind != kHeader && it.kind != kInfo {
			count++
		}
	}
	if count > 0 {
		line += sDim.Render(fmt.Sprintf("  %d", count))
	}
	if v.loading {
		line += sAccent.Render("  загрузка…")
	}
	if len(s) > 1 {
		line += sDim.Render("  (Esc — назад)")
	}
	return fit(line, m.width)
}

func (m *Model) renderList(h int) string {
	v := m.cur()
	lines := make([]string, 0, h)
	switch {
	case v.err != "":
		lines = append(lines, "", sErr.Render(fit("  "+v.err, m.width)), sDim.Render("  R — повторить"))
	case v.loading && len(v.items) == 0:
		lines = append(lines, "", sDim.Render("  Загрузка…"))
	default:
		if v.cursor < v.offset {
			v.offset = v.cursor
		}
		if v.cursor >= v.offset+h {
			v.offset = v.cursor - h + 1
		}
		v.offset = max(0, min(v.offset, max(0, len(v.items)-h)))
		for i := v.offset; i < len(v.items) && len(lines) < h; i++ {
			lines = append(lines, m.renderItem(v.items[i], i == v.cursor))
		}
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines[:h], "\n")
}

func (m *Model) renderItem(it item, sel bool) string {
	w := m.width
	var line string
	switch it.kind {
	case kHeader:
		t := " ── " + it.title + " "
		return sHead.Render(t + strings.Repeat("─", max(0, w-lipgloss.Width(t)-1)))
	case kInfo:
		return sDim.Render(fit("   "+it.title, w))
	case kWave:
		line = "  ≋ " + sBold.Render(it.title)
		if it.sub != "" {
			line += sDim.Render("  " + it.sub)
		}
	case kSetting:
		line = "  " + padRight(settingTitles[it.setting], 38) + sAccent.Render("‹ "+m.settingValue(it.setting)+" ›")
	case kPlaylist:
		p := it.playlist
		meta := fmt.Sprintf("  %d тр.", p.TrackCount)
		if o := p.OwnerName(); o != "" && m.account != nil && p.UID != m.account.Account.UID {
			meta = "  " + o + meta
		}
		line = "  ▤ " + p.Title + sDim.Render(meta)
	case kAlbum:
		a := it.album
		meta := "  " + a.ArtistNames()
		if a.Year > 0 {
			meta += fmt.Sprintf(" · %d", a.Year)
		}
		if a.TrackCount > 0 {
			meta += fmt.Sprintf(" · %d тр.", a.TrackCount)
		}
		line = "  ◉ " + a.Title + sDim.Render(meta)
	case kArtist:
		line = "  ♪ " + it.artist.Name
	case kTrack:
		line = m.renderTrack(it, sel)
	}
	if sel {
		return sSel.Render(padRight(ansi.Strip(line), w))
	}
	return fit(line, w)
}

func (m *Model) renderTrack(it item, sel bool) string {
	t := it.track
	w := m.width
	marker := "  "
	if m.st.HasTrack && m.st.Track.ID == t.ID {
		marker = sAccent.Render("▶ ")
	}
	like := "  "
	if m.likes.Liked(t.ID) {
		like = sErr.Render("♥ ")
	}
	title := t.FullTitle()
	if t.ContentWarning == "explicit" {
		title += " [E]"
	}
	left := marker + like + title + sDim.Render(" — "+t.ArtistNames())
	right := fmtDur(float64(t.DurationMs) / 1000)
	if e := it.cached; e != nil {
		right = fmt.Sprintf("%s %s  %5.1f МБ  %s", strings.ToUpper(e.Codec), e.Quality, float64(e.Size)/(1<<20), right)
	} else if album := t.AlbumTitle(); album != "" && w >= 110 {
		right = sDim.Render(fit(album, w/4)) + "  " + right
	}
	line := leftRight(left, right, w-1)
	if !t.IsAvailable() && it.cached == nil && !sel {
		return sDim.Render(ansi.Strip(line))
	}
	return line
}

func (m *Model) renderHelp(h int) string {
	rows := [][2]string{
		{"Tab / Shift+Tab, 1–8", "вкладки"},
		{"↑↓ / j k, PgUp/PgDn, g/G", "навигация"},
		{"Enter", "играть список с трека / открыть плейлист, альбом, исполнителя"},
		{"Esc / Backspace", "назад"},
		{"/", "поиск"},
		{"Пробел", "пауза"},
		{"n / p", "следующий / предыдущий трек"},
		{"← → / [ ]", "перемотка ±5 с / ±30 с"},
		{"+ / -", "громкость"},
		{"l / L", "лайк играющего / выбранного трека"},
		{"d", "дизлайк играющего трека (пропустить)"},
		{"w", "волна по выбранному треку, исполнителю или альбому"},
		{"c", "качество: lossless → hq → nq (трек перезапускается с той же позиции)"},
		{"s / r", "перемешивание / повтор"},
		{"R", "обновить список"},
		{"q / Ctrl+C", "выход"},
	}
	lines := []string{"", sHead.Render("  Клавиши"), ""}
	for _, r := range rows {
		lines = append(lines, "  "+sAccent.Render(padRight(r[0], 28))+r[1])
	}
	lines = append(lines, "", sDim.Render("  Любая клавиша — закрыть"))
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = fit(lines[i], m.width)
	}
	return strings.Join(lines[:h], "\n")
}

func codecLabel(c string) string {
	switch c {
	case "flac":
		return "FLAC"
	case "flac-mp4":
		return "FLAC/MP4"
	case "aac-mp4":
		return "AAC/MP4"
	case "he-aac-mp4":
		return "HE-AAC/MP4"
	case "he-aac":
		return "HE-AAC"
	}
	return strings.ToUpper(c)
}

func qualityTag(q string) string {
	switch q {
	case config.QualityLossless:
		return sGood.Render("LOSSLESS")
	case config.QualityHigh:
		return sAccent.Render("HQ")
	case config.QualityNormal:
		return sWarn.Render("NQ")
	case "preview":
		return sErr.Render("ПРЕВЬЮ 30 с — нужна подписка Плюс")
	}
	return strings.ToUpper(q)
}

// signalLine — путь сигнала: исходник → обработка → выход.
func (m *Model) signalLine(st player.State) string {
	if !st.HasTrack {
		return sDim.Render(" Качество: " + config.QualityLabel(m.cfg.Quality))
	}
	if st.Codec == "" {
		return sDim.Render(" подготовка потока…")
	}
	var parts []string
	src := " " + qualityTag(st.Quality) + " " + codecLabel(st.Codec)
	switch {
	case st.HasSrcFormat:
		src += fmt.Sprintf(" %d bit/%s kHz", st.SrcFormat.Bits, khz(st.SrcFormat.SampleRate))
	case st.In.SampleRate > 0:
		src += " " + khz(st.In.SampleRate) + " kHz"
	}
	if st.Bitrate > 0 {
		src += fmt.Sprintf(" %d kbps", st.Bitrate)
	}
	if st.Quality != "preview" && config.QualityRank(st.Quality) < config.QualityRank(st.Requested) {
		src += sDim.Render(" (" + st.Requested + " нет)")
	}
	parts = append(parts, src)

	if st.Normalize {
		if st.HasR128 {
			parts = append(parts, fmt.Sprintf("R128 %.1f LUFS → %+.1f dB", st.LUFS, st.GainDB))
		} else {
			parts = append(parts, sDim.Render("R128 нет данных"))
		}
	}
	if st.Out.SampleRate > 0 {
		out := fmt.Sprintf("→ %s %s kHz", strings.TrimSuffix(st.Out.Format, "p"), khz(st.Out.SampleRate))
		if st.AO != "" {
			out += " · " + st.AO
		}
		parts = append(parts, out)
	}
	if st.BitPerfect {
		switch {
		case st.PathClean:
			parts = append(parts, sGood.Render("BIT-PERFECT ✓"))
		case st.Out.SampleRate == 0:
			parts = append(parts, sDim.Render("bit-perfect…"))
		case st.AO != "alsa":
			parts = append(parts, sErr.Render("не bit-perfect: вывод через "+st.AO))
		case st.In.SampleRate != st.Out.SampleRate:
			parts = append(parts, sErr.Render("не bit-perfect: ресемплинг"))
		default:
			parts = append(parts, sErr.Render("не bit-perfect: конвертация формата"))
		}
	}
	switch {
	case st.Crossfading:
		parts = append(parts, sAccent.Render("⇄ кроссфейд"))
	case st.Transition == config.TransitionCrossfade && !st.BitPerfect:
		parts = append(parts, "кроссфейд")
	default:
		parts = append(parts, "gapless")
	}
	switch {
	case st.FromCache:
		parts = append(parts, sGood.Render("из кэша"))
	case st.Committed:
		parts = append(parts, "в кэше ✓")
	case st.DLDone:
		parts = append(parts, "загружен")
	case st.DLPercent >= 0:
		parts = append(parts, fmt.Sprintf("загрузка %d%%", int(st.DLPercent)))
	}
	return strings.Join(parts, sDim.Render(" · "))
}

func (m *Model) renderPlayer() string {
	st := m.st
	w := m.width
	lines := []string{sDim.Render(strings.Repeat("─", w))}

	// Строка 1: трек.
	var left string
	switch {
	case !st.HasTrack:
		left = sDim.Render(" Ничего не играет")
	default:
		icon := sAccent.Render(" ▶ ")
		if st.Paused {
			icon = sDim.Render(" ❚❚ ")
		}
		if st.Loading || st.Buffering {
			icon = sAccent.Render(" … ")
		}
		left = icon + sBold.Render(st.Track.FullTitle()) + sDim.Render(" — "+st.Track.ArtistNames())
	}
	var flags []string
	if st.HasTrack && m.likes.Liked(st.Track.ID) {
		flags = append(flags, sErr.Render("♥"))
	}
	if st.Wave {
		flags = append(flags, sAccent.Render("≋ волна"))
	} else if st.QueueLen > 0 {
		flags = append(flags, sDim.Render(fmt.Sprintf("%d/%d", st.QueuePos+1, st.QueueLen)))
	}
	if st.Shuffle {
		flags = append(flags, sAccent.Render("⤮"))
	}
	switch st.Repeat {
	case player.RepeatAll:
		flags = append(flags, sAccent.Render("⟳"))
	case player.RepeatOne:
		flags = append(flags, sAccent.Render("⟳1"))
	}
	if st.BitPerfect {
		flags = append(flags, sDim.Render("vol —"))
	} else {
		flags = append(flags, fmt.Sprintf("vol %d%%", st.Volume))
	}
	lines = append(lines, leftRight(left, strings.Join(flags, "  ")+" ", w))

	// Строка 2: прогресс.
	pos, dur := st.Pos, st.Dur
	l, r := " "+fmtDur(pos)+" ", " "+fmtDur(dur)+" "
	barW := max(5, w-lipgloss.Width(l)-lipgloss.Width(r))
	filled := 0
	if dur > 0 {
		filled = int(math.Min(1, pos/dur) * float64(barW))
	}
	bar := sAccent.Render(strings.Repeat("━", filled)) + sDim.Render(strings.Repeat("─", barW-filled))
	lines = append(lines, fit(l+bar+r, w))

	// Строка 3: путь сигнала и качество.
	lines = append(lines, fit(m.signalLine(st), w))

	// Строка 4: статус или подсказки.
	var status string
	if st.Status != "" && time.Since(st.StatusAt) < 8*time.Second {
		if st.StatusErr {
			status = sErr.Render(" " + st.Status)
		} else {
			status = sAccent.Render(" " + st.Status)
		}
	} else {
		status = sDim.Render(" ␣ пауза · n/p трек · ←→ перемотка · +/- громк. · l ♥ · d дизлайк · c качество · ? справка · q выход")
	}
	lines = append(lines, fit(status, w))
	return strings.Join(lines, "\n")
}
