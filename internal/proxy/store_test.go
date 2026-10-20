// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDedupSameContentTwoURLs(t *testing.T) {
	dir := t.TempDir()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("same bytes")) }))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: dir, HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	defer s.Close()
	p := httptest.NewServer(s)
	defer p.Close()
	get(t, p.URL+"/"+host+"/a/x.deb")
	get(t, p.URL+"/"+host+"/b/y.deb")
	if n := countFiles(t, filepath.Join(dir, "blobs")); n != 1 {
		t.Fatalf("blobs = %d, want 1", n)
	}
	if n := countFiles(t, filepath.Join(dir, "refs")); n != 2 {
		t.Fatalf("refs = %d, want 2", n)
	}
	if xc, _ := get(t, p.URL+"/"+host+"/b/y.deb"); xc != "HIT" {
		t.Fatalf("xcache=%q", xc)
	}
}

func TestEviction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte(r.URL.Path[len(r.URL.Path)-5:]), 200)) // distinct content per path
	}))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	s, _ := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 3000})
	p := httptest.NewServer(s)
	defer p.Close()
	u := func(n string) string { return p.URL + "/" + host + "/pool/" + n + ".deb" }
	cached := func(n string) bool {
		f, _, ok := s.lookup(host+"/pool/"+n+".deb", false)
		if ok {
			f.Close()
		}
		return ok
	}

	fetch := func(n string) { // a client sees the last byte slightly before the commit lands
		get(t, u(n))
		for i := 0; i < 200 && !cached(n); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, n := range []string{"a", "b", "c"} {
		fetch(n)
	}
	get(t, u("a")) // a is now most recently delivered
	time.Sleep(5 * time.Millisecond)
	get(t, u("d")) // total 4000 > 3000: evict oldest (b, c) down to <= 2700

	deadline := time.Now().Add(2 * time.Second)
	for (cached("b") || cached("c")) && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	if cached("b") || cached("c") || !cached("a") || !cached("d") {
		t.Fatalf("a=%v b=%v c=%v d=%v, want a,d only", cached("a"), cached("b"), cached("c"), cached("d"))
	}
}

func TestLastUsePersisted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("data")) }))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	dir := t.TempDir()
	s, _ := NewServer(Options{Dir: dir, HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	p := httptest.NewServer(s)
	defer p.Close()
	get(t, p.URL+"/"+host+"/pool/p.deb")
	time.Sleep(20 * time.Millisecond)
	get(t, p.URL+"/"+host+"/pool/p.deb") // hit: used > mtime
	f, _, _ := s.lookup(host+"/pool/p.deb", false)
	data := f.Name()
	f.Close()
	want, _ := s.lru.Used(data)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, _ := NewServer(Options{Dir: dir, HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	defer s2.Close()
	if got, _ := s2.lru.Used(data); got != want {
		t.Fatalf("used after restart = %d, want %d", got, want)
	}
}
