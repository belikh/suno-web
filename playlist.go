package main

// User-made playlists.
//
// These are the only irreplaceable data this service owns: every other byte it
// serves can be re-derived from Suno by the backup daemon, but a hand-curated
// playlist cannot. They therefore live in the service's StateDirectory, NOT
// inside /tank/archive/suno — writing there would break that dataset's role as
// a faithful mirror (and it is snapshotted by sanoid as such).
//
// Written temp-then-rename under a mutex, matching the suno-backup daemon's
// atomicWriteJSON discipline.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Playlist struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Note     string    `json:"note,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	TrackIDs []string  `json:"trackIds"`
}

type PlaylistStore struct {
	path string
	mu   sync.RWMutex
	byID map[string]*Playlist
	seq  int
}

var errNotFound = errors.New("playlist not found")

func NewPlaylistStore(path string) (*PlaylistStore, error) {
	s := &PlaylistStore{path: path, byID: make(map[string]*Playlist)}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *PlaylistStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var lists []*Playlist
	if err := json.Unmarshal(b, &lists); err != nil {
		return err
	}
	for _, p := range lists {
		s.byID[p.ID] = p
		var n int
		if _, err := fmt.Sscanf(p.ID, "pl%d", &n); err == nil && n > s.seq {
			s.seq = n
		}
	}
	return nil
}

// saveLocked persists the store. Caller holds the write lock.
func (s *PlaylistStore) saveLocked() error {
	lists := make([]*Playlist, 0, len(s.byID))
	for _, p := range s.byID {
		lists = append(lists, p)
	}
	sort.Slice(lists, func(i, j int) bool { return lists[i].Created.Before(lists[j].Created) })

	b, err := json.MarshalIndent(lists, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *PlaylistStore) List() []*Playlist {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Playlist, 0, len(s.byID))
	for _, p := range s.byID {
		cp := *p
		cp.TrackIDs = append([]string(nil), p.TrackIDs...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (s *PlaylistStore) Get(id string) (*Playlist, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	cp := *p
	cp.TrackIDs = append([]string(nil), p.TrackIDs...)
	return &cp, true
}

func (s *PlaylistStore) Create(name, note string, ids []string) (*Playlist, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	now := time.Now().UTC()
	p := &Playlist{
		ID:       fmt.Sprintf("pl%d", s.seq),
		Name:     name,
		Note:     strings.TrimSpace(note),
		Created:  now,
		Updated:  now,
		TrackIDs: dedupe(ids),
	}
	s.byID[p.ID] = p
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *p
	return &cp, nil
}

// Update applies a partial edit. Nil fields are left alone.
func (s *PlaylistStore) Update(id string, name, note *string, ids []string) (*Playlist, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return nil, errNotFound
	}
	if name != nil {
		if strings.TrimSpace(*name) == "" {
			return nil, errors.New("name required")
		}
		p.Name = strings.TrimSpace(*name)
	}
	if note != nil {
		p.Note = strings.TrimSpace(*note)
	}
	if ids != nil {
		p.TrackIDs = dedupe(ids)
	}
	p.Updated = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *p
	cp.TrackIDs = append([]string(nil), p.TrackIDs...)
	return &cp, nil
}

// Add appends tracks, skipping ones already present (order preserved).
func (s *PlaylistStore) Add(id string, ids []string) (*Playlist, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return nil, errNotFound
	}
	have := make(map[string]bool, len(p.TrackIDs))
	for _, t := range p.TrackIDs {
		have[t] = true
	}
	for _, t := range ids {
		if t != "" && !have[t] {
			p.TrackIDs = append(p.TrackIDs, t)
			have[t] = true
		}
	}
	p.Updated = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *p
	cp.TrackIDs = append([]string(nil), p.TrackIDs...)
	return &cp, nil
}

func (s *PlaylistStore) Remove(id string, ids []string) (*Playlist, error) {
	drop := make(map[string]bool, len(ids))
	for _, t := range ids {
		drop[t] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return nil, errNotFound
	}
	kept := p.TrackIDs[:0]
	for _, t := range p.TrackIDs {
		if !drop[t] {
			kept = append(kept, t)
		}
	}
	p.TrackIDs = kept
	p.Updated = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *p
	cp.TrackIDs = append([]string(nil), p.TrackIDs...)
	return &cp, nil
}

func (s *PlaylistStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return errNotFound
	}
	delete(s.byID, id)
	return s.saveLocked()
}

func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
