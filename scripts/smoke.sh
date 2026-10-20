#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# End-to-end check of a running instance: miss, hit, proxy mode, stats.
# usage: smoke.sh <cache-base-url> <upstream host:port>
# The upstream must be plain http, listed in the instance's -http-hosts, and serve /pool/smoke.deb.
set -euo pipefail
cache=${1:?usage: smoke.sh http://localhost:3142 127.0.0.1:8000}
up=${2:?usage: smoke.sh http://localhost:3142 127.0.0.1:8000}
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# fetch a URL (extra curl args after it), print its X-Cache header, body goes to $tmp/body
xcache() {
  curl -fsS -D - -o "$tmp/body" "$@" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-cache" {print $2}'
}

x=$(xcache "$cache/$up/pool/smoke.deb"); [ "$x" = MISS ] || fail "first request: X-Cache=$x, want MISS"
mv "$tmp/body" "$tmp/first"
[ -s "$tmp/first" ] || fail "empty body"

x=$(xcache "$cache/$up/pool/smoke.deb"); [ "$x" = HIT ] || fail "second request: X-Cache=$x, want HIT"
cmp "$tmp/first" "$tmp/body" || fail "cached body differs from the first download"

# proxy mode shares the cache entry
x=$(xcache -x "$cache" "http://$up/pool/smoke.deb"); [ "$x" = HIT ] || fail "proxy mode: X-Cache=$x, want HIT"
cmp "$tmp/first" "$tmp/body" || fail "proxy mode body differs"

stats=$(curl -fsS "$cache/_stats")
echo "$stats" | grep -q '"misses": 1' || fail "stats: want misses=1: $stats"
echo "$stats" | grep -q '"hits": 2' || fail "stats: want hits=2: $stats"
echo "smoke test passed"
