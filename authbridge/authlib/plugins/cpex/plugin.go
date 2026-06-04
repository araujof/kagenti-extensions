//go:build cpex

package cpex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

// cpexConfig is the AuthBridge-side schema for the cpex plugin. The
// plugin extracts the AuthBridge-side keys (hooks, bypass_*) and
// forwards everything else (apl, pipelines, custom CPEX-side blocks)
// verbatim to Manager.LoadConfig as YAML.
//
// See authbridge/docs/plugin-reference.md for the decode → defaults →
// validate convention shared with jwt-validation, token-exchange, and
// ibac.
type cpexConfig struct {
	// Hooks selects which CPEX hook names fire on each AuthBridge
	// pipeline phase. Empty slices mean the plugin is a no-op for
	// that phase — useful when an operator wants CPEX active on
	// request but not response (or vice versa).
	Hooks hooksConfig `json:"hooks" description:"AuthBridge phase → CPEX hook mapping. on_request fires during pctx.OnRequest; on_response fires during pctx.OnResponse."`

	// BypassHosts lists host glob patterns whose requests skip the
	// CPEX chain entirely. Mirrors ibac's host-bypass story for the
	// same reason: traffic to the IdP, observability stack, and the
	// agent's own LLM should never pay the FFI round-trip.
	BypassHosts []string `json:"bypass_hosts" description:"Host globs (path.Match) skipped without crossing the FFI boundary."`

	// BypassPaths lists URL path glob patterns whose requests skip
	// the CPEX chain. Defaults to /healthz, /readyz, /livez, /.well-known/*.
	BypassPaths []string `json:"bypass_paths" description:"URL path globs skipped without crossing the FFI boundary."`

	// APL holds the operator-supplied APL block. Walked by CPEX's
	// APL config visitor when EnableAPL was called before LoadConfig.
	// AuthBridge passes this through as opaque YAML — schema lives in
	// the apl-cpex crate at the pinned FFI version.
	APL json.RawMessage `json:"apl,omitempty" description:"APL config block (identity, pdp, delegator). Forwarded verbatim to LoadConfig."`

	// Pipelines is the CPEX-side pipeline composition keyed by hook
	// name. Same pass-through treatment as APL.
	Pipelines json.RawMessage `json:"pipelines,omitempty" description:"CPEX pipelines per hook. Forwarded verbatim to LoadConfig."`
}

// hooksConfig declares which CPEX hooks the AuthBridge plugin invokes
// on each phase. Lists are evaluated in order; the AuthBridge plugin
// short-circuits on the first sub-plugin that returns SubDeny.
type hooksConfig struct {
	OnRequest  []string `json:"on_request,omitempty" description:"CPEX hook names fired on AuthBridge OnRequest, in order."`
	OnResponse []string `json:"on_response,omitempty" description:"CPEX hook names fired on AuthBridge OnResponse, in order."`
}

// defaultBypassHosts mirrors ibac's conservative starting set so the
// agent's IdP / observability traffic never crosses the FFI boundary.
// Operators extend via config.BypassHosts.
var defaultBypassHosts = []string{
	"keycloak.*",
	"keycloak",
	"spire-server.*",
	"spire-agent.*",
	"otel-collector.*",
	"jaeger.*",
	"prometheus.*",
}

// defaultBypassPaths mirrors ibac's starting set for the same reason —
// liveness / readiness probes should never reach a CPEX sub-plugin.
var defaultBypassPaths = []string{
	"/healthz",
	"/readyz",
	"/livez",
	"/.well-known/*",
}

func (c *cpexConfig) applyDefaults() {
	if len(c.BypassHosts) == 0 {
		c.BypassHosts = defaultBypassHosts
	}
	if len(c.BypassPaths) == 0 {
		c.BypassPaths = defaultBypassPaths
	}
	if c.Hooks.OnRequest == nil {
		c.Hooks.OnRequest = append([]string(nil), defaultRequestHooks...)
	}
	if c.Hooks.OnResponse == nil {
		c.Hooks.OnResponse = append([]string(nil), defaultResponseHooks...)
	}
}

func (c *cpexConfig) validate() error {
	for _, p := range c.BypassHosts {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("invalid bypass_hosts pattern %q: %w", p, err)
		}
		if trimmed := strings.TrimSpace(p); trimmed == "" || trimmed == "*" {
			return fmt.Errorf("bypass_hosts pattern %q matches everything; "+
				"if you mean to disable CPEX, remove it from the pipeline instead", p)
		}
	}
	for _, p := range c.BypassPaths {
		if _, err := path.Match(p, "/"); err != nil {
			return fmt.Errorf("invalid bypass_paths pattern %q: %w", p, err)
		}
		if trimmed := strings.TrimSpace(p); trimmed == "" || trimmed == "*" || trimmed == "/*" {
			return fmt.Errorf("bypass_paths pattern %q matches everything; "+
				"if you mean to disable CPEX, remove it from the pipeline instead", p)
		}
	}
	for _, h := range c.Hooks.OnRequest {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("hooks.on_request: empty hook name")
		}
	}
	for _, h := range c.Hooks.OnResponse {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("hooks.on_response: empty hook name")
		}
	}
	return nil
}

// CPEX is the AuthBridge plugin. It owns one Manager (constructed in
// Configure via the package-level managerFactory) and forwards each
// configured hook to InvokeByName when traffic matches the bypass
// rules.
type CPEX struct {
	cfg         cpexConfig
	manager     Manager
	bypassPaths *bypass.Matcher
	bypassHosts []string
}

// NewCPEX constructs an unconfigured plugin instance. Configure must
// run before the pipeline accepts traffic — Build calls Configure
// for every Configurable plugin so production paths are covered.
func NewCPEX() *CPEX { return &CPEX{} }

func init() {
	plugins.RegisterPlugin("cpex", func() pipeline.Plugin { return NewCPEX() })
}

func (p *CPEX) Name() string { return "cpex" }

func (p *CPEX) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// CMF carries body content into the chain; CPEX sub-plugins
		// inspect / redact via the shared payload.
		ReadsBody: true,
		// Sub-plugins can rewrite the body (validator/pii-scan
		// redacts in place; output sanitizers rewrite the response).
		// Pipeline.New rejects a chain with two WritesBody plugins
		// per direction — operators stacking cpex with another
		// mutator get a clear boot-time error.
		WritesBody: true,
		// CPEX sub-plugins consume the protocol-parser extensions
		// (MCP tool name + args, Inference messages, A2A fragments).
		// Boot-fail if no parser is in the chain so misconfigured
		// pipelines don't ship CPEX with no context.
		RequiresAny: []string{"mcp-parser", "inference-parser", "a2a-parser"},
		Description: "CPEX runtime — runs a chain of policy / guardrail / transform sub-plugins (Rust core via CGO, APL bundled).",
	}
}

// ConfigSchema exposes the AuthBridge-side fields for schema-aware
// tooling. The opaque APL / Pipelines blocks render as RawMessage in
// the schema; the CPEX-side schema is separate (lives in apl-cpex)
// and surfaces in CPEX's own tooling rather than abctl.
func (p *CPEX) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(cpexConfig{})
}

func (p *CPEX) Configure(raw json.RawMessage) error {
	var c cpexConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("cpex config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("cpex config: %w", err)
	}
	p.cfg = c

	matcher, err := bypass.NewMatcher(c.BypassPaths)
	if err != nil {
		return fmt.Errorf("cpex bypass_paths: %w", err)
	}
	p.bypassPaths = matcher
	p.bypassHosts = c.BypassHosts

	mgr, err := managerFactory()
	if err != nil {
		return fmt.Errorf("cpex manager: %w", err)
	}
	p.manager = mgr

	// EnableAPL is unconditional — calling it on a manager whose FFI
	// bundle does not include APL returns an error, which is the
	// right boot-time signal for an operator who mistakenly deployed
	// the non-APL variant. Calling it on an APL-bundled manager when
	// no apl/ pipelines blocks were declared is harmless.
	if err := p.manager.EnableAPL(); err != nil {
		return fmt.Errorf("cpex EnableAPL: %w", err)
	}

	yamlPayload, err := buildLoadConfigYAML(c)
	if err != nil {
		return fmt.Errorf("cpex LoadConfig payload: %w", err)
	}
	if len(yamlPayload) > 0 {
		if err := p.manager.LoadConfig(context.Background(), yamlPayload); err != nil {
			return fmt.Errorf("cpex LoadConfig: %w", err)
		}
	}

	return nil
}

// Init runs the CPEX manager's Initialize hook. The pipeline reports
// "not ready" until this returns, mirroring the credential-file wait
// pattern other plugins use.
func (p *CPEX) Init(ctx context.Context) error {
	if p.manager == nil {
		return errors.New("cpex Init called before Configure")
	}
	if err := p.manager.Initialize(ctx); err != nil {
		return fmt.Errorf("cpex Initialize: %w", err)
	}
	return nil
}

// Shutdown frees the manager. Best-effort: the framework logs and
// continues if shutdown fails.
func (p *CPEX) Shutdown(ctx context.Context) error {
	if p.manager == nil {
		return nil
	}
	return p.manager.Shutdown(ctx)
}

func (p *CPEX) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	return p.runHooks(ctx, pctx, p.cfg.Hooks.OnRequest)
}

func (p *CPEX) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	return p.runHooks(ctx, pctx, p.cfg.Hooks.OnResponse)
}

// runHooks fires the configured CPEX hooks in order, recording one
// Invocation per CPEX sub-plugin per hook and short-circuiting on
// the first sub-plugin that returns SubDeny.
func (p *CPEX) runHooks(ctx context.Context, pctx *pipeline.Context, hooks []string) pipeline.Action {
	if p.manager == nil {
		return pipeline.DenyStatus(503, "cpex.unconfigured", "cpex plugin not configured")
	}
	if len(hooks) == 0 {
		return pipeline.Action{Type: pipeline.Continue}
	}

	if p.bypassPaths != nil && p.bypassPaths.Match(pctx.Path) {
		pctx.Skip("path_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}
	if matchesAnyHost(p.bypassHosts, pctx.Host) {
		pctx.Skip("host_bypass")
		return pipeline.Action{Type: pipeline.Continue}
	}

	req := buildRequest(pctx)
	for _, hook := range hooks {
		res, err := p.manager.InvokeByName(ctx, hook, req)
		if err != nil {
			pctx.Record(pipeline.Invocation{
				Action:  pipeline.ActionDeny,
				Reason:  "cpex_unavailable",
				Details: map[string]string{"hook": hook, "error": preview(err.Error(), 240)},
			})
			return pipeline.DenyStatus(503, "cpex.unavailable", err.Error())
		}
		denyIdx, denyReason := applyResult(pctx, hook, res)
		if denyIdx >= 0 {
			subName := res.SubPlugins[denyIdx].Name
			code := "cpex." + sanitizeReason(subName) + "." + sanitizeReason(denyReason)
			return pipeline.DenyStatus(403, code, res.SubPlugins[denyIdx].Message)
		}
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// buildLoadConfigYAML re-serializes the apl + pipelines subtrees into
// a single YAML document for Manager.LoadConfig. Returns nil when
// neither block is present so an operator who only wants the bypass
// gates can omit the CPEX-side wiring entirely.
func buildLoadConfigYAML(c cpexConfig) ([]byte, error) {
	if len(c.APL) == 0 && len(c.Pipelines) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	if len(c.APL) > 0 {
		var v any
		if err := json.Unmarshal(c.APL, &v); err != nil {
			return nil, fmt.Errorf("apl: %w", err)
		}
		out["apl"] = v
	}
	if len(c.Pipelines) > 0 {
		var v any
		if err := json.Unmarshal(c.Pipelines, &v); err != nil {
			return nil, fmt.Errorf("pipelines: %w", err)
		}
		out["pipelines"] = v
	}
	return yaml.Marshal(out)
}

// matchesAnyHost is identical in spirit to ibac.matchesAnyHost.
// Duplicated rather than shared to avoid a cross-plugin dependency
// for a 12-line helper that may diverge as either plugin's bypass
// vocabulary evolves.
func matchesAnyHost(patterns []string, host string) bool {
	if host == "" {
		return false
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, p := range patterns {
		if matched, _ := path.Match(p, host); matched {
			return true
		}
	}
	return false
}

// sanitizeReason flattens a CPEX sub-plugin name or reason code into
// the dotted form used in AuthBridge violation codes — lowercase
// alphanumerics with underscores. Keeps the operator-visible code
// stable across CPEX-side cosmetic changes.
func sanitizeReason(s string) string {
	if s == "" {
		return "unspecified"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// preview truncates a string to n runes for inclusion in
// Invocation.Details — mirrors ibac.preview so operators reading
// abctl get consistent shapes.
func preview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin       = (*CPEX)(nil)
	_ pipeline.Configurable = (*CPEX)(nil)
	_ pipeline.Initializer  = (*CPEX)(nil)
	_ pipeline.Shutdowner   = (*CPEX)(nil)
)
