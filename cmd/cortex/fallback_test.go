package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The supervisor never parsed config before this, so it learns the address the same
// way it learns everything else: from the argv it is about to hand the child. Both
// spellings and both forms, because main.go uses flag.String and so accepts all four.
func TestConfigPathFromArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate single dash", []string{"-config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"separate double dash", []string{"--config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined single dash", []string{"-config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined double dash", []string{"--config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"among others", []string{"-local", "--config", "/etc/c.yaml", "-ca-dir", "/d"}, "/etc/c.yaml"},
		{"absent", []string{"-local"}, ""},
		{"dangling value", []string{"-local", "--config"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configPathFromArgs(tc.args); got != tc.want {
				t.Errorf("configPathFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// Every miss has to yield an address, not an error. The whole point of the fallback is
// to speak when the proxy cannot start, and "the config will not load" is one of the
// reasons it cannot start -- so that case must not be the one where we fall silent.
func TestSupervisorForwardAddr_FallsBackWhereverTheConfigCannotAnswer(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("mode: proxy-sidecar\nlistener:\n  forward_proxy_addr: 127.0.0.1:48123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("listener: [this is not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"config names an address", []string{"--config", good}, "127.0.0.1:48123"},
		{"config will not parse", []string{"--config", broken}, fallbackDefaultAddr},
		{"config is not there", []string{"--config", filepath.Join(dir, "absent.yaml")}, fallbackDefaultAddr},
		{"no config flag at all", []string{"-local"}, fallbackDefaultAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorForwardAddr(tc.args); got != tc.want {
				t.Errorf("supervisorForwardAddr(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// The exact bytes, because two different fields of this response are read by two
// different clients and a plausible-looking refactor can drop either without any other
// test noticing. Measured against real clients: Claude Code prints the body, Go's
// net/http returns the reason phrase as the error, curl discards both.
func TestFallbackRefusal_Bytes(t *testing.T) {
	got := string(fallbackRefusal)
	want := "HTTP/1.1 502 " + fallbackReason + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(fallbackBody)) + "\r\n" +
		"Connection: close\r\n" +
		"Proxy-Connection: close\r\n" +
		"\r\n" + fallbackBody
	if got != want {
		t.Errorf("refusal =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(fallbackBody, "\n") {
		t.Error("the body spans lines; a trailing newline renders as a stray '.' inside Claude Code's error framing")
	}
	if !strings.Contains(fallbackBody, "agentop service start") {
		t.Error("the body does not name the command that fixes it, which is the whole point of it")
	}
}

// A Go client reads the reason phrase and nothing else. This is the half that would be
// lost by rewriting the response as http.Error, which always writes the canonical
// status text -- a change that would still pass every other test here.
func TestFallbackServer_GoClientSeesTheReasonPhrase(t *testing.T) {
	addr := startFallback(t)

	pu, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		Timeout:   5 * time.Second,
	}
	_, err = c.Get("https://cortex.invalid/") //nolint:noctx // bounded by Timeout
	if err == nil {
		t.Fatal("the request succeeded; the fallback must refuse every connection")
	}
	if !strings.Contains(err.Error(), fallbackReason) {
		t.Errorf("go client error = %v, want it to carry the reason phrase %q", err, fallbackReason)
	}
}

// Claude Code reads the body. Asserted over a raw CONNECT because that is what an agent
// routed at an HTTPS endpoint actually sends.
func TestFallbackServer_ConnectGetsTheBody(t *testing.T) {
	addr := startFallback(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasPrefix(got, "HTTP/1.1 502 "+fallbackReason) {
		t.Errorf("status line = %q, want the 502 and the reason phrase", strings.SplitN(got, "\r\n", 2)[0])
	}
	if !strings.HasSuffix(got, fallbackBody) {
		t.Errorf("response does not end in the body; got %q", got)
	}
}

// close() has to actually free the address, because the supervisor closes the fallback
// precisely so the child can bind it. A close that leaked the port would turn this
// feature into the port conflict of #1122 on every single restart.
func TestFallbackServer_CloseFreesTheAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fallbackServer{}
	fb.openOn(ln)
	addr := ln.Addr().String()

	fb.close()

	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the address is still held after close: %v — the child could not bind it", err)
	}
	_ = again.Close()

	fb.close() // idempotent: the supervisor closes before every start, including the first
}

// startFallback opens a fallback server on a port the kernel picks and returns its
// address. openOn rather than open so there is no window between finding a free port
// and binding it.
func startFallback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fallbackServer{}
	fb.openOn(ln)
	addr := ln.Addr().String()
	t.Cleanup(fb.close)
	return addr
}
