package cpex

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

func egressRequest(method, host, path, body string, h http.Header) *pipeline.Context {
	if h == nil {
		h = http.Header{}
	}
	return &pipeline.Context{
		Direction: pipeline.Outbound, Method: method, Host: host, Path: path,
		Headers: h, Body: []byte(body),
	}
}

func TestEgressPart(t *testing.T) {
	cases := []struct {
		name string
		pctx *pipeline.Context
		want map[string]any
	}{
		{
			name: "plaintext post",
			pctx: egressRequest("POST", "support.local:8080", "/projects/A/reports", `{"r":"ok"}`, nil),
			want: map[string]any{
				"method": "POST", "host": "support.local", "path": "/projects/A/reports",
				"body": `{"r":"ok"}`, "truncated": false, "content_encoding": "",
			},
		},
		{
			name: "bridged inner request",
			pctx: egressRequest("put", "API.GitHub.com:443", "/repos/x", "", http.Header{"Content-Encoding": {"GZIP"}}),
			want: map[string]any{
				"method": "PUT", "host": "api.github.com", "path": "/repos/x",
				"body": "", "truncated": false, "content_encoding": "gzip",
			},
		},
		{
			name: "connect is host only",
			pctx: egressRequest("CONNECT", "github.com:443", "/ignored", "bytes", http.Header{"Content-Encoding": {"gzip"}}),
			want: map[string]any{
				"method": "CONNECT", "host": "github.com", "path": "",
				"body": "", "truncated": false, "content_encoding": "",
			},
		},
		{
			name: "ipv6 literal",
			pctx: egressRequest("GET", "[::1]:9000", "/", "", nil),
			want: map[string]any{
				"method": "GET", "host": "::1", "path": "/",
				"body": "", "truncated": false, "content_encoding": "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts, et, en := pctxToCMFParts(tc.pctx, false, true)
			if et != "tool" || en != egressToolName || len(parts) != 1 {
				t.Fatalf("entity = %s/%s, parts = %+v", et, en, parts)
			}
			if parts[0].Kind != cmfPartToolCall || !reflect.DeepEqual(parts[0].Arguments, tc.want) {
				t.Fatalf("args = %#v, want %#v", parts[0].Arguments, tc.want)
			}
		})
	}
}

func TestEgressPart_TruncatesOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("a", egressExcerptBytes-1) + "é tail"
	args := egressPart(egressRequest("POST", "h", "/", body, nil)).Arguments
	got := args["body"].(string)
	if args["truncated"] != true || !utf8.ValidString(got) || len(got) != egressExcerptBytes-1 {
		t.Fatalf("truncated = %v, len = %d, valid = %v", args["truncated"], len(got), utf8.ValidString(got))
	}
	if d := egressDetails(egressToolName, args); d["egress_excerpt_bytes"] != "8191" || d["egress_truncated"] != "true" {
		t.Fatalf("details = %v", d)
	}
}

func TestEgressPart_DisabledOrResponse(t *testing.T) {
	pctx := egressRequest("POST", "support.local", "/x", "body", nil)
	for _, tc := range []struct {
		response, egress bool
	}{{false, false}, {true, true}} {
		parts, et, en := pctxToCMFParts(pctx, tc.response, tc.egress)
		if et != "" || en != "" || len(parts) != 1 || parts[0].Kind != cmfPartText {
			t.Fatalf("response=%v egress=%v: entity = %s/%s parts = %+v", tc.response, tc.egress, et, en, parts)
		}
	}
	// A parsed request is never presented as egress.
	pctx.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/list"}
	if _, et, _ := pctxToCMFParts(pctx, false, true); et != "" {
		t.Fatalf("mcp request presented as %s", et)
	}
}

func TestEgressDetailsOtherTools(t *testing.T) {
	if d := egressDetails("get_weather", map[string]any{"body": "x"}); d != nil {
		t.Fatalf("details = %v", d)
	}
}

func writeToken(t *testing.T, tok string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIdentityHeaders(t *testing.T) {
	caller := http.Header{"Authorization": {"Bearer c"}, "X-User-Token": {"spoofed"}}

	got, err := identityHeaders(caller, true, nil)
	if err != nil || got["x-user-token"] != "spoofed" || got["authorization"] != "Bearer c" {
		t.Fatalf("no principal: %v %v", got, err)
	}

	p, err := newPrincipalSource(principalConfig{TokenFile: writeToken(t, "jwt"), Header: "X-User-Token"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = identityHeaders(caller, true, p)
	if err != nil || got["x-user-token"] != "jwt" || got["authorization"] != "Bearer c" {
		t.Fatalf("tool call: %v %v", got, err)
	}
	got, err = identityHeaders(caller, false, p)
	if _, has := got["x-user-token"]; err != nil || has {
		t.Fatalf("inference: %v %v, want no principal header", got, err)
	}
	if caller.Get("X-User-Token") != "spoofed" {
		t.Fatal("caller headers mutated")
	}
	if got, err := identityHeaders(nil, true, p); err != nil || got["x-user-token"] != "jwt" {
		t.Fatalf("no caller headers: %v %v", got, err)
	}
}

func TestPrincipalSourceMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	p, err := newPrincipalSource(principalConfig{TokenFile: path, Header: "x-user-token"})
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
	if _, err := identityHeaders(nil, true, p); err != errPrincipalUnavailable {
		t.Fatalf("tool call err = %v, want errPrincipalUnavailable", err)
	}
	if _, err := identityHeaders(nil, false, p); err != nil {
		t.Fatalf("inference err = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := identityHeaders(nil, true, p); err != nil || got["x-user-token"] != "late" {
		t.Fatalf("after the file appears: %v %v", got, err)
	}
}

func TestConfigure_EgressAndPrincipal(t *testing.T) {
	var seen ManagerOptions
	p := NewCPEX()
	p.newManager = func(opts ManagerOptions) (Manager, error) {
		seen = opts
		return &FakeManager{}, nil
	}
	missing := filepath.Join(t.TempDir(), "absent")
	raw := `{"synthetic_egress":true,"principal":{"token_file":"` + missing + `"}}`
	if err := p.Configure([]byte(raw)); err != nil {
		t.Fatalf("Configure with a missing token file: %v", err)
	}
	if !seen.SyntheticEgress || seen.Principal == nil || seen.Principal.header != defaultPrincipalHeader {
		t.Fatalf("options = %+v", seen)
	}

	if err := p.Configure([]byte(`{}`)); err != nil || seen.SyntheticEgress || seen.Principal != nil {
		t.Fatalf("defaults: err = %v, options = %+v", err, seen)
	}
	for _, h := range []string{"Authorization", "host", "x-api-key", "bad header"} {
		raw := `{"principal":{"token_file":"/x","header":"` + h + `"}}`
		if err := setupFake(&FakeManager{}).Configure([]byte(raw)); err == nil {
			t.Errorf("principal.header %q accepted", h)
		}
	}
}
