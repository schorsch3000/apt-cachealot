// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fetchText(t *testing.T, url string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAptConfUsesRequestHost(t *testing.T) {
	p, _, _ := newTestServer(t, nil)
	code, body := fetchText(t, p.URL+"/_apt.conf", map[string]string{"Host": "barbara:3142"})
	if code != 200 || !strings.Contains(body, "Acquire::http::Proxy \"http://barbara:3142\";\n") {
		t.Fatalf("status=%d body=%q", code, body)
	}
	// without an override it is whatever the client used to reach us
	_, body = fetchText(t, p.URL+"/_apt.conf", nil)
	if !strings.Contains(body, `Acquire::http::Proxy "`+p.URL+`";`) {
		t.Fatalf("body=%q", body)
	}
}

func TestPublicURLOverridesHost(t *testing.T) {
	s, err := NewServer(Options{Dir: t.TempDir(), PublicURL: "http://cache.lan:3142/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := httptest.NewServer(s)
	defer p.Close()
	_, body := fetchText(t, p.URL+"/_apt.conf", map[string]string{"Host": "something.else:1"})
	if !strings.Contains(body, `Acquire::http::Proxy "http://cache.lan:3142";`) {
		t.Fatalf("body=%q", body)
	}
}

func TestBadPublicURL(t *testing.T) {
	for _, u := range []string{"cache.lan:3142", "https://cache.lan", "http://cache.lan/x", `http://a";b`, "http://u@cache.lan"} {
		if _, err := NewServer(Options{Dir: t.TempDir(), PublicURL: u}); err == nil {
			t.Errorf("public URL %q accepted", u)
		}
	}
}

// The Host header is chosen by the client and lands in a script that is piped into sh.
func TestSetupRejectsHostsThatCouldInject(t *testing.T) {
	p, _, _ := newTestServer(t, nil)
	for _, h := range []string{`x"; touch /tmp/pwned; #`, "a b", "host$(id)", "h`id`", "a;b", "host:99999x", "-", "host:", `a'b`} {
		for _, path := range []string{"/_apt.conf", "/_setup.sh"} {
			code, body := fetchText(t, p.URL+path, map[string]string{"Host": h})
			if code == 200 {
				t.Errorf("%s with Host %q: status 200, body %q", path, h, body)
			}
		}
	}
	for _, h := range []string{"barbara", "barbara:3142", "cache.lan:3142", "10.0.0.5:3142", "[fe80::1]:3142", "[::1]"} {
		if code, _ := fetchText(t, p.URL+"/_apt.conf", map[string]string{"Host": h}); code != 200 {
			t.Errorf("Host %q refused: %d", h, code)
		}
	}
}

// Run the served script for real: it installs exactly what /_apt.conf serves, and removes it.
func TestSetupScriptInstallsAndRemoves(t *testing.T) {
	p, _, _ := newTestServer(t, nil)
	hdr := map[string]string{"Host": "barbara:3142"}
	_, conf := fetchText(t, p.URL+"/_apt.conf", hdr)
	code, script := fetchText(t, p.URL+"/_setup.sh", hdr)
	if code != 200 || !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("status=%d script=%q", code, script)
	}
	target := filepath.Join(t.TempDir(), "apt.conf.d", "01apt-cachealot")
	run := func(args ...string) string {
		cmd := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
		cmd.Stdin = strings.NewReader(script) // like `curl ... | sh`
		cmd.Env = append(os.Environ(), "APT_CACHEALOT_CONF="+target)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "http://barbara:3142") {
		t.Fatalf("output: %q", out)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != conf {
		t.Fatalf("installed file %q (err %v), want %q", got, err, conf)
	}
	run("remove")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file still there after remove: %v", err)
	}
}
