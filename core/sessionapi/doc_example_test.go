package sessionapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestDocExamplePromptContextMatchesWire pins that the promptContext value in
// session-api.md's GET /v1/sessions example unmarshals into the type the
// server actually sends. The example showed a bare number where the wire
// carries an object {tokens, stated, at, msgs} (PR #1423 review).
func TestDocExamplePromptContextMatchesWire(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "session-api.md"))
	if err != nil {
		t.Fatalf("read docs/session-api.md: %v", err)
	}
	m := regexp.MustCompile(`"promptContext": (\{[^}]*\}|-?\d+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("session-api.md's GET /v1/sessions example carries no promptContext value")
	}
	var pc pipeline.PromptContext
	if err := json.Unmarshal(m[1], &pc); err != nil {
		t.Errorf("the example's promptContext %s does not match the wire type pipeline.PromptContext: %v", m[1], err)
	}
	if pc.Tokens == 0 {
		t.Errorf("the example's promptContext %s has tokens = 0; an example should carry a real figure", m[1])
	}
}
