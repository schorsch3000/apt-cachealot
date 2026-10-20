// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestStatsEndpoint(t *testing.T) {
	p, _, host := newTestServer(t, bytes.Repeat([]byte("s"), 500))
	url := p.URL + "/" + host + "/pool/s.deb"
	get(t, url) // miss
	get(t, url) // hit

	resp, err := http.Get(p.URL + "/_stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{
		"requests": 2, "hits": 1, "misses": 1, "hit_rate": 0.5,
		"bytes_from_cache": 500, "bytes_from_upstream": 500, "byte_hit_rate": 0.5,
		"cache_bytes": 500, "cache_objects": 1, "downloads_in_flight": 0,
	} {
		if st[k] != any(want) {
			t.Errorf("%s = %v, want %v", k, st[k], want)
		}
	}
	if free, _ := st["disk_free_bytes"].(float64); free <= 0 {
		t.Errorf("disk_free_bytes = %v", st["disk_free_bytes"])
	}
}
