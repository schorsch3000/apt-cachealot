// SPDX-License-Identifier: GPL-3.0-or-later

package pkgindex

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

func packagesFor(filename string, payload []byte) string {
	sum := sha256.Sum256(payload)
	return "Package: x\nVersion: 1\nFilename: " + filename + "\nSize: " + fmt.Sprint(len(payload)) +
		"\nSHA256: " + hex.EncodeToString(sum[:]) + "\n\nPackage: other\nFilename: pool/o.deb\nSHA256: " + strings.Repeat("0", 64) + "\n"
}

func TestParsePackagesFormats(t *testing.T) {
	text := packagesFor("pool/x.deb", []byte("c"))
	var gz, xzb, bz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write([]byte(text))
	gw.Close()
	xw, _ := xz.NewWriter(&xzb)
	xw.Write([]byte(text))
	xw.Close()
	_ = bz
	for name, data := range map[string][]byte{"plain": []byte(text), "gzip": gz.Bytes(), "xz": xzb.Bytes()} {
		dec, err := Decompress(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]bool{}
		n, err := Parse(dec, "h/debian/", func(k string, _ [32]byte) { got[k] = true })
		if err != nil || n != 2 || !got["h/debian/pool/x.deb"] {
			t.Fatalf("%s: n=%d err=%v got=%v", name, n, err, got)
		}
	}
}

func TestIsPackagesIndex(t *testing.T) {
	for in, want := range map[string]bool{
		"debian/dists/trixie/main/binary-amd64/Packages.xz":         true,
		"dists/trixie/main/binary-amd64/Packages":                   true,
		"debian/dists/trixie/main/binary-amd64/by-hash/SHA256/ab":   true,
		"debian/dists/trixie/main/binary-amd64/Release":             false,
		"debian/dists/trixie/main/binary-amd64/Packages.diff/Index": false,
		"debian/dists/trixie/InRelease":                             false,
		"debian/pool/main/c/coreutils/coreutils_1_amd64.deb":        false,
		"debian/notdists/trixie/main/binary-amd64/Packages":         false,
	} {
		if got := IsPackagesIndex(in); got != want {
			t.Errorf("IsPackagesIndex(%q) = %v, want %v", in, got, want)
		}
	}
}
