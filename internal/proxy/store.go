// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/heilig/apt-cachealot/internal/pkgindex"
)

// Storage layout, all under the cache dir:
//
//	blobs/<xx>/<sha256>   immutable content, stored once however many URLs point at it
//	refs/<xx>/<h>.json    one per cache key (host/path): which blob, plus response headers
//
// Evicting a blob leaves refs dangling; open() deletes them lazily.

// ref maps a cache key to a blob.
type ref struct {
	Key          string `json:"key"`
	SHA          string `json:"sha256"`
	ContentType  string `json:"content_type,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	ETag         string `json:"etag,omitempty"`

	// Validated is when the object was last confirmed current (the ref file's
	// mtime); not stored in the JSON. Index freshness is measured from it.
	Validated time.Time `json:"-"`
}

func (s *Server) blobPath(sum string) string {
	return filepath.Join(s.dir, "blobs", sum[:2], sum)
}

func (s *Server) refPath(key string) string {
	h := sha256.Sum256([]byte(key))
	x := hex.EncodeToString(h[:])
	return filepath.Join(s.dir, "refs", x[:2], x+".json")
}

func (s *Server) touch(path string) { s.lru.Touch(path) }

func (s *Server) readRef(key string) (ref, time.Time, bool) {
	p := s.refPath(key)
	b, err := os.ReadFile(p)
	if err != nil {
		return ref{}, time.Time{}, false
	}
	var r ref
	if json.Unmarshal(b, &r) != nil || len(r.SHA) != 64 {
		return ref{}, time.Time{}, false
	}
	st, err := os.Stat(p)
	if err != nil {
		return ref{}, time.Time{}, false
	}
	r.Validated = st.ModTime()
	return r, st.ModTime(), true
}

func (s *Server) writeRef(r ref) error {
	p := s.refPath(r.Key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	tmp, err := os.CreateTemp(filepath.Dir(p), "ref-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	return os.Rename(tmp.Name(), p)
}

// open returns the blob for key (nil if none) and whether it is fresh.
// Index files whose ref is older than indexTTL are stale: still usable,
// but must be revalidated.
func (s *Server) open(key string, index bool) (*os.File, ref, bool) {
	return s.openFor(key, index, s.indexTTL)
}

// openFor is open with an explicit freshness limit for index files.
func (s *Server) openFor(key string, index bool, ttl time.Duration) (*os.File, ref, bool) {
	r, mod, ok := s.readRef(key)
	if !ok {
		return nil, ref{}, false
	}
	f, err := os.Open(s.blobPath(r.SHA))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			os.Remove(s.refPath(key)) // blob was evicted
		}
		return nil, ref{}, false
	}
	return f, r, !index || time.Since(mod) <= ttl
}

// lookup returns the cached blob only if it is fresh.
func (s *Server) lookup(key string, index bool) (*os.File, ref, bool) {
	return s.lookupFor(key, index, s.indexTTL)
}

func (s *Server) lookupFor(key string, index bool, ttl time.Duration) (*os.File, ref, bool) {
	f, r, fresh := s.openFor(key, index, ttl)
	if f != nil && !fresh {
		f.Close()
		return nil, ref{}, false
	}
	if f != nil {
		s.touch(f.Name())
	}
	return f, r, f != nil
}

// commit moves a finished download into blobs/ (dropping it if the content
// is already stored) and points key at it.
// If validated is set, the ref counts as last validated then (see Age handling).
func (s *Server) commit(tmp, key, sum string, size int64, r ref, validated time.Time) error {
	blob := s.blobPath(sum)
	if _, err := os.Stat(blob); err == nil {
		os.Remove(tmp) // dedup: same content already stored
	} else {
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, blob); err != nil {
			return err
		}
	}
	r.Key, r.SHA = key, sum
	if err := s.writeRef(r); err != nil {
		return err
	}
	if !validated.IsZero() {
		_ = os.Chtimes(s.refPath(key), validated, validated)
	}
	s.lru.Record(blob, size)
	return nil
}

// link points key at an existing blob when the content is known in advance
// (from a parsed Packages index or a by-hash URL), avoiding the download.
func (s *Server) link(key, rest string) bool {
	sum := s.idx.SHA(key)
	if sum == "" {
		sum = byHashSum(rest)
	}
	if sum == "" {
		return false
	}
	blob := s.blobPath(sum)
	if _, err := os.Stat(blob); err != nil {
		return false
	}
	if err := s.writeRef(ref{Key: key, SHA: sum}); err != nil {
		return false
	}
	s.stats.linked.Add(1)
	if pkgindex.IsPackagesIndex(rest) { // e.g. identical Packages file from a second mirror
		s.schedule(key)
	}
	return true
}

// byHashSum extracts the sha256 from ".../by-hash/SHA256/<hex>" paths.
func byHashSum(rest string) string {
	_, sum, ok := strings.Cut(rest, "/by-hash/SHA256/")
	if !ok || len(sum) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return ""
	}
	return strings.ToLower(sum)
}
