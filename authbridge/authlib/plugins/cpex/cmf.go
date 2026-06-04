//go:build cpex

package cpex

import (
	"net/http"
	"strings"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// secretHeaderPrefixes lists header-name prefixes that are NEVER
// forwarded into the CMF payload. CPEX sub-plugins should never see
// raw bearer tokens, session cookies, or platform-issued internal
// secret headers — the session API has no auth and audit/logger
// sub-plugins typically log the payload they receive.
//
// The prefix set mirrors what ibac.describeAction strips before
// shipping a prompt to the judge LLM. Keep the two lists in sync:
// any name added here should also be added in ibac if it carries a
// secret in any production deployment.
var secretHeaderPrefixes = []string{
	"authorization",
	"cookie",
	"set-cookie",
	"proxy-authorization",
	"x-amz-security-token",
}

// secretHeaderExact lists exact (case-insensitive) header names to
// strip. Used for one-off names that aren't easily expressed as
// prefixes.
var secretHeaderExact = map[string]struct{}{
	"x-api-key":           {},
	"x-auth-token":        {},
	"x-authorization":     {},
	"x-secret-token":      {},
	"x-session-token":     {},
	"x-csrf-token":        {},
	"x-platform-secret":   {},
	"x-authbridge-secret": {},
}

// safeHeaders returns a copy of h with secret-bearing names removed.
// Used by buildRequest before forwarding the request to CPEX.
func safeHeaders(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for name, values := range h {
		lower := strings.ToLower(name)
		if _, ok := secretHeaderExact[lower]; ok {
			continue
		}
		stripped := false
		for _, prefix := range secretHeaderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				stripped = true
				break
			}
		}
		if stripped {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildRequest projects pctx into the package's CMF-shaped Request
// type, ready to hand to Manager.InvokeByName. The translation is
// deliberately narrow: only fields CPEX sub-plugins consume cross
// over, and secret-bearing headers are dropped here, not at the
// realManager boundary, so the test suite can verify the redaction
// without the binding stack-up.
func buildRequest(pctx *pipeline.Context) *Request {
	if pctx == nil {
		return &Request{}
	}
	req := &Request{
		Method:      pctx.Method,
		Scheme:      pctx.Scheme,
		Host:        pctx.Host,
		Path:        pctx.Path,
		SafeHeaders: safeHeaders(pctx.Headers),
		Body:        pctx.Body,
		RequestID:   pctx.Headers.Get("X-Request-Id"),
	}
	if !pctx.StartedAt.IsZero() {
		req.StartedUnixMilli = pctx.StartedAt.UnixMilli()
	}
	if pctx.Identity != nil {
		req.IdentitySubject = pctx.Identity.Subject()
		// Surface scopes + client_id as claims so CPEX policy can
		// read them without the sub-plugin having to know which
		// AuthBridge plugin populated identity.
		claims := map[string]any{}
		if cid := pctx.Identity.ClientID(); cid != "" {
			claims["client_id"] = cid
		}
		if scopes := pctx.Identity.Scopes(); len(scopes) > 0 {
			claims["scope"] = strings.Join(scopes, " ")
		}
		if len(claims) > 0 {
			req.IdentityClaims = claims
		}
	}
	if mcp := pctx.Extensions.MCP; mcp != nil {
		req.MCP = &MCPInfo{Method: mcp.Method, Params: mcp.Params}
	}
	if inf := pctx.Extensions.Inference; inf != nil {
		msgs := make([]InferenceMessage, 0, len(inf.Messages))
		for _, m := range inf.Messages {
			msgs = append(msgs, InferenceMessage{Role: m.Role, Content: m.Content})
		}
		req.Inference = &InferenceInfo{Model: inf.Model, Messages: msgs}
	}
	if a := pctx.Extensions.A2A; a != nil {
		parts := make([]A2APart, 0, len(a.Parts))
		for _, p := range a.Parts {
			parts = append(parts, A2APart{Kind: p.Kind, Content: p.Content})
		}
		req.A2A = &A2AInfo{Method: a.Method, Role: a.Role, Parts: parts}
	}
	if pctx.Extensions.Custom != nil {
		// Forward only entries scoped under the "cpex/" prefix —
		// other Custom entries are private state of unrelated
		// plugins (rate-limiter cookies, etc.) and shouldn't cross
		// the FFI boundary.
		var picked map[string]any
		for k, v := range pctx.Extensions.Custom {
			if !strings.HasPrefix(k, "cpex/") {
				continue
			}
			if picked == nil {
				picked = map[string]any{}
			}
			picked[strings.TrimPrefix(k, "cpex/")] = v
		}
		req.Custom = picked
	}
	return req
}

// applyResult wires a CPEX Result back into pctx — body / headers /
// labels / per-sub-plugin invocations. Returns whether the body was
// actually mutated and the deny outcome (if any) that should stop
// the AuthBridge pipeline.
//
// The function deliberately keeps body-mutation routing through
// pctx.SetBody so AuthBridge's framework-level mutation invariants
// (Content-Length recomputation, the auto-emitted modify Invocation,
// observe-mode shadow accounting) all hold.
func applyResult(pctx *pipeline.Context, hook string, res *Result) (denyIdx int, denyReason string) {
	denyIdx = -1
	if res == nil {
		return
	}
	for i, sp := range res.SubPlugins {
		recordSubPlugin(pctx, hook, sp)
		if sp.Action == SubDeny && denyIdx < 0 {
			denyIdx = i
			denyReason = sp.Reason
		}
	}
	if len(res.ModifiedHeaders) > 0 {
		for name, values := range res.ModifiedHeaders {
			pctx.Headers.Del(name)
			for _, v := range values {
				pctx.Headers.Add(name, v)
			}
		}
	}
	if len(res.AddedLabels) > 0 {
		mergeLabels(pctx, res.AddedLabels)
	}
	if res.ModifiedBody != nil {
		pctx.SetBody(res.ModifiedBody)
	}
	return denyIdx, denyReason
}

// recordSubPlugin emits one AuthBridge Invocation per CPEX
// sub-plugin outcome. The plugin name is namespaced as
// "cpex/<sub-plugin>" so abctl renders the chain inline alongside
// AuthBridge-native plugins without colliding on names.
func recordSubPlugin(pctx *pipeline.Context, hook string, sp SubPluginOutcome) {
	details := map[string]string{"hook": hook}
	for k, v := range sp.Details {
		details[k] = v
	}
	if sp.Message != "" {
		details["message"] = sp.Message
	}
	pctx.Record(pipeline.Invocation{
		Plugin:  "cpex/" + sp.Name,
		Action:  translateAction(sp.Action),
		Reason:  sp.Reason,
		Details: details,
	})
}

// translateAction maps CPEX's verdict vocabulary to AuthBridge's
// 5-value InvocationAction. Unknown values land on Observe so a
// future CPEX-side addition does not silently classify as deny.
func translateAction(a SubAction) pipeline.InvocationAction {
	switch a {
	case SubAllow:
		return pipeline.ActionAllow
	case SubDeny:
		return pipeline.ActionDeny
	case SubModify:
		return pipeline.ActionModify
	case SubSkip:
		return pipeline.ActionSkip
	case SubObserve:
		return pipeline.ActionObserve
	}
	return pipeline.ActionObserve
}

// mergeLabels appends labels into pctx.Extensions.Custom["cpex/labels"]
// as a []string. Downstream guardrails read this slice to discover
// taints CPEX added.
func mergeLabels(pctx *pipeline.Context, labels []string) {
	if len(labels) == 0 {
		return
	}
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	const key = "cpex/labels"
	switch existing := pctx.Extensions.Custom[key].(type) {
	case []string:
		pctx.Extensions.Custom[key] = append(existing, labels...)
	default:
		pctx.Extensions.Custom[key] = append([]string(nil), labels...)
	}
}
