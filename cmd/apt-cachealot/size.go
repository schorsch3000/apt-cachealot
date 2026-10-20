// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strconv"
	"strings"
)

// parseSize parses sizes like "20GB", "500M", "1.5TiB" (all powers of 1024). "0" means unlimited.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := int64(1)
	if n := len(s); n > 0 {
		if i := strings.IndexByte("KMGT", s[n-1]); i >= 0 {
			mult = int64(1) << (10 * (i + 1))
			s = s[:n-1]
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int64(v * float64(mult)), nil
}
