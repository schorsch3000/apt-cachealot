// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/schorsch3000/apt-cachealot/internal/proxy"
)

func main() {
	listen := flag.String("listen", ":3142", "listen address")
	dir := flag.String("dir", "./cache", "cache directory")
	scheme := flag.String("scheme", "https", "default upstream scheme")
	httpHosts := flag.String("http-hosts", "", "comma separated upstream hosts to fetch via plain http")
	indexTTL := flag.Duration("index-ttl", 5*time.Minute, "max age of cached index files before refetch")
	maxSize := flag.String("max-size", "20GB", "max cache size, e.g. 500M, 20GB, 1TB; 0 = unlimited")
	parent := flag.String("parent", "", "base URL of another apt-cachealot to fetch from instead of the mirrors, e.g. http://cache.lan:3142")
	parentFallback := flag.Bool("parent-fallback", false, "if the parent cannot be reached at all, fetch from the mirrors directly (for 30s at a time)")
	healthcheck := flag.Bool("healthcheck", false, "query a running instance on -listen and exit 0 if healthy (for container health checks)")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*listen))
	}

	max, err := parseSize(*maxSize)
	if err != nil {
		slog.Error("bad -max-size", "err", err)
		os.Exit(2)
	}

	s, err := proxy.NewServer(proxy.Options{
		Dir: *dir, Scheme: *scheme, HTTPHosts: strings.Split(*httpHosts, ","),
		IndexTTL: *indexTTL, MaxSize: max, Parent: *parent, ParentFallback: *parentFallback,
	})
	if err != nil {
		slog.Error("init", "err", err)
		os.Exit(1)
	}
	slog.Info("listening", "addr", *listen, "dir", *dir, "parent", *parent)
	srv := &http.Server{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		slog.Error("serve", "err", err)
		s.Close()
		os.Exit(1)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown) // let running downloads to clients finish, briefly
	if err := s.Close(); err != nil {
		slog.Warn("save state", "err", err)
	}
}

func runHealthcheck(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 2
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/_stats")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
