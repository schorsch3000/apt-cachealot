#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Compare direct vs proxy cold vs proxy warm for one file.
# usage: ./bench.sh <host/path> [cache-dir] [proxy-base] [runs]
# cache-dir is wiped before each cold run (server looks up files on disk, no restart needed).
# e.g.   ./bench.sh deb.debian.org/debian/dists/trixie/main/binary-amd64/Packages.xz ./cache
set -euo pipefail
target=${1:?usage: bench.sh host/path [cache-dir] [proxy] [runs]}
cache=${2:-./cache}
proxy=${3:-http://localhost:3142}
runs=${4:-5}
fmt='%{time_starttransfer} %{time_total}\n'
t() { curl -sS -o /dev/null -w "$fmt" "$1"; }

t "https://$target" >/dev/null   # warm up DNS/TLS/CDN so order does not bias results
declare -a d_tt d_to c_tt c_to w_tt w_to
for i in $(seq "$runs"); do
  read -r a b < <(t "https://$target");  d_tt+=("$a"); d_to+=("$b")
  rm -rf "$cache/blobs" "$cache/refs"
  read -r a b < <(t "$proxy/$target");   c_tt+=("$a"); c_to+=("$b")
  read -r a b < <(t "$proxy/$target");   w_tt+=("$a"); w_to+=("$b")
done
med() { printf '%s\n' "$@" | sort -g | awk '{v[NR]=$1} END{printf "%.4f", v[int((NR+1)/2)]}'; }
printf '%-7s %10s %10s   (median of %s runs, seconds)\n' case ttfb total "$runs"
printf '%-7s %10s %10s\n' direct "$(med "${d_tt[@]}")" "$(med "${d_to[@]}")"
printf '%-7s %10s %10s\n' cold   "$(med "${c_tt[@]}")" "$(med "${c_to[@]}")"
printf '%-7s %10s %10s\n' warm   "$(med "${w_tt[@]}")" "$(med "${w_to[@]}")"
