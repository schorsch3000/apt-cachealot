// SPDX-License-Identifier: GPL-3.0-or-later

// Package pkgindex parses apt Packages files to learn which sha256 a mirror
// promises for each .deb URL.
package pkgindex

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"encoding/hex"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/ulikunitz/xz"
)

// Index remembers which sha256 a mirror's Packages file promises for each
// .deb URL. A request whose content is already cached under another URL is then
// answered without contacting upstream. Entries are scoped by host and repo
// root, so two repos that reuse a pool path with different content never mix.
type Index struct {
	mu sync.RWMutex
	m  map[string][32]byte // key host/<root><Filename> -> sha256
}

// New returns an empty Index.
func New() *Index { return &Index{m: map[string][32]byte{}} }

// SHA returns the sha256 (hex) promised for key, or "".
func (x *Index) SHA(key string) string {
	x.mu.RLock()
	v, ok := x.m[key]
	x.mu.RUnlock()
	if !ok {
		return ""
	}
	return hex.EncodeToString(v[:])
}

// Add records the sha256 promised for key.
func (x *Index) Add(key string, sum [32]byte) {
	x.mu.Lock()
	x.m[key] = sum
	x.mu.Unlock()
}

// Len returns the number of known URLs.
func (x *Index) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.m)
}

// IsPackagesIndex reports whether rest looks like a binary Packages index,
// plain, compressed, or fetched via by-hash.
func IsPackagesIndex(rest string) bool {
	i := strings.Index(rest, "dists/")
	if i < 0 || (i > 0 && rest[i-1] != '/') || !strings.Contains(rest[i:], "/binary-") {
		return false
	}
	return strings.HasPrefix(path.Base(rest), "Packages") || strings.Contains(rest, "/by-hash/")
}

// RepoRoot returns the path prefix before "dists/" (e.g. "debian/").
func RepoRoot(rest string) string {
	return rest[:strings.Index(rest, "dists/")]
}

// Decompress sniffs the compression format; by-hash URLs carry no extension.
func Decompress(br *bufio.Reader) (io.Reader, error) {
	head, _ := br.Peek(6)
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return gzip.NewReader(br)
	case len(head) >= 6 && string(head) == "\xfd7zXZ\x00":
		return xz.NewReader(br)
	case len(head) >= 3 && string(head[:3]) == "BZh":
		return bzip2.NewReader(br), nil
	}
	return br, nil
}

// Parse reads a Packages file and calls add(prefix+Filename, sha256) per stanza.
func Parse(r io.Reader, prefix string, add func(key string, sum [32]byte)) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var filename, sumHex string
	n := 0
	flush := func() {
		if filename != "" && len(sumHex) == 64 {
			var sum [32]byte
			if _, err := hex.Decode(sum[:], []byte(sumHex)); err == nil {
				add(prefix+filename, sum)
				n++
			}
		}
		filename, sumHex = "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Filename: "):
			filename = strings.TrimSpace(line[len("Filename: "):])
		case strings.HasPrefix(line, "SHA256: "):
			sumHex = strings.TrimSpace(line[len("SHA256: "):])
		}
	}
	flush()
	return n, sc.Err()
}
