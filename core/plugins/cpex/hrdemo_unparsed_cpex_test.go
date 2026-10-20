// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/mcpparser"
)

// rawCall runs body through the real mcp-parser and then the plugin, as
// the sidecar's outbound pipeline does.
func rawCall(t *testing.T, p *CPEX, idp *hrIdP, user, body string) pipeline.Action {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "hr-mcp.cpex-demo:9100",
		Path:      "/mcp",
		Method:    http.MethodPost,
		Headers: http.Header{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + idp.clientToken(t)},
			"X-User-Token":  {idp.userToken(t, user)},
		},
		Body: []byte(body),
	}
	parser := mcpparser.NewMCPParser()
	if err := parser.Configure(json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	parser.OnRequest(context.Background(), pctx)
	return p.OnRequest(context.Background(), pctx)
}

// A tool call the mcp-parser cannot decode would otherwise reach CPEX as
// global traffic, which no tool route judges, while hr-mcp's Python json
// reads NaN, Infinity, 1e400 and a byte order mark and runs the tool.
// Each is refused before CPEX sees it.
func TestHRDemoUnparseableToolCallDenied(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	const adjust = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":%s}}}`
	for label, body := range map[string]string{
		"NaN beside the arguments": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"EMP-001234","include_ssn":true,"x":NaN}}}`,
		"NaN amount":               fmt.Sprintf(adjust, "NaN"),
		"Infinity amount":          fmt.Sprintf(adjust, "Infinity"),
		"-Infinity amount":         fmt.Sprintf(adjust, "-Infinity"),
		"1e400 amount":             fmt.Sprintf(adjust, "1e400"),
		"byte order mark":          "\xef\xbb\xbf" + fmt.Sprintf(adjust, "50000"),
		"Method of another type":   `{"jsonrpc":"2.0","id":1,"Method":5,"method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"EMP-001234"}}}`,
	} {
		wantReject(t, label, rawCall(t, p, idp, "alice", body), "cpex.unparseable_request")
	}
	if n := idp.exchangeCount(); n != 0 {
		t.Fatalf("token exchanges = %d, want 0", n)
	}
	if n := idp.cibaCount(); n != 0 {
		t.Fatalf("CIBA requests = %d, want 0", n)
	}
}
