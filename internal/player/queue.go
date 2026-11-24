package player

import (
	"math/rand"

	"ymusic/internal/api"
)

type Repeat int

const (
	RepeatOff Repeat = iota
	RepeatAll
	RepeatOne
)

func (r Repeat) String() string {
	switch r {
	case RepeatAll:
		return "повтор всех"
	case RepeatOne:
		return "повтор трека"
	}
	return "без повтора"
}

// Queue — очередь воспроизведения. pos — позиция в порядке проигрывания (order), а не в tracks.
type Queue struct {
	tracks  []api.Track
	order   []int
	pos     int
	shuffle bool
	repeat  Repeat
	source  string
	version int
}

func (q *Queue) Set(tracks []api.Track, start int, source string) {
	q.tracks = append([]api.Track(nil), tracks...)
	q.source = source
	q.order = make([]int, len(tracks))
	for i := range q.order {
		q.order[i] = i
	}
	q.pos = 0
	if start >= 0 && start < len(tracks) {
		q.pos = start
	}
	if q.shuffle {
		q.reshuffle()
	}
	q.version++
}

// reshuffle перемешивает очередь, оставляя текущий трек первым.
func (q *Queue) reshuffle() {
	if len(q.order) == 0 {
		return
	}
	cur := q.order[q.pos]
	rest := make([]int, 0, len(q.order)-1)
	for _, i := range q.order {
		if i != cur {
			rest = append(rest, i)
		}
	}
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	q.order = append([]int{cur}, rest...)
	q.pos = 0
}

func (q *Queue) SetShuffle(on bool) {
	if q.shuffle == on {
		return
	}
	q.shuffle = on
	if len(q.order) == 0 {
		return
	}
	if on {
		q.reshuffle()
	} else {
		cur := q.order[q.pos]
		for i := range q.order {
			q.order[i] = i
		}
		q.pos = cur
	}
	q.version++
}

func (q *Queue) Len() int { return len(q.order) }

func (q *Queue) Pos() int { return q.pos }

func (q *Queue) SetPos(p int) {
	if p >= 0 && p < len(q.order) {
		q.pos = p
	}
}

func (q *Queue) At(p int) (api.Track, bool) {
	if p < 0 || p >= len(q.order) {
		return api.Track{}, false
	}
	return q.tracks[q.order[p]], true
}

// NextPos — позиция следующего трека с учётом повтора. Для RepeatOne возвращает текущую.
func (q *Queue) NextPos(manual bool) (int, bool) {
	if len(q.order) == 0 {
		return 0, false
	}
	if q.repeat == RepeatOne && !manual {
		return q.pos, true
	}
	if q.pos+1 < len(q.order) {
		return q.pos + 1, true
	}
	if q.repeat != RepeatOff {
		return 0, true
	}
	return 0, false
}

func (q *Queue) PrevPos() (int, bool) {
	if len(q.order) == 0 {
		return 0, false
	}
	if q.pos > 0 {
		return q.pos - 1, true
	}
	if q.repeat == RepeatAll {
		return len(q.order) - 1, true
	}
	return 0, false
}

// Append добавляет треки в конец (используется волной), пропуская уже имеющиеся.
func (q *Queue) Append(tracks []api.Track) int {
	seen := make(map[api.ID]bool, len(q.tracks))
	for _, t := range q.tracks {
		seen[t.ID] = true
	}
	added := 0
	for _, t := range tracks {
		if seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		q.tracks = append(q.tracks, t)
		q.order = append(q.order, len(q.tracks)-1)
		added++
	}
	if added > 0 {
		q.version++
	}
	return added
}

// Items — треки в порядке проигрывания.
func (q *Queue) Items() []api.Track {
	out := make([]api.Track, len(q.order))
	for i, idx := range q.order {
		out[i] = q.tracks[idx]
	}
	return out
}
