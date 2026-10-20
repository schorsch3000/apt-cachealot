// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/heilig/apt-cachealot/internal/pkgindex"
)

// schedule queues a cached Packages index for parsing in the background.
func (s *Server) schedule(key string) {
	select {
	case s.parseQ <- key:
	default:
		slog.Warn("index parse queue full", "key", key)
		return
	}
	s.rememberIndexKey(key)
}

func (s *Server) parseLoop() {
	defer s.stopped.Done()
	for {
		select {
		case key := <-s.parseQ:
			if n, err := s.parseIndex(key); err != nil {
				slog.Warn("parse index", "key", key, "err", err)
			} else {
				slog.Debug("indexed", "key", key, "packages", n)
			}
		case <-s.stop:
			return
		}
	}
}

func (s *Server) parseIndex(key string) (int, error) {
	r, _, ok := s.readRef(key)
	if !ok {
		return 0, nil
	}
	f, err := os.Open(s.blobPath(r.SHA))
	if err != nil {
		return 0, nil // evicted meanwhile
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 128<<10)
	dec, err := pkgindex.Decompress(br)
	if err != nil {
		return 0, err
	}
	host, rest, _ := strings.Cut(key, "/")
	return pkgindex.Parse(dec, host+"/"+pkgindex.RepoRoot(rest), s.idx.Add)
}

// The set of Packages keys is persisted so the index can be rebuilt after a restart.
func (s *Server) indexKeysPath() string { return filepath.Join(s.dir, "indexes.json") }

func (s *Server) rememberIndexKey(key string) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	if s.indexKeys[key] {
		return
	}
	s.indexKeys[key] = true
	keys := make([]string, 0, len(s.indexKeys))
	for k := range s.indexKeys {
		keys = append(keys, k)
	}
	b, _ := json.Marshal(keys)
	tmp := s.indexKeysPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.indexKeysPath())
	}
}

func (s *Server) loadIndexKeys() {
	b, err := os.ReadFile(s.indexKeysPath())
	if err != nil {
		return
	}
	var keys []string
	if json.Unmarshal(b, &keys) != nil {
		return
	}
	for _, k := range keys {
		if _, _, ok := s.readRef(k); ok {
			s.schedule(k)
		}
	}
}
