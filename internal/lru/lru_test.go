// SPDX-License-Identifier: GPL-3.0-or-later

package lru

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeBlob(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, "blobs", name[:2], name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestEvictsLeastRecentlyDelivered(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, 3000)
	var p [4]string
	for i, n := range []string{"aa1", "bb2", "cc3"} {
		p[i] = writeBlob(t, dir, n, 1000)
		l.Record(p[i], 1000)
		time.Sleep(2 * time.Millisecond)
	}
	l.Touch(p[0]) // aa1 delivered again: bb2 and cc3 are now the oldest
	time.Sleep(2 * time.Millisecond)
	p[3] = writeBlob(t, dir, "dd4", 1000)
	l.Record(p[3], 1000) // 4000 > 3000: evict oldest down to 2700
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if total, _, _ := l.Usage(); total <= 2700 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, want := range []bool{true, false, false, true} {
		if got := exists(p[i]); got != want {
			t.Errorf("blob %d exists=%v, want %v", i, got, want)
		}
		if _, tracked := l.Used(p[i]); tracked != want {
			t.Errorf("blob %d tracked=%v, want %v", i, tracked, want)
		}
	}
}

func TestSaveAndScanKeepUseTimes(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, 0)
	p := writeBlob(t, dir, "ee5", 10)
	l.Record(p, 10)
	time.Sleep(2 * time.Millisecond)
	l.Touch(p)
	want, _ := l.Used(p)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	l2 := New(dir, 0)
	l2.Scan()
	if got, ok := l2.Used(p); !ok || got != want {
		t.Fatalf("after Scan: used=%d ok=%v, want %d", got, ok, want)
	}
	if total, objects, _ := l2.Usage(); total != 10 || objects != 1 {
		t.Fatalf("usage = %d, %d", total, objects)
	}
}

func TestSaveSkipsWhenClean(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, 0)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "state.json")) {
		t.Fatal("state.json written although nothing changed")
	}
}
