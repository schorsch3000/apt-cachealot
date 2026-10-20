# apt-cachealot

A small, fast caching proxy for apt. Built for spinning up Debian containers over and over
without downloading the same packages again.

- **Fast on a cold cache.** A miss streams from upstream to the client while it is written to disk. It is never "download, then deliver". Concurrent requests for the same file share one upstream download.
- **Fast on a warm cache.** Hits are served with `sendfile` (zero copy).
- **Stores content once.** Files are stored by sha256. The same `.deb` fetched through two mirrors takes the space of one. If a mirror's `Packages` index says a file has a hash that is already cached, the request is a hit without contacting upstream.
- **Simple cache rules.** `.deb` files are never revalidated. Index files (`Release`, `Packages`, ...) are revalidated with a conditional request after a configurable interval, and the stale copy is served if the mirror is down. When the size limit is reached, the file delivered longest ago is deleted first.

Measured on a 9.7 MB `Packages.xz` (`scripts/bench.sh`): direct 394 ms, through the cache cold 372 ms, warm 2 ms.

## Run

Docker (about 10 MB: a static binary on `distroless/static`, running as uid 65532; images for amd64 and arm64 are published to GitHub's registry). A bind-mounted cache directory must be writable by that uid:

```
docker run -d --name apt-cachealot -p 3142:3142 -v apt-cachealot:/cache \
  ghcr.io/schorsch3000/apt-cachealot -max-size 50GB
```

Tags: `latest` follows the main branch, `X.Y.Z` and `X.Y` are releases.

Docker Compose (cache in `./data`, see `docker-compose.yml`):

```
mkdir -p data
CACHE_UID=$(id -u) CACHE_GID=$(id -g) docker compose up -d
```

Create `data` yourself: if Docker creates it, it belongs to root and the container user cannot write to it.

From source (Go 1.27+):

```
go build -o apt-cachealot ./cmd/apt-cachealot && ./apt-cachealot -dir ./cache -listen :3142
```

## Use it from apt

**Quick setup** for plain `http://` sources (the Debian images use these): the cache can write the proxy config for you.

```
curl -fsS http://apt-cachealot.example.com:3142/_setup.sh | sudo sh            # install
curl -fsS http://apt-cachealot.example.com:3142/_setup.sh | sudo sh -s -- remove
curl -fsS http://apt-cachealot.example.com:3142/_apt.conf                      # just the config
```

`/_setup.sh` writes `/etc/apt/apt.conf.d/01apt-cachealot` (override with `APT_CACHEALOT_CONF`), `/_apt.conf` is its content. The address in them is the one you used to reach the cache, so use the name your clients will use, or set `-public-url` (for example behind NAT or a reverse proxy). Hosts that are not plain names, IPs or `host:port` are refused, because the value ends up in a script you pipe into `sh`.

**Prefix mode** works for http and https mirrors. The mirror host goes into the path, the cache talks to the mirror (https by default):

```
# /etc/apt/sources.list.d/debian.sources
URIs: http://apt-cachealot.example.com:3142/deb.debian.org/debian
```

**Proxy mode** works for plain `http://` sources:

```
# /etc/apt/apt.conf.d/01proxy
Acquire::http::Proxy "http://apt-cachealot.example.com:3142";
```

The cache still fetches upstream over https unless the host is listed in `-http-hosts`. Both modes share the same cache.

`https://` sources cannot be cached in proxy mode, because apt tunnels them with `CONNECT` and the cache cannot see inside. The cache answers `501` with a hint. Use prefix mode for them.

## Flags

| flag | default | meaning |
|---|---|---|
| `-listen` | `:3142` | listen address |
| `-dir` | `./cache` | cache directory |
| `-max-size` | `20GB` | cache size limit (`500M`, `20GB`, `1TB`, `0` = unlimited) |
| `-index-ttl` | `5m` | how long index files count as fresh before they are revalidated |
| `-scheme` | `https` | scheme used to reach upstream mirrors |
| `-public-url` | from the request | address clients use to reach this instance (`http://host[:port]`), shown by `/_apt.conf` and `/_setup.sh` |
| `-parent` | | base URL of another apt-cachealot to fetch from instead of the mirrors (see below) |
| `-parent-fallback` | off | with `-parent`: if the parent cannot be reached at all, fetch from the mirrors directly |
| `-http-hosts` | | comma separated hosts (`host` or `host:port`) to fetch over plain http |
| `-healthcheck` | | query a running instance and exit 0 if healthy (used by the Docker `HEALTHCHECK`) |

There is no authentication and any host can be requested through the cache, so run it only on a trusted network.

## Chaining instances

A small instance (say on a workstation, a few GB) can use a big one on the LAN (hundreds of GB) as its upstream. The big one talks to the internet.

```
# workstation: small cache, misses go to the LAN cache
apt-cachealot -max-size 5GB -parent http://cache.lan:3142

# LAN server: large cache, fetches from the mirrors
apt-cachealot -max-size 300GB
```

The workstation then asks the parent for `http://cache.lan:3142/<host>/<path>` (prefix mode). Notes:

- Mirror settings (`-scheme`, `-http-hosts`) only matter on the instance that talks to the mirrors, which is the parent. They are ignored on the child.
- Index freshness is synchronised. The parent reports how long ago it last validated an index (`Age` header) and the child counts from that moment, so a chain is never staler than the longest single TTL. The child also sends `Cache-Control: max-age=<its TTL>`, so a parent with a longer TTL revalidates instead of handing over a copy the child would discard at once. The parent only honors `max-age` from other instances (requests carrying `X-Cachealot-Via`), never from apt, which sends `max-age=0` on every update.
- If the parent is unreachable, requests fail with `502` (cached files and stale indexes are still served). With `-parent-fallback` the instance fetches from the mirrors itself instead, so a laptop away from the LAN keeps working, and tries the parent again after 30 seconds. Only "cannot connect" (refused, no route, DNS) triggers this, not a slow or failing parent, so a misbehaving parent is not silently bypassed. Mirror settings (`-scheme`, `-http-hosts`) then apply on the instance itself.
- Loops are caught: every forwarded request carries the ids of the instances it passed, and an instance that sees its own id (or a chain of 8) answers `508` instead of recursing.
- Chains can be longer than two instances.

## Statistics

Cached index files carry an `Age` header (seconds since they were last validated upstream). `GET /_stats` returns JSON: requests, hits, misses, `hit_rate`, bytes served from cache vs fetched from upstream, `byte_hit_rate`, cache size and object count, downloads in flight, free disk space, the configured parent and whether it is currently bypassed (`parent_down`, `parent_fallbacks`). A revalidation that ends in a 304 is not counted as a miss. Counters reset when the process restarts.

```
curl -s http://localhost:3142/_stats
```

## How it works

- Cache key is `host/path`. Content lives in `blobs/<sha256>`, and one small `refs/` file per key points at a blob and keeps ETag and Last-Modified.
- A miss creates one download per key. All clients, the first included, read from the growing temp file. The download finishes even if every client disconnects. It is committed only if it completed in full.
- `Packages` indexes (plain, gz, bz2, xz) are parsed in the background to learn the sha256 of each `.deb` per mirror. `by-hash` URLs carry their hash in the path.
- Last-delivery times are kept in memory and saved to `state.json` every minute and on shutdown (SIGINT/SIGTERM).

Known limits: indexes that apt updates with pdiff patches are never seen as a whole, so they are not parsed. Query strings are ignored. Download counters are not persisted.

## Development

Layout: `cmd/apt-cachealot` (flags, shutdown), `internal/proxy` (HTTP handler, storage, stats), `internal/lru` (size limit and eviction), `internal/pkgindex` (Packages parsing), `scripts/bench.sh` and `scripts/smoke.sh` (benchmark, end-to-end check), `docs/idea.md` (original design notes).

```
GOPATH=$HOME/go go test -race ./...      # GOPATH override only needed where it is set to "~/go"
./scripts/bench.sh deb.debian.org/debian/dists/trixie/main/binary-amd64/Packages.xz ./cache
```

## License

Copyright (C) 2026 Dirk Heilig. apt-cachealot is free software under the GNU General Public License, version 3 or (at your option) any later version; see [LICENSE](LICENSE). Licenses of bundled third-party code are in [THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES). The Docker image carries both files in `/licenses`.
