package main

import (
	"bufio"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/config"
)

// fallbackDefaultAddr is where the forward proxy is pinned for a local install. It is
// the answer whenever the config cannot give one, which is not a rare path: a config
// that will not load is one of the reasons the proxy cannot start, and that is exactly
// when the fallback has something to say. Kept equal to the default in
// cmd/agentop/setup_step_service.go, which picks it for the same reason.
const fallbackDefaultAddr = "127.0.0.1:47600"

// configPathFromArgs finds the -config value in the arguments the supervisor is about
// to hand the child. main.go declares it with flag.String, so all four spellings are
// accepted and all four are read here.
func configPathFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return ""
}

// supervisorForwardAddr is the address the fallback listener binds: the forward proxy's
// own, so a client routed at it reaches the fallback when no child holds it. Every miss
// falls back rather than failing, for the reason on fallbackDefaultAddr.
func supervisorForwardAddr(args []string) string {
	path := configPathFromArgs(args)
	if path == "" {
		return fallbackDefaultAddr
	}
	cfg, err := config.Load(path)
	if err != nil {
		slog.Debug("supervisor: cannot read the config for the fallback address; using the default",
			"config", path, "addr", fallbackDefaultAddr, "error", err)
		return fallbackDefaultAddr
	}
	if cfg.Listener.ForwardProxyAddr == "" {
		return fallbackDefaultAddr
	}
	return cfg.Listener.ForwardProxyAddr
}

// The refusal is fail-closed by design: traffic does not reach the internet unobserved
// just because the proxy is down. What it buys over a refused connection is a client
// that can say why, and that depends on which field the client reads -- measured, not
// assumed:
//
//	curl 8.7.1        "CONNECT tunnel failed, response 502"   neither field
//	Go net/http       the reason phrase, as the error          reason phrase
//	Claude Code       prints it inside "API Error: 502 ..."    body
//
// So both are set. This is also why the response is written as bytes rather than through
// http.Error: Go's net/http always writes the canonical status text for the code and
// offers no way to set a reason phrase, which would silently cost the Go half.
//
// The body is one line with no trailing newline. Claude Code wraps it in framing of its
// own, and a trailing newline landed a stray "." on a line by itself.
const (
	fallbackReason = "Cortex proxy is not serving"
	fallbackBody   = "Cortex is routed in this environment (HTTPS_PROXY) but is not serving — run: agentop service start"
)

// fallbackIOTimeout bounds a connection the fallback is answering. Without it a client
// that opens and never writes would hold a goroutine for as long as the proxy stays
// down, which can be the whole of a long port conflict.
const fallbackIOTimeout = 5 * time.Second

var fallbackRefusal = []byte("HTTP/1.1 502 " + fallbackReason + "\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Length: " + strconv.Itoa(len(fallbackBody)) + "\r\n" +
	"Connection: close\r\n" +
	"Proxy-Connection: close\r\n" +
	"\r\n" + fallbackBody)

// fallbackServer answers every connection with fallbackRefusal. The supervisor holds it
// open only while no child is serving, so in steady state it is closed and a healthy
// install is unchanged: no extra hop, no second process, no new port.
type fallbackServer struct {
	mu sync.Mutex
	ln net.Listener
}

// open binds addr and starts answering. A bind that fails is logged and otherwise
// ignored: restarting the child is the supervisor's actual job, and losing the
// diagnostic listener must never stop it. The usual cause is the previous child not
// having released the port yet, which the next iteration retries anyway.
func (f *fallbackServer) open(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("supervisor: could not open the fallback listener, so a client will see a refused connection rather than an explanation",
			"addr", addr, "error", err)
		return
	}
	f.openOn(ln)
}

// openOn is open's second half, split out so a test can hand in a listener on a port
// the kernel picked instead of racing to bind a port it found free.
func (f *fallbackServer) openOn(ln net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		_ = ln.Close()
		return
	}
	f.ln = ln
	slog.Debug("supervisor: fallback listener open", "addr", ln.Addr().String())
	go f.serve(ln)
}

// close frees the address. The supervisor calls this immediately before starting a
// child, which is the only reason the child can bind at all, so a close that leaked the
// port would make every restart a port conflict.
func (f *fallbackServer) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return
	}
	_ = f.ln.Close()
	f.ln = nil
}

func (f *fallbackServer) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed, which is the normal way this ends
		}
		go refuse(c)
	}
}

// refuse answers one connection and closes it. The request is read as far as its first
// line and then ignored -- the answer does not depend on it -- but it is read rather
// than answered blind so the client has finished writing before the close, which keeps
// the peer from seeing a reset in place of the response it was about to read.
//
// Logging is DEBUG per connection on purpose: clients retry, Claude Code three times
// per request, so anything louder would fill the log while the proxy is down.
func refuse(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(fallbackIOTimeout))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	slog.Debug("supervisor: refused a request while the proxy is not serving",
		"request", strings.TrimSpace(line), "remote", c.RemoteAddr().String())
	_, _ = c.Write(fallbackRefusal)
}
