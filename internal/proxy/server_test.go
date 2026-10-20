// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMissThenHit(t *testing.T) {
	payload := bytes.Repeat([]byte("deb"), 1<<20)
	p, hits, host := newTestServer(t, payload)
	url := p.URL + "/" + host + "/pool/main/a/a.deb"

	xc, body := get(t, url)
	if xc != "MISS" || !bytes.Equal(body, payload) {
		t.Fatalf("cold: xcache=%q equal=%v", xc, bytes.Equal(body, payload))
	}
	xc, body = get(t, url)
	if xc != "HIT" || !bytes.Equal(body, payload) {
		t.Fatalf("warm: xcache=%q equal=%v", xc, bytes.Equal(body, payload))
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

func TestRangeOnHit(t *testing.T) {
	p, _, host := newTestServer(t, []byte("0123456789"))
	url := p.URL + "/" + host + "/pool/x.deb"
	get(t, url)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(b) != "234" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, b)
	}
}

func TestBadPath(t *testing.T) {
	p, _, _ := newTestServer(t, nil)
	resp, _ := http.Get(p.URL + "/onlyhost")
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestCoalesce(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	hits := &atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "1048576")
		for i := 0; i < len(payload); i += 128 << 10 {
			w.Write(payload[i : i+128<<10])
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	p := httptest.NewServer(s)
	defer p.Close()

	const n = 8
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			time.Sleep(time.Duration(i) * 15 * time.Millisecond) // join mid-download
			_, b := get(t, p.URL+"/"+host+"/pool/c.deb")
			if !bytes.Equal(b, payload) {
				errs <- "client got wrong body"
				return
			}
			errs <- ""
		}(i)
	}
	for i := 0; i < n; i++ {
		if e := <-errs; e != "" {
			t.Fatal(e)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

func TestNotFoundNotCached(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	p := httptest.NewServer(s)
	defer p.Close()
	resp, _ := http.Get(p.URL + "/" + host + "/nope.deb")
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if _, _, ok := s.lookup(host+"/nope.deb", false); ok {
		t.Fatal("404 was cached")
	}
}

func TestTruncatedUpstreamNotCached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("short"))
	}))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	p := httptest.NewServer(s)
	defer p.Close()
	if resp, err := http.Get(p.URL + "/" + host + "/t.deb"); err == nil {
		_, err = io.ReadAll(resp.Body)
		if err == nil {
			t.Fatal("client saw truncated body as success")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, ok := s.lookup(host+"/t.deb", false); ok {
		t.Fatal("truncated download was cached")
	}
}

func TestIndexRevalidation(t *testing.T) {
	var full, cond atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			cond.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte("release-v1"))
	}))
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: 200 * time.Millisecond, MaxSize: 0})
	p := httptest.NewServer(s)
	defer p.Close()
	url := p.URL + "/" + host + "/debian/dists/x/Release"

	get(t, url)
	time.Sleep(300 * time.Millisecond) // index now stale
	xc, b := get(t, url)
	if xc != "HIT" || string(b) != "release-v1" {
		t.Fatalf("revalidated: xcache=%q body=%q", xc, b)
	}
	if full.Load() != 1 || cond.Load() != 1 {
		t.Fatalf("full=%d cond=%d, want 1 and 1", full.Load(), cond.Load())
	}

	// upstream down: stale copy is served
	up.Close()
	time.Sleep(300 * time.Millisecond)
	xc, b = get(t, url)
	if xc != "HIT" || string(b) != "release-v1" {
		t.Fatalf("stale: xcache=%q body=%q", xc, b)
	}
}

func TestProxyMode(t *testing.T) {
	p, hits, host := newTestServer(t, []byte("proxied"))
	addr := strings.TrimPrefix(p.URL, "http://")

	resp, b := rawGet(t, addr, "GET", "http://"+host+"/pool/m.deb", host)
	if resp.StatusCode != 200 || string(b) != "proxied" || resp.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("proxy miss: %d %q %q", resp.StatusCode, b, resp.Header.Get("X-Cache"))
	}
	// same object requested in prefix mode shares the cache entry
	xc, b := get(t, p.URL+"/"+host+"/pool/m.deb")
	if xc != "HIT" || string(b) != "proxied" || hits.Load() != 1 {
		t.Fatalf("prefix after proxy: xcache=%q hits=%d", xc, hits.Load())
	}
	resp, _ = rawGet(t, addr, "CONNECT", host, host)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("CONNECT status=%d", resp.StatusCode)
	}
}

// A small instance in front of a big one: misses go to the parent, which is the
// only one talking to the mirror.
func TestParentChain(t *testing.T) {
	payload := bytes.Repeat([]byte("p"), 4096)
	var mirrorHits atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		w.Header().Set("ETag", `"e1"`)
		http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer mirror.Close()
	host := strings.TrimPrefix(mirror.URL, "http://")

	parent, err := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	ps := httptest.NewServer(parent)
	defer ps.Close()

	childDir := t.TempDir()
	child, err := NewServer(Options{Dir: childDir, IndexTTL: time.Hour, Parent: ps.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	cs := httptest.NewServer(child)
	defer cs.Close()
	url := cs.URL + "/" + host + "/pool/c.deb"

	if xc, b := get(t, url); xc != "MISS" || !bytes.Equal(b, payload) {
		t.Fatalf("first: xcache=%q len=%d", xc, len(b))
	}
	if xc, _ := get(t, url); xc != "HIT" {
		t.Fatalf("child second: xcache=%q", xc)
	}
	// child loses its cache (small disk): the parent still has it
	os.RemoveAll(filepath.Join(childDir, "blobs"))
	os.RemoveAll(filepath.Join(childDir, "refs"))
	if xc, b := get(t, url); xc != "MISS" || !bytes.Equal(b, payload) {
		t.Fatalf("after child wipe: xcache=%q len=%d", xc, len(b))
	}
	if mirrorHits.Load() != 1 {
		t.Fatalf("mirror hits = %d, want 1", mirrorHits.Load())
	}
}

func TestParentLoopDetected(t *testing.T) {
	// an instance configured as its own parent must fail fast, not hang or recurse
	hs := httptest.NewUnstartedServer(nil)
	s, err := NewServer(Options{Dir: t.TempDir(), IndexTTL: time.Hour, Parent: "http://" + hs.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hs.Config.Handler = s
	hs.Start()
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/example.org/pool/x.deb")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusLoopDetected {
		t.Fatalf("status = %d, want 508", resp.StatusCode)
	}
}

func TestBadParentURL(t *testing.T) {
	for _, p := range []string{"cache.lan:3142", "ftp://x", "http://x/path"} {
		if _, err := NewServer(Options{Dir: t.TempDir(), Parent: p}); err == nil {
			t.Errorf("parent %q accepted", p)
		}
	}
}

// chain builds mirror <- parent <- child. The mirror counts full and conditional requests.
type chain struct {
	parent, child       *Server
	parentURL, childURL string
	host                string
	full, cond          *atomic.Int32
}

func newChain(t *testing.T, parentTTL, childTTL time.Duration) *chain {
	t.Helper()
	c := &chain{full: &atomic.Int32{}, cond: &atomic.Int32{}}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			c.cond.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.full.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte("release"))
	}))
	t.Cleanup(mirror.Close)
	c.host = strings.TrimPrefix(mirror.URL, "http://")
	var err error
	c.parent, err = NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{c.host}, IndexTTL: parentTTL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.parent.Close() })
	ps := httptest.NewServer(c.parent)
	t.Cleanup(ps.Close)
	c.child, err = NewServer(Options{Dir: t.TempDir(), IndexTTL: childTTL, Parent: ps.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.child.Close() })
	cs := httptest.NewServer(c.child)
	t.Cleanup(cs.Close)
	c.parentURL, c.childURL = ps.URL, cs.URL
	return c
}

func (c *chain) key() string  { return c.host + "/debian/dists/x/Release" }
func (c *chain) path() string { return "/" + c.key() }

// age makes the parent's copy look validated d ago; forget empties the child's cache.
func (c *chain) age(t *testing.T, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(c.parent.refPath(c.key()), old, old); err != nil {
		t.Fatal(err)
	}
}
func (c *chain) forget() {
	os.RemoveAll(filepath.Join(c.child.dir, "refs"))
	os.RemoveAll(filepath.Join(c.child.dir, "blobs"))
}
func (c *chain) childAge(t *testing.T) time.Duration {
	t.Helper()
	st, err := os.Stat(c.child.refPath(c.key()))
	if err != nil {
		t.Fatal(err)
	}
	return time.Since(st.ModTime())
}

// The child takes over the parent's idea of how old the index is, so
// the two TTLs do not add up.
func TestChainAgeIsPropagated(t *testing.T) {
	c := newChain(t, time.Hour, time.Hour)
	get(t, c.childURL+c.path())
	c.age(t, 100*time.Second)
	c.forget()
	get(t, c.childURL+c.path())
	if a := c.childAge(t); a < 99*time.Second || a > 110*time.Second {
		t.Fatalf("child's index age = %v, want about 100s (the parent's)", a)
	}
}

// A child that accepts only fresher data than the parent's TTL makes the parent
// revalidate instead of answering with a copy the child would discard at once.
func TestChainMaxAgeForcesParentRevalidation(t *testing.T) {
	c := newChain(t, time.Hour, 10*time.Second)
	get(t, c.childURL+c.path())
	c.age(t, 100*time.Second)
	c.forget()
	get(t, c.childURL+c.path())
	if c.cond.Load() != 1 || c.full.Load() != 1 {
		t.Fatalf("mirror full=%d cond=%d, want 1 and 1", c.full.Load(), c.cond.Load())
	}
	if a := c.childAge(t); a > 5*time.Second {
		t.Fatalf("child's index age = %v, want fresh", a)
	}
}

// Ordinary clients (apt sends max-age=0 on update) cannot force upstream checks.
func TestMaxAgeIgnoredWithoutChainHeader(t *testing.T) {
	c := newChain(t, time.Hour, time.Hour)
	get(t, c.childURL+c.path())
	c.age(t, 100*time.Second)
	req, _ := http.NewRequest("GET", c.parentURL+c.path(), nil)
	req.Header.Set("Cache-Control", "max-age=0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Cache") != "HIT" || resp.Header.Get("Age") == "" || c.cond.Load() != 0 {
		t.Fatalf("xcache=%q age=%q cond=%d", resp.Header.Get("X-Cache"), resp.Header.Get("Age"), c.cond.Load())
	}
}

// Revalidation (304) from the parent also carries the parent's age.
func TestChainAgeIsPropagatedOnRevalidation(t *testing.T) {
	c := newChain(t, time.Hour, time.Hour)
	get(t, c.childURL+c.path())
	c.age(t, 100*time.Second) // parent: validated 100s ago, still fresh
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.child.refPath(c.key()), old, old); err != nil { // child: stale
		t.Fatal(err)
	}
	if xc, b := get(t, c.childURL+c.path()); xc != "HIT" || string(b) != "release" {
		t.Fatalf("xcache=%q body=%q", xc, b)
	}
	if a := c.childAge(t); a < 99*time.Second || a > 110*time.Second {
		t.Fatalf("child's index age = %v, want about 100s", a)
	}
	if c.cond.Load() != 0 {
		t.Fatalf("mirror was asked %d times; the parent's copy was fresh", c.cond.Load())
	}
}
