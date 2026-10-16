package inferencerouter

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripReasoning drops from body the reasoning only its producer can validate,
// and returns the body it made with how many artifacts it dropped. Anthropic
// messages carry it as thinking and redacted_thinking blocks in an assistant
// message's content; responses carries it as items of type reasoning in input.
// Chat completions carries reasoning as plain-text fields every measured server
// accepts, and matches neither shape, so it is left alone — the artifacts name
// themselves; no format is sniffed. An assistant message the drop empties goes
// whole: an empty content array is refused at Anthropic, and a thinking-only
// turn carries no tool_use for a tool_result to reference.
func stripReasoning(body []byte) ([]byte, int) {
	dropped := 0
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		arr := msgs.Array()
		for i := len(arr) - 1; i >= 0; i-- { // reverse: a deletion shifts no later path
			m := arr[i]
			if m.Get("role").String() != "assistant" {
				continue
			}
			content := m.Get("content")
			if !content.IsArray() {
				continue
			}
			blocks := content.Array()
			kept := make([]string, 0, len(blocks))
			for _, b := range blocks {
				switch b.Get("type").String() {
				case "thinking", "redacted_thinking":
					dropped++
				default:
					kept = append(kept, b.Raw)
				}
			}
			if len(kept) == len(blocks) {
				continue
			}
			if len(kept) == 0 {
				body, _ = sjson.DeleteBytes(body, "messages."+strconv.Itoa(i))
				continue
			}
			body, _ = sjson.SetRawBytes(body, "messages."+strconv.Itoa(i)+".content",
				[]byte("["+strings.Join(kept, ",")+"]"))
		}
	}
	if items := gjson.GetBytes(body, "input"); items.IsArray() {
		arr := items.Array()
		for i := len(arr) - 1; i >= 0; i-- {
			if arr[i].Get("type").String() == "reasoning" {
				body, _ = sjson.DeleteBytes(body, "input."+strconv.Itoa(i))
				dropped++
			}
		}
	}
	return body, dropped
}
