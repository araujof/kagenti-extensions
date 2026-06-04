//go:build cpex

package cpex

// CPEX hook names. AuthBridge maps OnRequest / OnResponse onto
// configurable subsets of these. The exact set CPEX accepts is
// governed by whichever sub-plugins are registered on the manager;
// validation of unknown hook names happens at LoadConfig time on
// the CPEX side.
//
// Documented here so operators reading the AuthBridge plugin's
// config schema can refer to the same names CPEX uses.
const (
	// HookOnInput fires on the inbound side when the upstream caller
	// has finished sending a request. Canonical APL home for
	// validator/pii-scan and identity/jwt resolution.
	HookOnInput = "on-input"

	// HookToolPreInvoke fires before AuthBridge forwards an
	// outbound MCP tool call. Canonical APL home for PDP checks
	// (pdp/cedar-direct, pdp/cedarling).
	HookToolPreInvoke = "tool-pre-invoke"

	// HookToolPostInvoke fires after the outbound tool call returns,
	// before the result reaches the agent. Canonical APL home for
	// audit/logger and post-call delegators.
	HookToolPostInvoke = "tool-post-invoke"

	// HookOnOutput fires when the agent's reply is about to leave
	// the pod. Canonical APL home for output redactors.
	HookOnOutput = "on-output"
)

// defaultRequestHooks is the empty set: operators must opt in
// explicitly. The plan exposes hooks via config; defaults stay
// conservative so a misconfigured chain is a no-op rather than a
// surprise traffic transformation.
var defaultRequestHooks []string

// defaultResponseHooks mirrors the request side.
var defaultResponseHooks []string
