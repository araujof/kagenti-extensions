package cpex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// --- inferenceToCMFParts (request) ---

func TestInferenceToCMFParts_RequestFlattensMessages(t *testing.T) {
	inf := &pipeline.InferenceExtension{
		Model: "gpt-4o",
		Messages: []pipeline.InferenceMessage{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "my ssn is 123-45-6789"},
			{Role: "assistant", Content: ""}, // tool-call turn, no content — dropped
		},
	}
	parts := inferenceToCMFParts(inf, false, nil)
	if len(parts) != 2 {
		t.Fatalf("want 2 text parts (empty-content message dropped), got %d", len(parts))
	}
	for i, want := range []string{"be terse", "my ssn is 123-45-6789"} {
		if parts[i].Kind != cmfPartText || parts[i].Text != want {
			t.Fatalf("part %d = %+v, want text %q", i, parts[i], want)
		}
	}
}

// --- inferenceResponseParts (response phase parses body) ---

func TestInferenceResponseParts_CompletionAndToolCalls(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "here is the answer",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"id\":\"E1\"}"}}
				]
			},
			"finish_reason": "stop"
		}]
	}`)
	parts := inferenceResponseParts(body)
	if len(parts) != 2 {
		t.Fatalf("want completion + tool_call = 2 parts, got %d: %+v", len(parts), parts)
	}
	if parts[0].Kind != cmfPartText || parts[0].Text != "here is the answer" {
		t.Fatalf("part0 = %+v, want completion text", parts[0])
	}
	if parts[1].Kind != cmfPartToolCall || parts[1].Name != "lookup" {
		t.Fatalf("part1 = %+v, want tool_call lookup", parts[1])
	}
	if got := parts[1].Arguments["id"]; got != "E1" {
		t.Fatalf("tool_call args id = %v, want E1", got)
	}
	if parts[1].CorrelationID != "call_1" {
		t.Fatalf("tool_call id = %q, want call_1", parts[1].CorrelationID)
	}
}

func TestInferenceResponseParts_StreamingBodyYieldsNil(t *testing.T) {
	// SSE frames don't parse as one JSON object.
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	if parts := inferenceResponseParts(sse); parts != nil {
		t.Fatalf("streaming body should yield nil parts, got %+v", parts)
	}
}

// --- applyInferenceRequestBodyMod ---

func TestInferenceRequestBodyMod_RedactsPositionally(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"sys"},{"role":"user","content":"ssn 123-45-6789"}],"temperature":0.2}`),
	}
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"sys", "ssn [REDACTED]"})
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	msgs := decoded["messages"].([]any)
	if got := msgs[1].(map[string]any)["content"]; got != "ssn [REDACTED]" {
		t.Fatalf("user content not redacted: %v", got)
	}
	// Untouched fields survive.
	if decoded["temperature"].(float64) != 0.2 {
		t.Fatalf("temperature lost: %v", decoded["temperature"])
	}
}

func TestInferenceRequestBodyMod_CountDriftFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`),
	}
	// Only one redacted text for two string-content messages → drift.
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"a"})
	if err == nil {
		t.Fatal("expected count-drift error (fail closed), got nil")
	}
	if mutated {
		t.Fatal("must not mutate on drift")
	}
}

func TestInferenceRequestBodyMod_NoChangeReportsNoOp(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"messages":[{"role":"user","content":"unchanged"}]}`),
	}
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"unchanged"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mutated {
		t.Fatal("identical rewrite should report mutated=false")
	}
}

// --- applyInferenceResponseBodyMod ---

func TestInferenceResponseBodyMod_RewritesChoiceContent(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"choices":[{"message":{"role":"assistant","content":"leaked 123-45-6789"},"finish_reason":"stop"}],"usage":{"total_tokens":9}}`),
	}
	mutated, err := applyInferenceResponseBodyMod(pctx, "leaked [REDACTED]")
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &decoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	msg := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "leaked [REDACTED]" {
		t.Fatalf("completion not redacted: %v", msg["content"])
	}
}

func TestInferenceResponseBodyMod_MultiChoiceFailsClosed(t *testing.T) {
	// n>1: two choices carry string content, but only one redacted
	// completion is available. Stamping it over both would collapse
	// distinct completions, so the rewrite must fail closed.
	pctx := &pipeline.Context{
		ResponseBody: []byte(`{"choices":[{"message":{"role":"assistant","content":"first 123-45-6789"}},{"message":{"role":"assistant","content":"second answer"}}]}`),
	}
	original := append([]byte(nil), pctx.ResponseBody...)
	mutated, err := applyInferenceResponseBodyMod(pctx, "first [REDACTED]")
	if err == nil {
		t.Fatal("multi-choice response rewrite should fail closed with an error")
	}
	if mutated {
		t.Fatal("must not mutate a multi-choice response")
	}
	if string(pctx.ResponseBody) != string(original) {
		t.Fatalf("response body must be left untouched on fail-closed, got %s", pctx.ResponseBody)
	}
}

func TestInferenceResponseBodyMod_StreamingFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		ResponseBody: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"),
	}
	mutated, err := applyInferenceResponseBodyMod(pctx, "redacted")
	if err == nil {
		t.Fatal("streaming response rewrite should fail closed with an error")
	}
	if mutated {
		t.Fatal("must not mutate a streaming response")
	}
}

// --- parseToolArgs / inferProvider ---

func TestParseToolArgs(t *testing.T) {
	if got := parseToolArgs(`{"a":1}`); !reflect.DeepEqual(got, map[string]any{"a": float64(1)}) {
		t.Fatalf("valid json: %v", got)
	}
	if got := parseToolArgs("not json"); got["_raw"] != "not json" {
		t.Fatalf("malformed should wrap as _raw: %v", got)
	}
	if got := parseToolArgs(""); got != nil {
		t.Fatalf("empty should be nil: %v", got)
	}
}

func TestInferProvider(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":            "openai",
		"o1-preview":        "openai",
		"claude-3-5-sonnet": "anthropic",
		"gemini-1.5-pro":    "google",
		"llama3":            "",
		"granite-3":         "",
	}
	for model, want := range cases {
		if got := inferProvider(model); got != want {
			t.Errorf("inferProvider(%q) = %q, want %q", model, got, want)
		}
	}
}

// --- tool results on the request ---

func TestInferenceToCMFParts_AnthropicToolResults(t *testing.T) {
	inf := &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "read .env"}},
		ToolResults: []pipeline.InferenceToolResult{
			{ToolUseID: "toolu_1", Content: "AWS_SECRET_ACCESS_KEY=abc", IsError: true},
			{ToolUseID: "toolu_2"}, // image block only
		},
	}
	parts := inferenceToCMFParts(inf, false, nil)
	want := []cmfPart{
		{Kind: cmfPartText, Text: "read .env"},
		{Kind: cmfPartToolResult, CorrelationID: "toolu_1", Content: "AWS_SECRET_ACCESS_KEY=abc", IsError: true},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
}

func TestInferenceToCMFParts_OpenAIToolRole(t *testing.T) {
	inf := &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
		{Role: "user", Content: "read .env"},
		{Role: "tool", Content: "TOKEN=xyz"},
	}}
	parts := inferenceToCMFParts(inf, false, nil)
	want := []cmfPart{
		{Kind: cmfPartText, Text: "read .env"},
		{Kind: cmfPartToolResult, Content: "TOKEN=xyz"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
}

func TestInferenceRequestBodyMod_SkipsToolMessages(t *testing.T) {
	pctx := &pipeline.Context{
		Body: []byte(`{"messages":[{"role":"user","content":"a"},{"role":"tool","content":"t"},{"role":"user","content":"b"}]}`),
	}
	mutated, err := applyInferenceRequestBodyMod(pctx, []string{"a", "[REDACTED]"})
	if err != nil || !mutated {
		t.Fatalf("mutated=%v err=%v", mutated, err)
	}
	var decoded struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	if err := json.Unmarshal(pctx.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Messages[1].Content != "t" || decoded.Messages[2].Content != "[REDACTED]" {
		t.Fatalf("messages = %+v", decoded.Messages)
	}
}

func inferencePctx(path string, body string, inf *pipeline.InferenceExtension) *pipeline.Context {
	return &pipeline.Context{Path: path, Body: []byte(body), Extensions: pipeline.Extensions{Inference: inf}}
}

func TestApplyInferenceRequestMod(t *testing.T) {
	anthropic := func() *pipeline.Context {
		return inferencePctx("/v1/messages",
			`{"messages":[{"role":"user","content":"hi"}]}`,
			&pipeline.InferenceExtension{
				Messages:    []pipeline.InferenceMessage{{Role: "user", Content: "hi"}},
				ToolResults: []pipeline.InferenceToolResult{{ToolUseID: "t1", Content: "secret"}},
			})
	}
	openai := func() *pipeline.Context {
		return inferencePctx("/v1/chat/completions",
			`{"messages":[{"role":"user","content":"hi"},{"role":"tool","content":"secret"}]}`,
			&pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
				{Role: "user", Content: "hi"}, {Role: "tool", Content: "secret"},
			}})
	}

	t.Run("unchanged anthropic payload is a no-op", func(t *testing.T) {
		pctx := anthropic()
		mutated, err := applyInferenceRequestMod(pctx, []string{"hi"}, []any{"secret"})
		if err != nil || mutated || pctx.BodyMutated() {
			t.Fatalf("mutated=%v err=%v", mutated, err)
		}
	})
	t.Run("text change on anthropic is an error", func(t *testing.T) {
		pctx := anthropic()
		if _, err := applyInferenceRequestMod(pctx, []string{"[REDACTED]"}, []any{"secret"}); err == nil {
			t.Fatal("want error")
		}
		if pctx.BodyMutated() {
			t.Fatal("body rewritten")
		}
	})
	t.Run("tool result change is an error", func(t *testing.T) {
		if _, err := applyInferenceRequestMod(openai(), []string{"hi"}, []any{"[REDACTED]"}); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("text change on openai chat is written back", func(t *testing.T) {
		pctx := openai()
		mutated, err := applyInferenceRequestMod(pctx, []string{"[REDACTED]"}, []any{"secret"})
		if err != nil || !mutated {
			t.Fatalf("mutated=%v err=%v", mutated, err)
		}
		if string(pctx.Body) != `{"messages":[{"content":"[REDACTED]","role":"user"},{"content":"secret","role":"tool"}]}` {
			t.Fatalf("body = %s", pctx.Body)
		}
	})
}

func TestInferenceToolResultsCustom(t *testing.T) {
	long := strings.Repeat("x", egressExcerptBytes+10)
	inf := &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "hi"},
			{Role: roleTool, Content: "openai result"},
		},
		ToolResults: []pipeline.InferenceToolResult{
			{ToolUseID: "toolu_1", Content: "AKIAABCDEFGHIJKLMNOP"},
			{ToolUseID: "toolu_2", Content: long, IsError: true},
			{ToolUseID: "toolu_3"},
		},
	}
	count, results := inferenceToolResultsCustom(inf)
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	want := map[string]any{
		"0": map[string]any{"tool_use_id": "", "content": "openai result", "is_error": false, "truncated": false},
		"1": map[string]any{"tool_use_id": "toolu_1", "content": "AKIAABCDEFGHIJKLMNOP", "is_error": false, "truncated": false},
	}
	for k, v := range want {
		if !jsonEqual(results[k], v) {
			t.Errorf("results[%s] = %v, want %v", k, results[k], v)
		}
	}
	r2, _ := results["2"].(map[string]any)
	if c, _ := r2["content"].(string); len(c) != egressExcerptBytes || r2["truncated"] != true || r2["is_error"] != true {
		t.Fatalf("results[2] = %d bytes, %v", len(c), r2)
	}
	if count, results := inferenceToolResultsCustom(&pipeline.InferenceExtension{}); count != 0 || results != nil {
		t.Fatalf("no tool results: %d %v", count, results)
	}
}

// A response modification is applied or refused, never dropped: a changed
// tool call has no re-serializer, and a completion redacted to nothing, or
// with its text part removed, is still a change.
func TestApplyInferenceResponseMod(t *testing.T) {
	const body = `{"choices":[{"message":{"role":"assistant","content":"key AKIAABCDEFGHIJKLMNOP",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"send","arguments":"{\"to\":\"evil\"}"}}]}}]}`
	resp := func() *pipeline.Context { return &pipeline.Context{ResponseBody: []byte(body)} }
	sent := inferenceResponseParts([]byte(body))

	if mutated, err := applyInferenceResponseMod(resp(), sent); err != nil || mutated {
		t.Fatalf("unchanged: mutated=%v err=%v", mutated, err)
	}

	redactedCall := append([]cmfPart(nil), sent...)
	redactedCall[1].Arguments = map[string]any{"to": "[REDACTED]"}
	if _, err := applyInferenceResponseMod(resp(), redactedCall); err == nil {
		t.Fatal("changed tool call dropped")
	}
	if _, err := applyInferenceResponseMod(resp(), sent[:1]); err == nil {
		t.Fatal("removed tool call dropped")
	}

	pctx := resp()
	blank := append([]cmfPart(nil), sent...)
	blank[0].Text = ""
	if mutated, err := applyInferenceResponseMod(pctx, blank); err != nil || !mutated || strings.Contains(string(pctx.ResponseBody), "AKIA") {
		t.Fatalf("blanked completion: mutated=%v err=%v body=%s", mutated, err, pctx.ResponseBody)
	}
	if _, err := applyInferenceResponseMod(resp(), sent[1:]); err == nil {
		t.Fatal("removed completion text dropped")
	}

	pctx = resp()
	redacted := append([]cmfPart(nil), sent...)
	redacted[0].Text = "key [REDACTED]"
	if mutated, err := applyInferenceResponseMod(pctx, redacted); err != nil || !mutated || strings.Contains(string(pctx.ResponseBody), "AKIA") {
		t.Fatalf("redacted completion: mutated=%v err=%v body=%s", mutated, err, pctx.ResponseBody)
	}
}

// A redaction requested on a request with no messages array (a legacy
// /v1/completions prompt) cannot be applied, so it is an error.
func TestApplyInferenceRequestMod_NoMessagesFailsClosed(t *testing.T) {
	pctx := &pipeline.Context{
		Path: "/v1/completions",
		Body: []byte(`{"model":"m","prompt":"key AKIAABCDEFGHIJKLMNOP"}`),
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:    "m",
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "key AKIAABCDEFGHIJKLMNOP"}},
		}},
	}
	if mutated, err := applyInferenceRequestMod(pctx, []string{"key [REDACTED]"}, nil); err == nil {
		t.Fatalf("redaction dropped: mutated=%v body=%s", mutated, pctx.Body)
	}
	if mutated, err := applyInferenceRequestMod(pctx, []string{"key AKIAABCDEFGHIJKLMNOP"}, nil); err != nil || mutated {
		t.Fatalf("unchanged: mutated=%v err=%v", mutated, err)
	}
}
