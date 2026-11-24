package desktop

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"ymusic/internal/api"
	"ymusic/internal/player"
)

// MPRIS D-Bus Interface Specification v2.2.
const (
	mprisPath   = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	ifaceRoot   = "org.mpris.MediaPlayer2"
	ifacePlayer = "org.mpris.MediaPlayer2.Player"
	noTrack     = dbus.ObjectPath("/org/mpris/MediaPlayer2/TrackList/NoTrack")
)

type mprisRoot struct{ c *controller }

func (r *mprisRoot) Raise() *dbus.Error { return nil }

func (r *mprisRoot) Quit() *dbus.Error {
	r.c.quit()
	return nil
}

type mprisPlayer struct {
	c    *controller
	conn *dbus.Conn

	mu sync.Mutex
	st player.State
}

func (p *mprisPlayer) state() player.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func (p *mprisPlayer) Next() *dbus.Error      { p.c.eng.Next(); return nil }
func (p *mprisPlayer) Previous() *dbus.Error  { p.c.eng.Prev(); return nil }
func (p *mprisPlayer) Pause() *dbus.Error     { p.c.eng.SetPaused(true); return nil }
func (p *mprisPlayer) PlayPause() *dbus.Error { p.c.playPause(); return nil }
func (p *mprisPlayer) Play() *dbus.Error      { p.c.play(p.state()); return nil }

// Stop — по договорённости «стоп» = пауза: продолжить можно с того же места.
func (p *mprisPlayer) Stop() *dbus.Error { p.c.eng.SetPaused(true); return nil }

// SeekBy — метод MPRIS Seek (смещение в микросекундах). На шине экспортируется под именем Seek;
// в Go назван иначе, чтобы не путать с io.Seeker.
func (p *mprisPlayer) SeekBy(offset int64) *dbus.Error {
	p.c.eng.Seek(float64(offset) / 1e6)
	return nil
}

// SetPosition — абсолютная позиция в микросекундах; игнорируется, если трек уже сменился.
func (p *mprisPlayer) SetPosition(track dbus.ObjectPath, pos int64) *dbus.Error {
	st := p.state()
	if !st.HasTrack || track != trackPath(st.Track.ID) || pos < 0 {
		return nil
	}
	p.c.eng.SeekTo(float64(pos) / 1e6)
	return nil
}

func (p *mprisPlayer) OpenUri(string) *dbus.Error {
	return dbus.MakeFailedError(errors.New("открытие ссылок не поддерживается"))
}

// trackPath — идентификатор трека для MPRIS (объектный путь D-Bus: только [A-Za-z0-9_]).
func trackPath(id api.ID) dbus.ObjectPath {
	if id == "" {
		return noTrack
	}
	var b strings.Builder
	for _, r := range string(id) {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return dbus.ObjectPath("/ru/ymusic/track/t" + b.String())
}

func metadata(st player.State) map[string]dbus.Variant {
	if !st.HasTrack {
		return map[string]dbus.Variant{"mpris:trackid": dbus.MakeVariant(noTrack)}
	}
	t := st.Track
	dur := st.Dur
	if dur <= 0 {
		dur = float64(t.DurationMs) / 1000
	}
	artists := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		artists = append(artists, a.Name)
	}
	md := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(trackPath(t.ID)),
		"mpris:length":  dbus.MakeVariant(int64(dur * 1e6)),
		"xesam:title":   dbus.MakeVariant(t.FullTitle()),
		"xesam:artist":  dbus.MakeVariant(artists),
	}
	if al := t.AlbumTitle(); al != "" {
		md["xesam:album"] = dbus.MakeVariant(al)
	}
	if u := coverURL(t); u != "" {
		md["mpris:artUrl"] = dbus.MakeVariant(u)
	}
	if a := t.AlbumID(); a != "" {
		md["xesam:url"] = dbus.MakeVariant(fmt.Sprintf("https://music.yandex.ru/album/%s/track/%s", a, t.ID))
	}
	return md
}

func playbackStatus(st player.State) string {
	switch {
	case !st.HasTrack:
		return "Stopped"
	case st.Paused:
		return "Paused"
	}
	return "Playing"
}

func loopStatus(r player.Repeat) string {
	switch r {
	case player.RepeatOne:
		return "Track"
	case player.RepeatAll:
		return "Playlist"
	}
	return "None"
}

func (c *controller) startMPRIS() error {
	// Отдельное соединение: библиотека трея при завершении закрывает общее.
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	root := &mprisRoot{c: c}
	pl := &mprisPlayer{c: c, conn: conn}

	setErr := func(err error) *dbus.Error { return dbus.MakeFailedError(err) }
	props, err := prop.Export(conn, mprisPath, prop.Map{
		ifaceRoot: {
			"CanQuit":             {Value: true, Emit: prop.EmitConst},
			"CanRaise":            {Value: false, Emit: prop.EmitConst},
			"HasTrackList":        {Value: false, Emit: prop.EmitConst},
			"Identity":            {Value: "Яндекс Музыка (ymusic)", Emit: prop.EmitConst},
			"SupportedUriSchemes": {Value: []string{}, Emit: prop.EmitConst},
			"SupportedMimeTypes":  {Value: []string{}, Emit: prop.EmitConst},
		},
		ifacePlayer: {
			"PlaybackStatus": {Value: "Stopped", Emit: prop.EmitTrue},
			"LoopStatus": {Value: "None", Writable: true, Emit: prop.EmitTrue, Callback: func(ch *prop.Change) *dbus.Error {
				switch ch.Value.(string) {
				case "Track":
					c.eng.SetRepeat(player.RepeatOne)
				case "Playlist":
					c.eng.SetRepeat(player.RepeatAll)
				case "None":
					c.eng.SetRepeat(player.RepeatOff)
				default:
					return setErr(errors.New("неизвестный LoopStatus"))
				}
				return nil
			}},
			"Rate": {Value: 1.0, Writable: true, Emit: prop.EmitTrue, Callback: func(*prop.Change) *dbus.Error { return nil }},
			"Shuffle": {Value: false, Writable: true, Emit: prop.EmitTrue, Callback: func(ch *prop.Change) *dbus.Error {
				c.eng.SetShuffle(ch.Value.(bool))
				return nil
			}},
			"Metadata": {Value: metadata(player.State{}), Emit: prop.EmitTrue},
			"Volume": {Value: 1.0, Writable: true, Emit: prop.EmitTrue, Callback: func(ch *prop.Change) *dbus.Error {
				v := ch.Value.(float64)
				c.eng.SetVolume(int(math.Round(math.Max(0, math.Min(1, v)) * 100)))
				return nil
			}},
			"Position":      {Value: int64(0), Emit: prop.EmitFalse},
			"MinimumRate":   {Value: 1.0, Emit: prop.EmitConst},
			"MaximumRate":   {Value: 1.0, Emit: prop.EmitConst},
			"CanGoNext":     {Value: true, Emit: prop.EmitTrue},
			"CanGoPrevious": {Value: true, Emit: prop.EmitTrue},
			"CanPlay":       {Value: true, Emit: prop.EmitTrue},
			"CanPause":      {Value: true, Emit: prop.EmitTrue},
			"CanSeek":       {Value: false, Emit: prop.EmitTrue},
			"CanControl":    {Value: true, Emit: prop.EmitConst},
		},
	})
	if err != nil {
		conn.Close()
		return err
	}
	if err := conn.Export(root, mprisPath, ifaceRoot); err != nil {
		conn.Close()
		return err
	}
	if err := conn.ExportWithMap(pl, map[string]string{"SeekBy": "Seek"}, mprisPath, ifacePlayer); err != nil {
		conn.Close()
		return err
	}
	plMethods := introspect.Methods(pl)
	for i := range plMethods {
		if plMethods[i].Name == "SeekBy" {
			plMethods[i].Name = "Seek"
		}
	}
	node := &introspect.Node{
		Name: string(mprisPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{Name: ifaceRoot, Methods: introspect.Methods(root), Properties: props.Introspection(ifaceRoot)},
			{
				Name:       ifacePlayer,
				Methods:    plMethods,
				Properties: props.Introspection(ifacePlayer),
				Signals:    []introspect.Signal{{Name: "Seeked", Args: []introspect.Arg{{Name: "Position", Type: "x"}}}},
			},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), mprisPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		conn.Close()
		return err
	}

	// Имя на шине публикуем последним, когда объект уже готов отвечать.
	name := "org.mpris.MediaPlayer2.ymusic"
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err == nil && reply != dbus.RequestNameReplyPrimaryOwner {
		name = fmt.Sprintf("%s.instance%d", name, os.Getpid())
		reply, err = conn.RequestName(name, dbus.NameFlagDoNotQueue)
	}
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		if err == nil {
			err = fmt.Errorf("имя %s занято", name)
		}
		return err
	}

	go pl.follow(props)
	return nil
}

// follow переносит состояние плеера в свойства MPRIS; сигналы шлются только при изменениях.
func (p *mprisPlayer) follow(props *prop.Properties) {
	var (
		last     player.State
		first    = true
		lastMeta string
		lastAt   time.Time
	)
	for st := range p.c.eng.Subscribe() {
		p.mu.Lock()
		p.st = st
		p.mu.Unlock()

		set := func(iface, name string, v any) { props.SetMust(iface, name, v) }
		if status := playbackStatus(st); first || status != playbackStatus(last) {
			set(ifacePlayer, "PlaybackStatus", status)
		}
		metaKey := fmt.Sprintf("%v|%s|%d", st.HasTrack, st.Track.ID, int(st.Dur))
		if first || metaKey != lastMeta {
			lastMeta = metaKey
			set(ifacePlayer, "Metadata", metadata(st))
		}
		if first || st.Volume != last.Volume {
			set(ifacePlayer, "Volume", float64(st.Volume)/100)
		}
		if first || st.Repeat != last.Repeat {
			set(ifacePlayer, "LoopStatus", loopStatus(st.Repeat))
		}
		if first || st.Shuffle != last.Shuffle {
			set(ifacePlayer, "Shuffle", st.Shuffle)
		}
		if first || st.HasTrack != last.HasTrack {
			set(ifacePlayer, "CanSeek", st.HasTrack)
		}
		set(ifacePlayer, "Position", int64(st.Pos*1e6))

		// Перемотка (из TUI, трея или по MPRIS) — сигнал Seeked, чтобы виджеты обновили позицию.
		now := time.Now()
		if !first && st.HasTrack && st.Track.ID == last.Track.ID {
			expected := last.Pos
			if !last.Paused {
				expected += now.Sub(lastAt).Seconds()
			}
			if math.Abs(st.Pos-expected) > 2 {
				_ = p.conn.Emit(mprisPath, ifacePlayer+".Seeked", int64(st.Pos*1e6))
			}
		}
		last, lastAt, first = st, now, false
	}
}
