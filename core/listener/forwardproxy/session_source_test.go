package forwardproxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The handler records on pctx how it resolved the session it hands plugins.
func TestSessionSourceThroughTheHandler(t *testing.T) {
	const claudeUA = "claude-cli/2.1.284 (external, cli)"
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	probe, client, backendURL := newAffinityProxy(t, store)

	send := func(ua, header, sid string) {
		t.Helper()
		req, _ := http.NewRequest("POST", backendURL+"/x", nil)
		req.Header.Set("User-Agent", ua)
		if header != "" {
			req.Header.Set(header, sid)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(claudeUA, "", "")                                      // pending:claude-code
	send(claudeUA, session.ClaudeCodeSessionHeader, "claude-1") // header
	send(claudeUA, "", "")                                      // affinity
	send("curl/8.7.1", "", "")                                  // active
	send("curl/8.7.1", session.SessionIDHeader, "spoofed")      // not a configured header: active

	want := []struct {
		id     string
		source pipeline.SessionSource
		header string
	}{
		{session.PendingSessionID("claude-code"), pipeline.SessionSourcePending, ""},
		{"claude-1", pipeline.SessionSourceHeader, session.ClaudeCodeSessionHeader},
		{"claude-1", pipeline.SessionSourceAffinity, ""},
		{"claude-1", pipeline.SessionSourceActive, ""},
		{"claude-1", pipeline.SessionSourceActive, ""},
	}
	if len(probe.sawSource) != len(want) {
		t.Fatalf("plugin saw %d sessions (%v), want %d", len(probe.sawSource), probe.sawID, len(want))
	}
	for i, w := range want {
		if probe.sawID[i] != w.id || probe.sawSource[i] != w.source || probe.sawHeader[i] != w.header {
			t.Errorf("request %d: session %q via %q/%q, want %q via %q/%q",
				i, probe.sawID[i], probe.sawSource[i], probe.sawHeader[i], w.id, w.source, w.header)
		}
	}
}

// A process that never sent a session header takes the session its ancestor
// named, and that is recorded as a process resolution.
func TestSessionSourceProcessChain(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := &Server{
		Sessions:         store,
		SessionIDHeaders: []string{session.ClaudeCodeSessionHeader},
		Processes:        newFakeProcs(),
	}
	agent := session.Proc{PID: 100, Start: 1}
	tool := session.Proc{PID: 200, Start: 2}

	h := http.Header{"User-Agent": {procClaudeUA}}
	h.Set(session.ClaudeCodeSessionHeader, "claude-1")
	store.Append("claude-1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if sid, how := s.resolvePluginSession(h, []session.Proc{agent}); sid != "claude-1" || how.source != pipeline.SessionSourceHeader {
		t.Fatalf("agent: %q via %q, want claude-1 via header", sid, how.source)
	}
	pctx := &pipeline.Context{}
	if sid := s.hydrateSession(pctx, http.Header{}, []session.Proc{tool, agent}); sid != "claude-1" {
		t.Fatalf("tool: session %q, want claude-1", sid)
	}
	if pctx.SessionSource != pipeline.SessionSourceProcess || pctx.SessionHeader != "" {
		t.Fatalf("tool: source %q/%q, want process", pctx.SessionSource, pctx.SessionHeader)
	}
}
