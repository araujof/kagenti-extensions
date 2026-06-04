//go:build cpex

package cpex

import (
	"context"
	"errors"
)

// Manager is the small surface this package needs from a CPEX runtime.
// It mirrors the public-facing methods of cpex.PluginManager from the
// CPEX Go binding (NewPluginManagerDefault → EnableAPL → LoadConfig →
// Initialize → InvokeByName → Shutdown), expressed in types this
// package owns so authlib does not have to import the CPEX binding.
//
// The real implementation that wraps cpex.PluginManager lives in
// cmd/authbridge-cpex/realmanager.go (build tag `cpex && cgo`); it is
// installed at boot via SetManagerFactory. Unit tests under `-tags
// cpex` substitute a fake Manager directly via SetManagerFactory or by
// assigning to the plugin's manager field, so neither CGO nor the
// linked `.a` is required to exercise the plugin's decode + CMF
// mapping logic.
type Manager interface {
	// EnableAPL installs the APL config visitor and registers the
	// bundled APL plugin / PDP factories on the manager. Must be
	// called BEFORE LoadConfig so the visitor is in place when
	// LoadConfig walks the YAML's `apl:` / `pipelines:` blocks.
	// Calling EnableAPL on a manager whose underlying FFI bundle
	// does not include APL (a future variant build) returns an
	// error rather than panicking.
	EnableAPL() error

	// LoadConfig parses the YAML payload and registers the configured
	// sub-plugins / pipelines on the manager. The payload is whatever
	// the operator declared under the AuthBridge plugin's
	// `config.apl` / `config.pipelines` keys, marshaled back to YAML.
	// Validation errors surface as the returned error so AuthBridge
	// can fail boot loudly.
	LoadConfig(ctx context.Context, yaml []byte) error

	// Initialize runs the manager's startup work — typically
	// instantiating sub-plugins, warming caches, connecting to PDPs.
	// Called from the AuthBridge plugin's Init hook; the plugin
	// reports not-ready until this returns.
	Initialize(ctx context.Context) error

	// InvokeByName runs the named CPEX hook (e.g. "on-input",
	// "tool-pre-invoke", "tool-post-invoke", "on-output") with the
	// supplied request payload, returning one result entry per
	// sub-plugin that ran. The AuthBridge plugin records one
	// Invocation per entry so abctl renders the chain inline.
	InvokeByName(ctx context.Context, hook string, req *Request) (*Result, error)

	// Shutdown frees the manager and drains background tasks.
	Shutdown(ctx context.Context) error
}

// ManagerFactory constructs a fresh Manager. The real factory wraps
// cpex.NewPluginManagerDefault and lives in the cmd binary. Tests
// install fake factories via SetManagerFactory.
type ManagerFactory func() (Manager, error)

// errNoManagerFactory is returned by Configure when no factory has
// been installed. The cmd/authbridge-cpex binary must call
// SetManagerFactory at boot before the plugin pipeline is built.
var errNoManagerFactory = errors.New(
	"cpex: no manager factory installed (the authbridge-cpex binary " +
		"must call cpex.SetManagerFactory before the pipeline is built)")

// managerFactory is the package-level slot the plugin's Configure
// uses to construct a fresh Manager. The default value returns a
// "no factory" error so a misconfigured binary fails loudly at
// Configure rather than silently serving traffic without CPEX.
var managerFactory ManagerFactory = func() (Manager, error) {
	return nil, errNoManagerFactory
}

// SetManagerFactory installs the constructor used to build the CPEX
// Manager when the plugin is configured. The cmd/authbridge-cpex
// binary calls this from main() with a factory that returns a
// realManager wrapping cpex.PluginManager. Tests that exercise the
// full lifecycle install a fake factory; tests that bypass the
// lifecycle assign to the plugin's manager field directly.
//
// SetManagerFactory is process-global state. Calling it more than
// once is allowed (the most recent factory wins) — convenient for
// tests that toggle between fakes, irrelevant in production where
// only main() calls it.
func SetManagerFactory(f ManagerFactory) {
	if f == nil {
		f = func() (Manager, error) { return nil, errNoManagerFactory }
	}
	managerFactory = f
}

// Request is the CMF-shaped payload handed to InvokeByName. Fields
// are populated by cmf.go from pctx.
//
// Kept package-private rather than mirroring the full cpex binding
// Extensions struct so realManager owns the binding-specific
// translation. Adding a new slot here means: bump this struct +
// update buildCMF + update realManager's adapter, all in lockstep.
type Request struct {
	// Method, Scheme, Host, Path mirror the inbound HTTP request line
	// (or the outbound request being attempted). Plugins that key on
	// URL shape consume these.
	Method string
	Scheme string
	Host   string
	Path   string

	// SafeHeaders carries a subset of the original request headers —
	// secret-bearing names (Authorization, Cookie, X-*-Token, etc.)
	// are stripped by cmf.go before they ever cross the FFI boundary.
	// CPEX sub-plugins that need to react to a header (Content-Type,
	// User-Agent) read from this map.
	SafeHeaders map[string][]string

	// Body is the raw request body. Set only when at least one
	// AuthBridge plugin in the chain reads the body — the framework
	// guarantees pctx.Body is populated before OnRequest dispatches.
	Body []byte

	// IdentitySubject and IdentityClaims populate the Security
	// Identity slot in CPEX's CMF when an upstream auth plugin
	// established identity. Empty when no identity is available.
	IdentitySubject string
	IdentityClaims  map[string]any

	// MCP / Inference / A2A surface the parsed protocol extension
	// when the corresponding AuthBridge parser ran earlier in the
	// chain. CPEX sub-plugins that key on tool name, model, or A2A
	// fragments read these.
	MCP       *MCPInfo
	Inference *InferenceInfo
	A2A       *A2AInfo

	// Custom is the free-form passthrough — operators that ship
	// out-of-tree CPEX sub-plugins can stash plugin-specific blobs in
	// pctx.Extensions.Custom["cpex/<name>"] and they land here.
	Custom map[string]any

	// RequestID and StartedUnixMilli identify and timestamp the
	// request. Populated from pctx.Headers["X-Request-Id"] / pctx.StartedAt.
	RequestID        string
	StartedUnixMilli int64
}

// MCPInfo is the subset of pipeline.MCPExtension forwarded to CPEX.
// Mirrors the wire-relevant fields (Method + Params for tool calls);
// telemetry-only fields stay on the AuthBridge side.
type MCPInfo struct {
	Method string
	Params map[string]any
}

// InferenceInfo is the subset of pipeline.InferenceExtension forwarded
// to CPEX. Carries the model name and message payload so the LLM-PII
// redactor and similar inspection sub-plugins can run.
type InferenceInfo struct {
	Model    string
	Messages []InferenceMessage
}

// InferenceMessage mirrors pipeline.InferenceMessage in the local
// types.
type InferenceMessage struct {
	Role    string
	Content string
}

// A2AInfo is the subset of pipeline.A2AExtension forwarded to CPEX —
// just the role + parts that downstream policy plugins might key on.
type A2AInfo struct {
	Method string
	Role   string
	Parts  []A2APart
}

// A2APart mirrors pipeline.A2APart locally.
type A2APart struct {
	Kind    string
	Content string
}

// Result is the outcome of one InvokeByName call. It carries one
// SubPluginOutcome per sub-plugin that ran in the hook's pipeline,
// plus the optional ModifiedBody when a sub-plugin rewrote the
// request payload (the bundled validator/pii-scan factory, for
// instance, redacts PII matches in-place).
type Result struct {
	// SubPlugins is in declared order — the AuthBridge plugin records
	// one Invocation per entry so abctl preserves the chain shape.
	SubPlugins []SubPluginOutcome

	// ModifiedBody, when non-nil, replaces the request body. Only one
	// sub-plugin in the chain may rewrite the body (CPEX itself
	// enforces); AuthBridge's WritesBody capability surfaces a
	// downstream conflict (another mutator in the same direction) at
	// pipeline build time.
	ModifiedBody []byte

	// ModifiedHeaders, when non-empty, patches the request headers
	// before they leave AuthBridge. Plugins that synthesize
	// Authorization headers (e.g. apl-delegator-oauth) populate this.
	ModifiedHeaders map[string][]string

	// AddedLabels surfaces taint labels emitted by CPEX sub-plugins;
	// the AuthBridge plugin merges these into pctx.Extensions.Custom
	// under "cpex/labels" so downstream guardrails can read them.
	AddedLabels []string
}

// SubPluginOutcome is the per-sub-plugin verdict translated from the
// CPEX PipelineResult into AuthBridge's vocabulary. Action maps to one
// of the 5-value AuthBridge invocation actions; the cpex plugin's
// translateAction enforces the mapping.
type SubPluginOutcome struct {
	// Name is the CPEX sub-plugin's registered name (e.g.
	// "validator/pii-scan", "audit/logger", "pdp/cedar-direct"). The
	// AuthBridge plugin prefixes it with "cpex/" when recording the
	// Invocation so operators can see at a glance which row came
	// from CPEX.
	Name string

	// Action is the CPEX-side verdict.
	Action SubAction

	// Reason is a short stable code emitted by the sub-plugin (e.g.
	// "redacted_ssn", "scope_missing", "policy_permitted"). Used as
	// the Invocation.Reason on the AuthBridge side.
	Reason string

	// Message is human-prose context the sub-plugin returned. It
	// surfaces in Invocation.Details["message"] when non-empty;
	// the AuthBridge plugin never logs it at WARN/ERROR level.
	Message string

	// Details carries arbitrary key/value pairs the sub-plugin
	// emitted. Forwarded onto Invocation.Details verbatim.
	Details map[string]string
}

// SubAction is the CPEX-side action vocabulary. Translated 1:1 to
// AuthBridge's 5-value InvocationAction by translateAction.
type SubAction string

const (
	// SubAllow — sub-plugin permitted the request.
	SubAllow SubAction = "allow"
	// SubDeny — sub-plugin rejected the request; AuthBridge
	// surfaces a Reject action upstream.
	SubDeny SubAction = "deny"
	// SubModify — sub-plugin mutated the request body / headers /
	// labels. Carried back through Result.ModifiedBody etc.
	SubModify SubAction = "modify"
	// SubObserve — sub-plugin produced diagnostics without changing
	// flow (audit/logger is the canonical case).
	SubObserve SubAction = "observe"
	// SubSkip — sub-plugin opted not to act on this request.
	SubSkip SubAction = "skip"
)
