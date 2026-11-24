// Package likes — общее для TUI и трея множество лайкнутых треков.
package likes

import (
	"context"
	"sync"

	"ymusic/internal/api"
)

type Store struct {
	api *api.Client

	mu      sync.RWMutex
	ids     map[api.ID]bool
	version int
}

func New(client *api.Client) *Store {
	return &Store{api: client, ids: map[api.ID]bool{}}
}

// Load загружает «Мне нравится» с сервера.
func (s *Store) Load(ctx context.Context) error {
	shorts, err := s.api.LikedTrackIDs(ctx)
	if err != nil {
		return err
	}
	ids := make(map[api.ID]bool, len(shorts))
	for _, t := range shorts {
		ids[t.ID] = true
	}
	s.mu.Lock()
	s.ids = ids
	s.version++
	s.mu.Unlock()
	return nil
}

func (s *Store) Liked(id api.ID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ids[id]
}

// Version меняется при каждом изменении — по нему интерфейс понимает, что список лайков устарел.
func (s *Store) Version() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Toggle ставит или снимает лайк и возвращает новое состояние.
func (s *Store) Toggle(ctx context.Context, t api.Track) (bool, error) {
	like := !s.Liked(t.ID)
	var err error
	if like {
		err = s.api.LikeTrack(ctx, t.Key())
	} else {
		err = s.api.UnlikeTrack(ctx, t.Key())
	}
	if err != nil {
		return !like, err
	}
	s.mu.Lock()
	if like {
		s.ids[t.ID] = true
	} else {
		delete(s.ids, t.ID)
	}
	s.version++
	s.mu.Unlock()
	return like, nil
}
