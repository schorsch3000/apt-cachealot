// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T, payload []byte) (proxy *httptest.Server, upstreamHits *atomic.Int32, host string) {
	t.Helper()
	hits := &atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(up.Close)
	host = strings.TrimPrefix(up.URL, "http://")
	s, err := NewServer(Options{Dir: t.TempDir(), HTTPHosts: []string{host}, IndexTTL: time.Hour, MaxSize: 0})
	if err != nil {
		t.Fatal(err)
	}
	p := httptest.NewServer(s)
	t.Cleanup(p.Close)
	return p, hits, host
}

func get(t *testing.T, url string) (string, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Header.Get("X-Cache"), b
}

// rawGet sends a request line verbatim, as an apt proxy client does
// (Go's http.Transport would bypass the proxy for loopback hosts).
func rawGet(t *testing.T, proxyAddr, method, target, host string) (*http.Response, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", method, target, host)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func packagesFor(filename string, payload []byte) string {
	sum := sha256.Sum256(payload)
	return "Package: x\nVersion: 1\nFilename: " + filename + "\nSize: " + fmt.Sprint(len(payload)) +
		"\nSHA256: " + hex.EncodeToString(sum[:]) + "\n\nPackage: other\nFilename: pool/o.deb\nSHA256: " + strings.Repeat("0", 64) + "\n"
}
