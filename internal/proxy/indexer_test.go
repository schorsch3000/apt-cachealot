// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestByHashLinks(t *testing.T) {
	payload := []byte("by hash content")
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(payload)
	}))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	defer s.Close()
	p := httptest.NewServer(s)
	defer p.Close()
	get(t, p.URL+"/"+host+"/d/x.deb") // content now stored
	xc, b := get(t, p.URL+"/"+host+"/debian/dists/t/main/binary-amd64/by-hash/SHA256/"+hexsum)
	if xc != "HIT" || !bytes.Equal(b, payload) || hits.Load() != 1 {
		t.Fatalf("xcache=%q hits=%d", xc, hits.Load())
	}
}

// A second mirror's deb is served from the first mirror's blob without
// any upstream request for the deb, once the second mirror's Packages was seen.
func TestIndexLinksAcrossMirrors(t *testing.T) {
	payload := []byte("the deb content")
	var debHitsB atomic.Int32
	mk := func(debHits *atomic.Int32) (*httptest.Server, string) {
		u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/Packages"):
				w.Write([]byte(packagesFor("pool/x.deb", payload)))
			case strings.HasSuffix(r.URL.Path, ".deb"):
				if debHits != nil {
					debHits.Add(1)
				}
				w.Write(payload)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(u.Close)
		return u, strings.TrimPrefix(u.URL, "http://")
	}
	_, hostA := mk(nil)
	_, hostB := mk(&debHitsB)
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{hostA, hostB}, IndexTTL: time.Hour, MaxSize: 0})
	defer s.Close()
	p := httptest.NewServer(s)
	defer p.Close()

	const pk = "/debian/dists/trixie/main/binary-amd64/Packages"
	// mirror A: index, then the deb (cached)
	get(t, p.URL+"/"+hostA+pk)
	get(t, p.URL+"/"+hostA+"/debian/pool/x.deb")
	// mirror B: only its index is fetched
	get(t, p.URL+"/"+hostB+pk)
	keyB := hostB + "/debian/pool/x.deb"
	deadline := time.Now().Add(2 * time.Second)
	for s.idx.SHA(keyB) == "" && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	xc, b := get(t, p.URL+"/"+hostB+"/debian/pool/x.deb")
	if xc != "HIT" || !bytes.Equal(b, payload) {
		t.Fatalf("xcache=%q body=%q", xc, b)
	}
	if debHitsB.Load() != 0 {
		t.Fatalf("mirror B served the deb %d times, want 0", debHitsB.Load())
	}
}

func TestIndexRebuiltAfterRestart(t *testing.T) {
	payload := []byte("deb")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(packagesFor("pool/x.deb", payload)))
	}))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	dir := t.TempDir()
	s, _ := NewServer(Options{Dir: dir, HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	p := httptest.NewServer(s)
	get(t, p.URL+"/"+host+"/debian/dists/t/main/binary-amd64/Packages")
	p.Close()
	s.Close()

	s2, _ := NewServer(Options{Dir: dir, HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	defer s2.Close()
	key := host + "/debian/pool/x.deb"
	deadline := time.Now().Add(2 * time.Second)
	for s2.idx.SHA(key) == "" && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	if s2.idx.SHA(key) == "" {
		t.Fatal("index not rebuilt after restart")
	}
}
