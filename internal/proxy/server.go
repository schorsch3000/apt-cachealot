// SPDX-License-Identifier: GPL-3.0-or-later

// Package proxy implements the caching HTTP handler: request routing, streaming
// downloads shared between clients, content-addressed storage and statistics.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/schorsch3000/apt-cachealot/internal/lru"
	"github.com/schorsch3000/apt-cachealot/internal/pkgindex"
)

// Server is a caching reverse proxy for apt repositories.
// Request path format: /<host[:port]>/<path>.
type Server struct {
	dir       string
	scheme    string
	parent    string // base URL of the parent instance, or ""
	id        string // random per process, used to detect request loops
	httpHosts map[string]bool
	indexTTL  time.Duration
	client    *http.Client
	bufs      sync.Pool
	mu        sync.Mutex
	flights   map[string]*flight
	lru       *lru.LRU
	idx       *pkgindex.Index
	stats     stats
	parseQ    chan string
	keysMu    sync.Mutex
	indexKeys map[string]bool
	stop      chan struct{}
	stopped   sync.WaitGroup
	closeOnce sync.Once
}

// Options configures a Server.
type Options struct {
	Dir       string        // cache directory
	Scheme    string        // upstream scheme, default "https"
	HTTPHosts []string      // upstream hosts to reach over plain http
	IndexTTL  time.Duration // how long index files stay fresh before revalidation
	MaxSize   int64         // cache size limit in bytes, 0 = unlimited
	// Parent is the base URL of another apt-cachealot (e.g. "http://cache.lan:3142").
	// If set, upstream requests go to it in prefix mode instead of to the mirrors;
	// Scheme and HTTPHosts then only matter on the parent.
	Parent string
}

func NewServer(o Options) (*Server, error) {
	dir, scheme, httpHosts, indexTTL, maxSize := o.Dir, o.Scheme, o.HTTPHosts, o.IndexTTL, o.MaxSize
	if scheme == "" {
		scheme = "https"
	}
	parent := strings.TrimRight(o.Parent, "/")
	if parent != "" {
		u, err := url.Parse(parent)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			return nil, fmt.Errorf("parent %q: want http(s)://host[:port]", o.Parent)
		}
	}
	tmpDir := filepath.Join(dir, "tmp")
	_ = os.RemoveAll(tmpDir) // leftovers of interrupted downloads
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, err
	}
	l := lru.New(dir, maxSize)
	l.Scan()
	hosts := map[string]bool{}
	for _, h := range httpHosts {
		if h = strings.TrimSpace(h); h != "" {
			hosts[h] = true
		}
	}
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true, // pass bytes through untouched
	}
	s := &Server{
		dir:       dir,
		scheme:    scheme,
		parent:    parent,
		id:        newID(),
		httpHosts: hosts,
		indexTTL:  indexTTL,
		client:    &http.Client{Transport: tr},
		flights:   map[string]*flight{},
		bufs:      sync.Pool{New: func() any { b := make([]byte, 128<<10); return &b }},
		lru:       l,
		idx:       pkgindex.New(),
		stats:     stats{started: time.Now()},
		parseQ:    make(chan string, 64),
		indexKeys: map[string]bool{},
		stop:      make(chan struct{}),
	}
	s.stopped.Add(2)
	go s.saveLoop(saveInterval)
	go s.parseLoop()
	s.loadIndexKeys()
	l.EvictIfOver()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" && r.URL.Path == "/_stats" {
		s.serveStats(w)
		return
	}
	if s.loop(r.Header.Get(viaHeader)) {
		http.Error(w, "request loop between apt-cachealot instances (check -parent)", http.StatusLoopDetected)
		return
	}
	s.stats.requests.Add(1)
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT (https through a proxy) cannot be cached; use prefix mode: http://<cache>/<host>/<path>", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host, rest, ok := splitTarget(r.URL)
	if !ok {
		http.Error(w, "expected /<host>/<path> or an absolute http:// proxy request", http.StatusBadRequest)
		return
	}
	key := host + "/" + rest
	index := isIndex(rest)
	via := r.Header.Get(viaHeader)
	ttl := s.requestTTL(r, via)
	cacheable := r.Method == http.MethodGet && r.Header.Get("Range") == ""
	for attempt := 0; attempt < 3; attempt++ {
		// ranges are served from cache on a hit, passed through uncached on a miss
		if f, m, ok := s.lookupFor(key, index, ttl); ok {
			defer f.Close()
			s.stats.hits.Add(1)
			s.serveHit(w, r, f, m)
			return
		}
		if !index && s.link(key, rest) { // same content already cached under another key
			continue
		}
		if !cacheable {
			break
		}
		if !s.follow(w, r, s.join(key, host, rest, r.Header.Get("User-Agent"), via, ttl), host, rest) {
			return
		}
	}
	s.passthrough(w, r, host, rest)
}

// splitTarget extracts upstream host and path from either request form:
// proxy mode (absolute URI, "GET http://host/path") or prefix mode ("GET /host/path").
// Both map to the same cache key.
func splitTarget(u *url.URL) (host, rest string, ok bool) {
	if u.Host != "" { // proxy mode
		if u.Scheme != "http" {
			return "", "", false
		}
		host = strings.TrimSuffix(u.Host, ":80") // upstream scheme is chosen by us, so :80 would be wrong for https
		rest = strings.TrimPrefix(u.Path, "/")
		return host, rest, host != "" && rest != "" && !strings.Contains(host, "@")
	}
	host, rest, found := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if !found || host == "" || rest == "" || strings.Contains(host, "@") {
		return "", "", false
	}
	return host, rest, true
}

// isIndex reports whether the path is a mutable repository index.
// .deb files and by-hash paths are immutable and never revalidated.
func isIndex(rest string) bool {
	return !(strings.HasSuffix(rest, ".deb") || strings.HasSuffix(rest, ".udeb") || strings.Contains(rest, "/by-hash/"))
}

const saveInterval = time.Minute

// saveLoop persists last-use times periodically (only when changed).
func (s *Server) saveLoop(every time.Duration) {
	defer s.stopped.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := s.lru.Save(); err != nil {
				slog.Warn("save state", "err", err)
			}
		case <-s.stop:
			return
		}
	}
}

// Close stops background work and saves last-use times.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.stop)
		s.stopped.Wait()
		err = s.lru.Save()
	})
	return err
}

func (s *Server) serveHit(w http.ResponseWriter, r *http.Request, f *os.File, m ref) {
	if st, err := f.Stat(); err == nil && r.Header.Get("Range") == "" && r.Method == http.MethodGet {
		s.stats.bytesFromCache.Add(st.Size())
	}
	h := w.Header()
	if m.ContentType != "" {
		h.Set("Content-Type", m.ContentType) // set so ServeContent never sniffs
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	if m.ETag != "" {
		h.Set("ETag", m.ETag)
	}
	var mod time.Time
	if m.LastModified != "" {
		mod, _ = http.ParseTime(m.LastModified)
	}
	h.Set("X-Cache", "HIT")
	if _, rest, _ := strings.Cut(m.Key, "/"); isIndex(rest) && !m.Validated.IsZero() {
		h.Set("Age", strconv.Itoa(max(0, int(time.Since(m.Validated).Seconds()))))
	}
	// ServeContent uses sendfile for plain full-body responses
	http.ServeContent(w, r, "", mod, f)
}

func (s *Server) upstreamURL(host, rest string) string {
	if s.parent != "" {
		return s.parent + "/" + host + "/" + rest // the parent picks the scheme
	}
	scheme := s.scheme
	if s.httpHosts[host] {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/%s", scheme, host, rest)
}

// viaHeader lists the instances a request has passed through (comma separated ids).
const viaHeader = "X-Cachealot-Via"

const maxChain = 8

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// setVia marks an upstream request as coming through this instance.
func (s *Server) setVia(req *http.Request, incoming string) {
	if s.parent == "" {
		return
	}
	if incoming != "" {
		incoming += ","
	}
	req.Header.Set(viaHeader, incoming+s.id)
}

// loop reports whether a request already passed through this instance or came
// through too many instances.
func (s *Server) loop(via string) bool {
	if via == "" {
		return false
	}
	ids := strings.Split(via, ",")
	if len(ids) >= maxChain {
		return true
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == s.id {
			return true
		}
	}
	return false
}

// requestTTL is how old an index may be to answer r. A request from another
// instance (it carries the via header) may ask for fresher data with
// Cache-Control: max-age=N; ordinary clients such as apt send max-age=0 on every
// update, which would force an upstream check each time, so it is ignored for them.
func (s *Server) requestTTL(r *http.Request, via string) time.Duration {
	ttl := s.indexTTL
	if via == "" {
		return ttl
	}
	for _, d := range strings.Split(r.Header.Get("Cache-Control"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(d), "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && time.Duration(n)*time.Second < ttl {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return ttl
}

// parentAge reads the Age header of a response from the parent instance: how long
// ago the parent last validated its copy. Index freshness here is measured from
// that moment, so a chain's total staleness does not add up.
func (s *Server) parentAge(h http.Header) time.Duration {
	if s.parent == "" {
		return 0
	}
	n, err := strconv.Atoi(h.Get("Age"))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

var copiedHeaders = []string{"Content-Length", "Content-Type", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"}

// passthrough proxies one request without caching (ranges, HEAD, fallback).
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, host, rest string) {
	s.stats.passthrough.Add(1)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.upstreamURL(host, rest), nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, k := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match", "User-Agent"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	s.setVia(req, r.Header.Get(viaHeader))
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Warn("upstream", "url", req.URL.String(), "err", err)
		s.badGateway(w)
		return
	}
	defer resp.Body.Close()
	h := w.Header()
	for _, k := range copiedHeaders {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("X-Cache", "PASS")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	bp := s.bufs.Get().(*[]byte)
	defer s.bufs.Put(bp)
	rc := http.NewResponseController(w)
	for {
		n, rerr := resp.Body.Read(*bp)
		if n > 0 {
			if _, werr := w.Write((*bp)[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// join returns the flight for key, starting the download if none is running.
func (s *Server) join(key, host, rest, userAgent, via string, ttl time.Duration) *flight {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.flights[key]; ok {
		s.stats.coalesced.Add(1)
		return f
	}
	f := newFlight(key)
	f.via, f.ttl = via, ttl
	s.flights[key] = f
	go s.fill(f, host, rest, userAgent)
	return f
}

// follow streams a flight to one client. It returns true if the caller
// should restart request handling.
func (s *Server) follow(w http.ResponseWriter, r *http.Request, f *flight, host, rest string) (retry bool) {
	select {
	case <-f.ready:
	case <-r.Context().Done():
		return false
	}
	switch {
	case f.retry:
		return true
	case f.stale:
		file, m, _ := s.open(f.key, false)
		if file == nil {
			s.badGateway(w)
			return false
		}
		defer file.Close()
		s.touch(file.Name())
		s.stats.hits.Add(1)
		s.stats.staleServed.Add(1)
		s.serveHit(w, r, file, m)
		return false
	case f.passthrough:
		s.passthrough(w, r, host, rest)
		return false
	case f.err != nil:
		s.badGateway(w)
		return false
	}
	h := w.Header()
	for _, k := range copiedHeaders {
		if v := f.header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("X-Cache", "MISS")
	if f.status != http.StatusOK {
		w.WriteHeader(f.status)
		_, _ = w.Write(f.body)
		return false
	}
	file, err := f.open()
	if err != nil {
		return true // download vanished before we could attach
	}
	defer file.Close()
	w.WriteHeader(http.StatusOK)
	bp := s.bufs.Get().(*[]byte)
	defer s.bufs.Put(bp)
	rc := http.NewResponseController(w)
	var off int64
	for {
		written, done, ok, changed := f.state()
		if off < written {
			for off < written {
				chunk := (*bp)[:min(int64(len(*bp)), written-off)]
				n, rerr := readAt(file, chunk, off)
				if n > 0 {
					if _, werr := w.Write(chunk[:n]); werr != nil {
						return false
					}
					off += int64(n)
				}
				if rerr != nil {
					panic(http.ErrAbortHandler)
				}
			}
			_ = rc.Flush()
			continue
		}
		if done {
			if ok {
				return false
			}
			panic(http.ErrAbortHandler) // headers already sent; make the client see a failure
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return false
		}
	}
}

// fill downloads one object into a temp file and commits it to the cache.
// It runs detached from any client request.
func (s *Server) fill(f *flight, host, rest, userAgent string) {
	ok := false
	defer func() {
		select {
		case <-f.ready: // clients already attached; they see done && !ok
		default:
			f.err = cmpErr(f.err)
			f.setReady()
		}
		s.mu.Lock()
		delete(s.flights, f.key)
		s.mu.Unlock()
		f.finish(ok)
	}()

	// A stale index file is revalidated with a conditional request.
	var validators ref
	haveStale := false
	if file, m, fresh := s.openFor(f.key, isIndex(rest), f.ttl); file != nil {
		file.Close()
		if fresh { // another flight finished just before us
			f.retry = true
			f.setReady()
			return
		}
		validators, haveStale = m, true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchdog := time.AfterFunc(stallTimeout, cancel)
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.upstreamURL(host, rest), nil)
	if err != nil {
		f.err = err
		return
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	s.setVia(req, f.via)
	if s.parent != "" && isIndex(rest) { // ask the parent for data at least as fresh as we would accept
		req.Header.Set("Cache-Control", "max-age="+strconv.Itoa(int(f.ttl.Seconds())))
	}
	if haveStale {
		if validators.ETag != "" {
			req.Header.Set("If-None-Match", validators.ETag)
		}
		if validators.LastModified != "" {
			req.Header.Set("If-Modified-Since", validators.LastModified)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Warn("upstream", "url", req.URL.String(), "err", err, "stale", haveStale)
		f.err, f.stale = err, haveStale
		return
	}
	defer resp.Body.Close()
	if haveStale {
		switch {
		case resp.StatusCode == http.StatusNotModified:
			s.stats.revalidated.Add(1)
			t := time.Now().Add(-s.parentAge(resp.Header))
			_ = os.Chtimes(s.refPath(f.key), t, t)
			f.retry = true
			f.setReady()
			return
		case resp.StatusCode >= 500:
			f.stale = true
			f.setReady()
			return
		}
	}
	f.header, f.status, f.length = resp.Header, resp.StatusCode, resp.ContentLength
	s.stats.misses.Add(1) // a real upstream response, not a 304 revalidation or a stale fallback

	if resp.StatusCode != http.StatusOK {
		// errors and the like are small: keep in memory, share, never cache
		f.body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		f.setReady()
		return
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "dl-*")
	if err != nil {
		slog.Warn("tmpfile", "err", err)
		f.passthrough = true
		f.setReady()
		return
	}
	f.tmpPath = tmp.Name()
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(f.tmpPath)
		}
	}()
	f.setReady()

	buf := make([]byte, 128<<10)
	hash := sha256.New()
	var total, withheld int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(stallTimeout)
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				slog.Warn("cache write", "err", werr)
				return
			}
			total += int64(n)
			s.stats.bytesFromUpstream.Add(int64(n))
			if f.length >= 0 && total == f.length {
				// Hold back the final chunk until committed, so a client that has
				// the whole body can rely on the next request being a hit.
				hash.Write(buf[:n])
				withheld = int64(n)
			} else {
				f.advance(int64(n))
				hash.Write(buf[:n]) // after advance: followers are never held up by hashing
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			slog.Warn("upstream body", "url", req.URL.String(), "err", rerr)
			return
		}
	}
	if f.length >= 0 && total != f.length {
		return
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	f.setBlob(s.blobPath(sum)) // followers fall back to this once the temp file is renamed
	var validated time.Time
	if age := s.parentAge(resp.Header); age > 0 {
		validated = time.Now().Add(-age)
	}
	if err := s.commit(f.tmpPath, f.key, sum, total, ref{
		ContentType:  resp.Header.Get("Content-Type"),
		LastModified: resp.Header.Get("Last-Modified"),
		ETag:         resp.Header.Get("ETag"),
	}, validated); err != nil {
		slog.Warn("commit", "key", f.key, "err", err)
		return
	}
	if pkgindex.IsPackagesIndex(rest) {
		s.schedule(f.key)
	}
	if withheld > 0 {
		f.advance(withheld)
	}
	ok = true
}

const stallTimeout = 60 * time.Second

func cmpErr(err error) error {
	if err != nil {
		return err
	}
	return errors.New("download failed")
}
