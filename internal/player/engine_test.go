package player

import (
	"io"
	"math"
	"os/exec"
	"testing"
	"time"

	"ymusic/internal/api"
	"ymusic/internal/config"
	"ymusic/internal/stream"
	"ymusic/internal/testutil"
)

// testEnv — кэш с тремя треками и движок с mpv без звука (ao=null), без сети.
func testEnv(t *testing.T, transition string, sec float64) (*Engine, []api.Track) {
	t.Helper()
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv не установлен")
	}
	var tracks []api.Track
	for i := range 3 {
		tracks = append(tracks, api.Track{
			ID:         api.ID([]string{"1", "2", "3"}[i]),
			Title:      []string{"Первый", "Второй", "Третий"}[i],
			DurationMs: int64(sec * 1000),
			R128:       &api.R128{I: -9, TP: -0.5},
		})
	}
	cache := testutil.Cache(t, tracks, sec)
	srv, err := stream.NewServer(nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.SetOffline(true)
	cfg := config.Audio{
		MpvPath: "mpv", Device: "auto", Transition: transition, CrossfadeSec: 1,
		Normalize: true, TargetLUFS: -14, Volume: 50, ExtraArgs: []string{"--ao=null"},
	}
	e, err := New(cfg, "lossless", nil, srv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e, tracks
}

// watch собирает порядок играющих треков, пока очередь не закончится.
func watch(t *testing.T, e *Engine, timeout time.Duration, onState func(State)) (order []api.ID, sawXfade bool, gains map[api.ID]float64) {
	t.Helper()
	gains = map[api.ID]float64{}
	deadline := time.After(timeout)
	for {
		select {
		case st := <-e.Updates():
			if onState != nil {
				onState(st)
			}
			if st.Crossfading {
				sawXfade = true
			}
			if st.HasTrack && (len(order) == 0 || order[len(order)-1] != st.Track.ID) {
				order = append(order, st.Track.ID)
				gains[st.Track.ID] = st.GainDB
			}
			if !st.HasTrack && st.Status == "Очередь закончилась" {
				return
			}
		case <-deadline:
			t.Fatalf("таймаут, сыграно: %v", order)
		}
	}
}

func TestGapless(t *testing.T) {
	e, tracks := testEnv(t, config.TransitionGapless, 1.5)
	e.PlayTracks(tracks, 0, "тест")
	order, xf, gains := watch(t, e, 20*time.Second, nil)
	if len(order) != 3 || order[0] != "1" || order[1] != "2" || order[2] != "3" {
		t.Fatalf("порядок: %v", order)
	}
	if xf {
		t.Fatal("в gapless не должно быть кроссфейда")
	}
	// −14 − (−9) = −5 дБ; ограничение по true peak (−1 − (−0.5) = −0.5) не срабатывает.
	if g := gains["1"]; math.Abs(g+5) > 0.01 {
		t.Fatalf("gain = %v, ожидалось −5", g)
	}
}

func TestCrossfade(t *testing.T) {
	e, tracks := testEnv(t, config.TransitionCrossfade, 3)
	e.PlayTracks(tracks, 0, "тест")
	order, xf, _ := watch(t, e, 25*time.Second, nil)
	if len(order) != 3 || order[2] != "3" {
		t.Fatalf("порядок: %v", order)
	}
	if !xf {
		t.Fatal("кроссфейд не сработал")
	}
}

func TestNextPrev(t *testing.T) {
	e, tracks := testEnv(t, config.TransitionGapless, 6)
	e.PlayTracks(tracks, 0, "тест")
	var order []api.ID
	deadline := time.After(20 * time.Second)
	step := 0
	for step < 4 {
		select {
		case st := <-e.Updates():
			if !st.HasTrack || st.Pos < 0.3 {
				continue
			}
			if len(order) == 0 || order[len(order)-1] != st.Track.ID {
				order = append(order, st.Track.ID)
				switch step {
				case 0, 1:
					e.Next() // 1 → 2 → 3 (второй переход — через уже загруженный в mpv следующий трек)
				case 2:
					e.Prev() // 3 → 2
				}
				step++
			}
		case <-deadline:
			t.Fatalf("таймаут, сыграно: %v", order)
		}
	}
	want := []api.ID{"1", "2", "3", "2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("порядок %v, ожидалось %v", order, want)
		}
	}
}
