// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// flight is one in-progress upstream download shared by all clients
// asking for the same key. A background goroutine (Server.fill) downloads
// into a temp file; every client, including the first, follows that file.
// The download is independent of any client: it completes even if all
// clients disconnect, so the cache still fills.
type flight struct {
	key     string
	via     string        // instance chain of the request that started this download
	ttl     time.Duration // index freshness the starting request needs
	tmpPath string        // set before ready is closed (200 responses only)

	// set before ready is closed, immutable afterwards
	ready       chan struct{}
	readyOnce   sync.Once
	retry       bool // state changed under us (e.g. became a cache hit); restart request handling
	stale       bool // upstream failed; serve the stale cached copy
	passthrough bool // cannot be cached here; client should fetch directly
	err         error
	status      int
	header      http.Header
	length      int64  // -1 if unknown
	body        []byte // small non-200 bodies, served from memory

	mu      sync.Mutex
	blob    string        // final blob path, set just before the temp file is renamed
	written int64         // bytes safely in the temp file
	done    bool          // download finished (ok tells how)
	ok      bool          // download complete and committed
	changed chan struct{} // closed and replaced on every state change
}

func newFlight(key string) *flight {
	return &flight{key: key, ready: make(chan struct{}), changed: make(chan struct{}), length: -1}
}

func (f *flight) setReady() { f.readyOnce.Do(func() { close(f.ready) }) }

func (f *flight) advance(n int64) {
	f.mu.Lock()
	f.written += n
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *flight) finish(ok bool) {
	f.mu.Lock()
	f.done, f.ok = true, ok
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *flight) state() (written int64, done, ok bool, changed <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written, f.done, f.ok, f.changed
}

// open returns the temp file, or the final file if it was already renamed.
func (f *flight) open() (*os.File, error) {
	file, err := os.Open(f.tmpPath)
	if errors.Is(err, os.ErrNotExist) {
		f.mu.Lock()
		blob := f.blob
		f.mu.Unlock()
		if blob != "" {
			return os.Open(blob)
		}
	}
	return file, err
}

func (f *flight) setBlob(p string) {
	f.mu.Lock()
	f.blob = p
	f.mu.Unlock()
}

// readAt reads up to len(p) at off,
// treating a short read at EOF as success.
func readAt(r io.ReaderAt, p []byte, off int64) (int, error) {
	n, err := r.ReadAt(p, off)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}
