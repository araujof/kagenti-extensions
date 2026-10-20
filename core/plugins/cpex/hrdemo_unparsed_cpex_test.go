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
// global traffic, which no tool route judges, while Python's json reads
// NaN, Infinity, 1e400, a byte order mark and UTF-16 and runs the tool.
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
		"UTF-16BE body":            string(utf16Body(true, fmt.Sprintf(adjust, "50000"))),
		"UTF-16LE body with a BOM": "\xff\xfe" + string(utf16Body(false, fmt.Sprintf(adjust, "50000"))),
		"trailing data":            fmt.Sprintf(adjust, "5000") + `{"x":1}`,
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

// Every JSON number form reaches the decision the tool would act on.
// The policy reads the amount as a float64 and hr-mcp as the integer it
// applies, so the two sides differ only past 2^53, where both are far
// over the threshold: every such amount needs an approval whose scope
// (|amount| <= 25000) then refuses it (TestHRDemoAdjustNeedsApproval).
func TestHRDemoAdjustAmountForms(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	const adjust = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":%s}}}`
	for _, amount := range []string{"5000", "5000.0", "5e3", "-5000", "-0", "0", "10000", "-10000", "10000.0", "1.0e4", "-1e4"} {
		wantContinue(t, "amount "+amount, rawCall(t, p, idp, "bob", fmt.Sprintf(adjust, amount)))
	}
	for _, amount := range []string{"10001", "-10001", "1.5e4", "15000.0", "25000", "-25000", "25001", "-25001",
		"9007199254740993", "-9007199254740993", "1e300"} {
		wantPending(t, "amount "+amount, rawCall(t, p, idp, "bob", fmt.Sprintf(adjust, amount)))
	}
}
