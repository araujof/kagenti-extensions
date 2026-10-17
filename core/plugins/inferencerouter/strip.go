package inferencerouter

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"

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

const (
	// originPrefix is where the router records, per session, the server that
	// produced the reasoning its requests carry. It lives where pins do, with the
	// pin's TTL, and X does not clear it.
	originPrefix = "inference-router/origin/"
	// originForeign marks a session whose reasoning has come from more than one
	// server. It is sticky: the client's history is its own, so blocks from the
	// old server stay in it however many turns pass on the new one, and the origin
	// must never resolve back to a single server. No server name can collide with
	// it: config refuses parentheses in a name.
	originForeign = "(foreign)"
)

// originKey is the store key for pctx's session's origin, "" when it has none a
// record could name.
func originKey(pctx *pipeline.Context) string {
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return ""
	}
	return originPrefix + pctx.Session.ID
}

// origin is the session's recorded reasoning origin, "" when none is.
func (p *Router) origin(pctx *pipeline.Context, key string) string {
	if p.store != nil {
		v, err := p.store.Get(context.Background(), key)
		if err != nil {
			return ""
		}
		return v
	}
	if pctx.Shared != nil {
		if v, ok := pctx.Shared.Get(key); ok {
			s, _ := v.(string)
			return s
		}
	}
	return ""
}

// keepOrigin records v as the session's reasoning origin, for as long as a pin is
// kept: the origin outlives a restart beside the pin it explains.
func (p *Router) keepOrigin(pctx *pipeline.Context, key, v string) {
	if p.store != nil {
		if err := p.store.Set(context.Background(), key, v, pinTTL); err != nil {
			slog.Warn("inference-router: could not record a session's reasoning origin", "error", err)
		}
		return
	}
	if pctx.Shared != nil {
		pctx.Shared.Put(key, v, pinTTL)
	}
}

// hasPriorPin reports whether the session holds any pin at all, its agent's or
// another's, "not routed" included. Such a session had requests before this one,
// so the reasoning in its history came from somewhere other than the target; a
// session with no pin is the only one whose history cannot hold reasoning from a
// life the router saw.
func (p *Router) hasPriorPin(pctx *pipeline.Context, key string) bool {
	_, id, ok := strings.Cut(key, originPrefix)
	if !ok {
		return false
	}
	store := p.pinsFor(pctx)
	if store == nil {
		return false
	}
	_, had := store.load(pinPrefix + id)
	return had
}

// stripForeign drops the reasoning a request carries that its target cannot have
// produced, and records the target as the session's reasoning origin. It runs only
// where the router applied a server (Redirected), for a session's
// first routed request it records and strips nothing, and once a session's target
// has differed from its recorded origin the origin is foreign for good and every
// switched request strips, the target's own reasoning included: a server that
// cannot verify foreign blocks may not get them, and one that can loses only
// reasoning text, which no measured continuation needed.
func (p *Router) stripForeign(pctx *pipeline.Context, server string) {
	// Redirected, not switched: a moved session's client asks its own provider,
	// which after the move is the target's own host, and a same-host redirect is
	// not "switched" — but the reasoning in its history still came from elsewhere.
	// The router applying a server at all is the signal provenance needs.
	if !pctx.Redirected() {
		return
	}
	key := originKey(pctx)
	if key == "" {
		return
	}
	if origin := p.origin(pctx, key); origin != "" {
		if origin == server {
			return
		}
		p.keepOrigin(pctx, key, originForeign)
	} else if p.hasPriorPin(pctx, key) {
		// A session with a pin has a prior inference life the router saw: unrouted,
		// on the client's own provider, with that provider's signed reasoning in its
		// history. Seeding the origin with the target would never strip it, and a
		// target that cannot verify foreign blocks would refuse it for good.
		p.keepOrigin(pctx, key, originForeign)
	} else {
		p.keepOrigin(pctx, key, server)
		return
	}
	body, dropped := stripReasoning(pctx.Body)
	if dropped == 0 {
		return
	}
	if pctx.SetBody(body) {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "reasoning_stripped",
			Details: map[string]string{"blocks": strconv.Itoa(dropped)}})
	}
}
