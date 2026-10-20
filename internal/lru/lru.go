// SPDX-License-Identifier: GPL-3.0-or-later

// Package lru tracks cached blobs by last delivery and evicts the least recently
// delivered ones when a size limit is exceeded. Last-use times are persisted.
package lru

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// LRU tracks cached objects in memory. Hits only update a timestamp under a
// mutex; deleting happens in a background goroutine once total size exceeds
// max, down to a low watermark so it does not run on every commit.
type LRU struct {
	mu      sync.Mutex
	items   map[string]*item // keyed by data file path
	total   int64
	max     int64 // 0 = unlimited
	running bool
	dirty   bool   // last-use times changed since the last save
	dir     string // cache dir; state keys are relative to it
}

type item struct {
	size int64
	used int64 // unix nanos of last delivery
}

const lowWatermark = 0.9

// New returns an LRU for the cache at dir (blobs live in dir/blobs). max <= 0 means unlimited.
func New(dir string, max int64) *LRU {
	return &LRU{items: map[string]*item{}, max: max, dir: dir}
}

func (l *LRU) statePath() string { return filepath.Join(l.dir, "state.json") }

// Scan loads existing objects after a restart. Last use comes from the saved
// state file, falling back to the file mtime for objects it does not know.
func (l *LRU) Scan() {
	saved := map[string]int64{}
	if b, err := os.ReadFile(l.statePath()); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	_ = filepath.WalkDir(filepath.Join(l.dir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		used, ok := saved[l.rel(p)]
		if !ok {
			used = info.ModTime().UnixNano()
		}
		l.items[p] = &item{size: info.Size(), used: used}
		l.total += info.Size()
		return nil
	})
}

func (l *LRU) rel(p string) string {
	r, err := filepath.Rel(l.dir, p)
	if err != nil {
		return p
	}
	return r
}

// Save writes last-use times atomically if anything changed since the last save.
func (l *LRU) Save() error {
	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	snap := make(map[string]int64, len(l.items))
	for p, it := range l.items {
		snap[l.rel(p)] = it.used
	}
	l.dirty = false
	l.mu.Unlock()

	b, _ := json.Marshal(snap)
	tmp := l.statePath() + ".tmp"
	err := os.WriteFile(tmp, b, 0o644)
	if err == nil {
		err = os.Rename(tmp, l.statePath())
	}
	if err != nil {
		l.mu.Lock()
		l.dirty = true
		l.mu.Unlock()
	}
	return err
}

// Touch marks path as delivered just now.
func (l *LRU) Touch(path string) {
	now := time.Now().UnixNano()
	l.mu.Lock()
	if it, ok := l.items[path]; ok {
		it.used = now
		l.dirty = true
	}
	l.mu.Unlock()
}

// Record registers a newly committed (or replaced) object and may start eviction.
func (l *LRU) Record(path string, size int64) {
	now := time.Now().UnixNano()
	l.mu.Lock()
	if old, ok := l.items[path]; ok {
		l.total -= old.size
	}
	l.items[path] = &item{size: size, used: now}
	l.total += size
	l.dirty = true
	start := l.max > 0 && l.total > l.max && !l.running
	if start {
		l.running = true
	}
	l.mu.Unlock()
	if start {
		go l.evict()
	}
}

// evict removes least recently delivered objects until under the low watermark.
func (l *LRU) evict() {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() { l.running = false }()
	if l.max <= 0 || l.total <= l.max {
		return
	}
	target := int64(float64(l.max) * lowWatermark)
	paths := make([]string, 0, len(l.items))
	for p := range l.items {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool { return l.items[paths[i]].used < l.items[paths[j]].used })
	l.dirty = true
	var removed int
	var freed int64
	for _, p := range paths {
		if l.total <= target {
			break
		}
		it := l.items[p]
		_ = os.Remove(p)
		delete(l.items, p)
		l.total -= it.size
		freed += it.size
		removed++
	}
	slog.Info("evicted", "objects", removed, "bytes", freed, "total", l.total, "max", l.max)
}

// EvictIfOver starts a background eviction if the cache is over its limit,
// e.g. right after a restart with a smaller -max-size.
func (l *LRU) EvictIfOver() {
	l.mu.Lock()
	start := l.max > 0 && l.total > l.max && !l.running
	if start {
		l.running = true
	}
	l.mu.Unlock()
	if start {
		go l.evict()
	}
}

// Usage returns total cached bytes, object count and the limit.
func (l *LRU) Usage() (total int64, objects int, max int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, len(l.items), l.max
}

// Used returns the last delivery time (unix nanos) of path.
func (l *LRU) Used(path string) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it, ok := l.items[path]
	if !ok {
		return 0, false
	}
	return it.used, true
}
