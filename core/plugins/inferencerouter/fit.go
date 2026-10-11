package inferencerouter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// trimAbove and trimTo are the share of a window a request may fill before its
	// oldest turns are dropped, and the share it is brought back under. The gap keeps
	// one cut for many turns, so the start of the request, and the server's prompt
	// cache for it, stay the same from turn to turn.
	trimAbove = 0.85
	trimTo    = 0.60

	// bytesPerToken estimates tokens from a body's length. Text runs nearer 4 bytes a
	// token, so the estimate is high, and a request is trimmed a little early rather
	// than refused.
	bytesPerToken = 3

	trimNote = "[Cortex: %d earlier messages were removed so the conversation fits this model's context window.]"
)

// outputFields are where the request formats put the output limit: Anthropic messages
// and chat completions, chat completions' newer name, and responses.
var outputFields = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// fitResult is a body fitted to a model's limits, and what fitting changed.
type fitResult struct {
	body       []byte
	field      string // the output field lowered, "" for none
	from, to   int
	dropped    int // messages removed
	cut        int // index into the client's messages where the kept tail starts
	estimate   int // tokens, after fitting
	unfittable bool
}

func estimate(body []byte) int { return len(body) / bytesPerToken }

// fitBody fits body to l: its oldest turns dropped when it nears the window, then its
// output field lowered to the cap and, for a window that counts both, to what the
// prompt leaves. cut is the session's last cut, 0 for none.
func fitBody(body []byte, l limits, cut int) fitResult {
	r := fitResult{body: body}
	if l.Window > 0 {
		r = trim(r, l.Window, cut)
	}
	r.estimate = estimate(r.body)
	return capOutput(r, l)
}

// trim drops whole turns after the first user message, oldest first, by the package
// rules: kept while the last cut still fits under trimAbove, else a new cut that
// brings the body under trimTo, made only where a user turn without a tool result
// starts. A body it cannot bring within the window is left as it was.
func trim(r fitResult, window, cut int) fitResult {
	msgs := gjson.GetBytes(r.body, "messages")
	if !msgs.IsArray() {
		return r
	}
	items := msgs.Array()
	head := -1
	for i, m := range items {
		if m.Get("role").String() == "user" {
			head = i + 1
			break
		}
	}
	if head < 0 {
		return r
	}
	var starts []int
	for i := head + 1; i < len(items); i++ {
		if turnStart(items[i]) {
			starts = append(starts, i)
		}
	}
	size := func(k int) int { // the estimate with items[head:k] dropped and the note added
		dropped := 0
		for _, m := range items[head:k] {
			dropped += len(m.Raw) + 1
		}
		return (len(r.body) - dropped + len(trimNote)) / bytesPerToken
	}
	high, low := int(float64(window)*trimAbove), int(float64(window)*trimTo)
	total := estimate(r.body)
	k := 0
	switch {
	case contains(starts, cut) && size(cut) <= high:
		k = cut
	case total <= high:
		return r
	default:
		for _, s := range starts {
			if size(s) <= low {
				k = s
				break
			}
		}
		if k == 0 && len(starts) > 0 {
			k = starts[len(starts)-1]
		}
	}
	if k == 0 || size(k) > window {
		r.unfittable = total > window
		return r
	}
	raws := make([]string, 0, head+len(items)-k)
	for _, m := range items[:head] {
		raws = append(raws, m.Raw)
	}
	raws = append(raws, noted(items[k], fmt.Sprintf(trimNote, k-head)))
	for _, m := range items[k+1:] {
		raws = append(raws, m.Raw)
	}
	body, err := sjson.SetRawBytes(r.body, "messages", []byte("["+strings.Join(raws, ",")+"]"))
	if err != nil {
		return r
	}
	r.body, r.dropped, r.cut = body, k-head, k
	return r
}

// turnStart reports whether m opens a turn a kept tail may start at: a user message
// carrying no tool result, so no tool result is kept without the call it answers.
// Chat completions puts tool results in role "tool", which is not "user".
func turnStart(m gjson.Result) bool {
	if m.Get("role").String() != "user" {
		return false
	}
	content := m.Get("content")
	if !content.IsArray() {
		return true
	}
	for _, block := range content.Array() {
		if block.Get("type").String() == "tool_result" {
			return false
		}
	}
	return true
}

// noted is message m with note at the start of its content.
func noted(m gjson.Result, note string) string {
	content := m.Get("content")
	switch {
	case content.Type == gjson.String:
		out, err := sjson.Set(m.Raw, "content", note+"\n\n"+content.String())
		if err == nil {
			return out
		}
	case content.IsArray():
		block, _ := json.Marshal(map[string]string{"type": "text", "text": note})
		inner := strings.TrimSpace(content.Raw[1 : len(content.Raw)-1])
		arr := "[" + string(block) + "]"
		if inner != "" {
			arr = "[" + string(block) + "," + inner + "]"
		}
		out, err := sjson.SetRaw(m.Raw, "content", arr)
		if err == nil {
			return out
		}
	}
	return m.Raw
}

// capOutput lowers the body's output field to l's cap, and for a window that counts
// input and output together to what r's estimate leaves, never below 1. A value is
// never raised, and a field that is absent or not a number is left alone.
func capOutput(r fitResult, l limits) fitResult {
	limit := l.Output
	if l.Combined && l.Window > 0 {
		room := max(l.Window-r.estimate, 1)
		if limit == 0 || room < limit {
			limit = room
		}
	}
	if limit == 0 {
		return r
	}
	for _, f := range outputFields {
		v := gjson.GetBytes(r.body, f)
		if v.Type != gjson.Number || int(v.Int()) <= limit {
			continue
		}
		body, err := sjson.SetBytes(r.body, f, limit)
		if err != nil {
			continue
		}
		r.body, r.field, r.from, r.to = body, f, int(v.Int()), limit
	}
	return r
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
