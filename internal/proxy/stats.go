// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"
)

// stats are in-memory counters since process start.
type stats struct {
	started time.Time

	requests        atomic.Int64 // all proxied requests (not /_stats)
	hits            atomic.Int64 // served from cache, incl. linked and stale
	misses          atomic.Int64 // upstream responses that needed a download (not 304 revalidations)
	coalesced       atomic.Int64 // joined a download already running
	passthrough     atomic.Int64 // uncached (ranges, HEAD, fallback)
	errors          atomic.Int64 // 502s
	linked          atomic.Int64 // hits that avoided a download via Packages / by-hash
	revalidated     atomic.Int64 // index refreshed with a 304
	staleServed     atomic.Int64 // stale index served because upstream failed
	parentFallbacks atomic.Int64 // requests sent to the mirrors because the parent was unreachable

	bytesFromCache    atomic.Int64 // size of cached files delivered (range requests not counted)
	bytesFromUpstream atomic.Int64
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func (s *Server) badGateway(w http.ResponseWriter) {
	s.stats.errors.Add(1)
	http.Error(w, "upstream error", http.StatusBadGateway)
}

// serveStats answers GET /_stats with JSON.
func (s *Server) serveStats(w http.ResponseWriter) {
	st := &s.stats
	hits, misses, coal := st.hits.Load(), st.misses.Load(), st.coalesced.Load()
	fromCache, fromUp := st.bytesFromCache.Load(), st.bytesFromUpstream.Load()

	cacheBytes, objects, maxBytes := s.lru.Usage()
	s.mu.Lock()
	inFlight := len(s.flights)
	s.mu.Unlock()
	indexed := s.idx.Len()

	var free int64 = -1
	var fs syscall.Statfs_t
	if syscall.Statfs(s.dir, &fs) == nil {
		free = int64(fs.Bavail) * int64(fs.Bsize)
	}

	out := map[string]any{
		"version":          s.version,
		"uptime_seconds":   int64(time.Since(st.started).Seconds()),
		"parent":           s.parent,
		"parent_down":      s.parentDown(),
		"parent_fallbacks": st.parentFallbacks.Load(),
		"requests":         st.requests.Load(),
		"hits":             hits,
		"misses":           misses,
		"coalesced":        coal,
		"passthrough":      st.passthrough.Load(),
		"errors":           st.errors.Load(),
		// share of cacheable requests answered without waiting for a new download
		"hit_rate":                ratio(hits, hits+misses+coal),
		"linked_without_download": st.linked.Load(),
		"index_revalidated":       st.revalidated.Load(),
		"stale_served":            st.staleServed.Load(),
		"bytes_from_cache":        fromCache,
		"bytes_from_upstream":     fromUp,
		"byte_hit_rate":           ratio(fromCache, fromCache+fromUp),
		"cache_bytes":             cacheBytes,
		"cache_max_bytes":         maxBytes,
		"cache_objects":           objects,
		"downloads_in_flight":     inFlight,
		"indexed_package_urls":    indexed,
		"disk_free_bytes":         free,
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
