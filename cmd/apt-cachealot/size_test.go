// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "500": 500, "1K": 1024, "2MB": 2 << 20, "20GB": 20 << 30, "1.5GiB": 3 << 29, "1T": 1 << 40} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
