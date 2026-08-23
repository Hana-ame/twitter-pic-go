package gallery

import (
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// HistoryEntry is a single recorded view of a media file.
type HistoryEntry struct {
	MediaID   string    `json:"media_id"`
	Path      string    `json:"path"`
	URL       string    `json:"url,omitempty"`
	Tags      []string  `json:"tags"`
	ViewedAt  time.Time `json:"viewed_at"`
	IP        string    `json:"ip,omitempty"`
	UserAgent string    `json:"user_agent,omitempty"`
}

// HistoryStore keeps a bounded in-memory history and optionally persists
// entries to SQLite.
type HistoryStore struct {
	mu      sync.RWMutex
	entries []HistoryEntry
	max     int

	db *sql.DB
	ch chan HistoryEntry
}

func NewHistoryStore(db *sql.DB, max int) *HistoryStore {
	if max <= 0 {
		max = 2000
	}
	h := &HistoryStore{
		max: max,
		db:  db,
		ch:  make(chan HistoryEntry, 2048),
	}
	if db != nil {
		if err := h.initDB(); err != nil {
			log.Printf("gallery: history db init failed: %v", err)
			h.db = nil
		} else {
			go h.worker()
		}
	}
	return h
}

func (h *HistoryStore) initDB() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS access_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			media_id TEXT NOT NULL,
			path TEXT NOT NULL,
			tags TEXT NOT NULL DEFAULT '[]',
			viewed_at TIMESTAMP NOT NULL,
			ip TEXT,
			user_agent TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_history_media ON access_history(media_id)`,
		`CREATE INDEX IF NOT EXISTS idx_history_time ON access_history(viewed_at)`,
	}
	for _, s := range stmts {
		if _, err := h.db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func (h *HistoryStore) worker() {
	for e := range h.ch {
		if h.db == nil {
			continue
		}
		tagsJSON, _ := json.Marshal(e.Tags)
		_, err := h.db.Exec(
			`INSERT INTO access_history (media_id, path, tags, viewed_at, ip, user_agent)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			e.MediaID, e.Path, string(tagsJSON), e.ViewedAt.UTC().Format(time.RFC3339), e.IP, e.UserAgent,
		)
		if err != nil {
			log.Printf("gallery: insert history failed: %v", err)
		}
	}
}

// Record adds an entry to memory and queues it for persistence.
func (h *HistoryStore) Record(e HistoryEntry) {
	if e.ViewedAt.IsZero() {
		e.ViewedAt = time.Now()
	}
	h.mu.Lock()
	h.entries = append(h.entries, e)
	if len(h.entries) > h.max {
		h.entries = h.entries[len(h.entries)-h.max:]
	}
	h.mu.Unlock()

	if h.db != nil {
		select {
		case h.ch <- e:
		default:
			// Do not block request handling; drop if the queue is full.
		}
	}
}

// Recent returns the last n entries in chronological order (oldest first).
func (h *HistoryStore) Recent(n int) []HistoryEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if n <= 0 || n > len(h.entries) {
		n = len(h.entries)
	}
	out := make([]HistoryEntry, n)
	start := len(h.entries) - n
	copy(out, h.entries[start:])
	return out
}

// Count returns the number of entries currently held in memory.
func (h *HistoryStore) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.entries)
}
