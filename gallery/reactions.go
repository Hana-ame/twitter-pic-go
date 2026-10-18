package gallery

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

type reactionCounts struct {
	Likes    int `json:"likes"`
	Dislikes int `json:"dislikes"`
}

type reactionStore struct {
	mu   sync.Mutex
	path string
	m    map[string]*reactionCounts
}

func newReactionStore(path string) *reactionStore {
	s := &reactionStore{path: path, m: map[string]*reactionCounts{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &s.m)
		}
	}
	return s
}

func (s *reactionStore) snapshot(keys []string) map[string]reactionCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]reactionCounts, len(keys))
	for _, k := range keys {
		if c := s.m[k]; c != nil {
			out[k] = *c
		} else {
			out[k] = reactionCounts{}
		}
	}
	return out
}

func (s *reactionStore) apply(key string, dl, dd int) reactionCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.m[key]
	if c == nil {
		c = &reactionCounts{}
		s.m[key] = c
	}
	c.Likes += dl
	if c.Likes < 0 {
		c.Likes = 0
	}
	c.Dislikes += dd
	if c.Dislikes < 0 {
		c.Dislikes = 0
	}
	s.saveLocked()
	return *c
}

func (s *reactionStore) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

func handleGetReactions(w http.ResponseWriter, r *http.Request, s *reactionStore) {
	var keys []string
	for _, k := range strings.Split(r.URL.Query().Get("keys"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	writeJSON(w, s.snapshot(keys))
}

func handlePostReact(w http.ResponseWriter, r *http.Request, s *reactionStore) {
	var req struct {
		Key string `json:"key"`
		DL  int    `json:"dl"`
		DD  int    `json:"dd"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	c := s.apply(req.Key, req.DL, req.DD)
	writeJSON(w, map[string]any{"key": req.Key, "likes": c.Likes, "dislikes": c.Dislikes})
}
