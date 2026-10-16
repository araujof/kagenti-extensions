package inferencerouter

import (
	"encoding/json"
	"testing"
)

func stripBody(t *testing.T, body string) (string, int) {
	t.Helper()
	out, n := stripReasoning([]byte(body))
	return string(out), n
}

func TestStrip_AnthropicThinkingBlocks(t *testing.T) {
	out, n := stripBody(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"sig"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"t1","name":"lookup","input":{}}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(v.Messages))
	}
	if want := `[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]`; string(v.Messages[1].Content) != want {
		t.Errorf("assistant content = %s, want %s", v.Messages[1].Content, want)
	}
}

// A thinking-only assistant turn is dropped whole: an empty content array is a
// 400 at Anthropic, and a thinking-only turn has no tool_use for a tool_result
// to reference.
func TestStrip_AThinkingOnlyMessageIsDroppedWhole(t *testing.T) {
	out, n := stripBody(t, `{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"sig"}]},
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]}
	]}`)
	if n != 2 {
		t.Fatalf("dropped = %d, want 2", n)
	}
	var v struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(v.Messages))
	}
}

// The user side is never touched: a user's own content, tool results included,
// passes byte-identical.
func TestStrip_LeavesUserMessagesAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"and this"}]}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d, out = %s; want 0 and unchanged", n, out)
	}
}

func TestStrip_ResponsesReasoningItems(t *testing.T) {
	out, n := stripBody(t, `{"input":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"12121"}]},
		{"role":"user","content":[{"type":"input_text","text":"add 1"}]}
	]}`)
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	var v struct {
		Input []struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(v.Input) != 2 || v.Input[0].Type != "message" {
		t.Errorf("input = %+v", v.Input)
	}
}

// Chat completions carries reasoning as plain-text fields no measured server
// rejects; the strip must not touch this format at all.
func TestStrip_LeavesChatCompletionsAlone(t *testing.T) {
	body := `{"messages":[
		{"role":"assistant","content":"12121","reasoning_content":"17*23=391","reasoning":"...","thinking_blocks":[{"signature":"sig"}]},
		{"role":"user","content":"add 1"}
	]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and byte-identical body", n)
	}
}

// A body with nothing strippable is returned unchanged, so the wiring in Task 4
// can skip SetBody entirely.
func TestStrip_NothingToDropReturnsTheBody(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"other":[1,2]}`
	out, n := stripBody(t, body)
	if n != 0 || string(out) != body {
		t.Errorf("dropped = %d; want 0 and unchanged", n)
	}
}
