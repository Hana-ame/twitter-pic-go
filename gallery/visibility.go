package gallery

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Hana-ame/twitter-pic-go/tags"
)

const visTTL = 60 * time.Second

type vis struct {
	mu    sync.Mutex
	store *tags.Store
	ttl   time.Duration
	at    time.Time

	banned    map[string]struct{}
	cloudView []tags.Count
	haveView  bool
	warned    bool
}

func newVis(store *tags.Store) *vis {
	return &vis{store: store, ttl: visTTL}
}

func (v *vis) invalidate() {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.at = time.Time{}
	v.mu.Unlock()
}

func (v *vis) refreshLocked() {
	if v.store == nil || (!v.at.IsZero() && time.Since(v.at) < v.ttl) {
		return
	}
	list, err := v.store.BannedUsernames()
	if err != nil {
		if !errors.Is(err, tags.ErrNoStore) || !v.warned {
			log.Printf("gallery: 封禁账号视图刷新失败，暂不隐身（fail-open）: %v", err)
			v.warned = true
		}
		return
	}
	m := make(map[string]struct{}, len(list))
	for _, u := range list {
		m[u] = struct{}{}
	}
	v.banned = m
	v.cloudView = v.store.Cloud(cloudTopN, true)
	v.at = time.Now()
	v.haveView = true
	v.warned = false
}

func (v *vis) hidden(name string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshLocked()
	if !v.haveView {
		return false
	}
	_, ok := v.banned[name]
	return ok
}

func (v *vis) filter(names []string) []string {
	if v == nil {
		return names
	}
	v.mu.Lock()
	v.refreshLocked()
	haveView, banned := v.haveView, v.banned
	v.mu.Unlock()
	if !haveView {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, isBanned := banned[n]; isBanned {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (v *vis) cloud() []tags.Count {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshLocked()
	return v.cloudView
}
