//go:build cpex && cgo

package main

import (
	"context"
	"fmt"

	cpexbinding "github.com/contextforge-org/contextforge-plugins-framework/go/cpex"

	cpexplugin "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/cpex"
)

// realManager wraps cpex.PluginManager from the CPEX Go binding and
// adapts it to the cpexplugin.Manager interface. The package-level
// SetManagerFactory call in main.go installs newRealManager so that
// when the cpex plugin's Configure runs, it gets a manager backed by
// the linked libcpex_ffi.a.
//
// The lifecycle is fixed by the binding: NewPluginManagerDefault →
// EnableAPL → LoadConfig → Initialize → InvokeByName → Shutdown.
// Any deviation panics on the binding side; the wrapper preserves
// that order and translates errors into the plugin's surface.
//
// Build constraints: this file is gated by `cpex && cgo`. With CGO
// disabled (CGO_ENABLED=0) the stub in realmanager_nocgo.go takes
// over and returns a clear "binary built without CGO" error from
// newRealManager, so the binary can still be `go vet`/`go build`-ed
// without the FFI library on disk during local development. Production
// builds always set CGO_ENABLED=1.
type realManager struct {
	mgr *cpexbinding.PluginManager
}

// newRealManager constructs the binding-backed manager. Called by the
// cpex plugin's Configure via the package-level managerFactory slot.
func newRealManager() (cpexplugin.Manager, error) {
	mgr, err := cpexbinding.NewPluginManagerDefault()
	if err != nil {
		return nil, fmt.Errorf("cpex.NewPluginManagerDefault: %w", err)
	}
	return &realManager{mgr: mgr}, nil
}

func (r *realManager) EnableAPL() error {
	return r.mgr.EnableAPL()
}

func (r *realManager) LoadConfig(_ context.Context, yaml []byte) error {
	return r.mgr.LoadConfig(yaml)
}

func (r *realManager) Initialize(_ context.Context) error {
	return r.mgr.Initialize()
}

func (r *realManager) InvokeByName(_ context.Context, hook string, req *cpexplugin.Request) (*cpexplugin.Result, error) {
	bindingReq := toBindingRequest(req)
	bindingRes, err := r.mgr.InvokeByName(hook, bindingReq)
	if err != nil {
		return nil, err
	}
	return fromBindingResult(bindingRes), nil
}

func (r *realManager) Shutdown(_ context.Context) error {
	return r.mgr.Shutdown()
}

// toBindingRequest projects the package-internal Request type onto the
// CPEX binding's Extensions / ContextTable shape. Kept narrow: only
// fields the CPEX-side sub-plugins consume cross over.
//
// The binding's exact field shape is governed by the pinned
// CPEX_FFI_VERSION; bumping the version may require updating this
// adapter. The ABI version check in cpexbinding's init() is the
// guardrail against silent drift.
func toBindingRequest(req *cpexplugin.Request) *cpexbinding.Request {
	if req == nil {
		return &cpexbinding.Request{}
	}
	out := &cpexbinding.Request{
		Method:    req.Method,
		Scheme:    req.Scheme,
		Host:      req.Host,
		Path:      req.Path,
		Headers:   req.SafeHeaders,
		Body:      req.Body,
		RequestID: req.RequestID,
		StartedAt: req.StartedUnixMilli,
	}
	if req.IdentitySubject != "" || len(req.IdentityClaims) > 0 {
		out.Identity = &cpexbinding.Identity{
			Subject: req.IdentitySubject,
			Claims:  req.IdentityClaims,
		}
	}
	if req.MCP != nil {
		out.MCP = &cpexbinding.MCPInfo{Method: req.MCP.Method, Params: req.MCP.Params}
	}
	if req.Inference != nil {
		msgs := make([]cpexbinding.InferenceMessage, 0, len(req.Inference.Messages))
		for _, m := range req.Inference.Messages {
			msgs = append(msgs, cpexbinding.InferenceMessage{Role: m.Role, Content: m.Content})
		}
		out.Inference = &cpexbinding.InferenceInfo{Model: req.Inference.Model, Messages: msgs}
	}
	if req.A2A != nil {
		parts := make([]cpexbinding.A2APart, 0, len(req.A2A.Parts))
		for _, p := range req.A2A.Parts {
			parts = append(parts, cpexbinding.A2APart{Kind: p.Kind, Content: p.Content})
		}
		out.A2A = &cpexbinding.A2AInfo{Method: req.A2A.Method, Role: req.A2A.Role, Parts: parts}
	}
	if len(req.Custom) > 0 {
		out.Custom = req.Custom
	}
	return out
}

// fromBindingResult inverts toBindingRequest for the response. Each
// PipelineResult entry from the binding becomes one SubPluginOutcome;
// body / header / label rewrites carry over verbatim.
func fromBindingResult(in *cpexbinding.PipelineResult) *cpexplugin.Result {
	if in == nil {
		return &cpexplugin.Result{}
	}
	out := &cpexplugin.Result{
		ModifiedBody:    in.ModifiedBody,
		ModifiedHeaders: in.ModifiedHeaders,
		AddedLabels:     in.AddedLabels,
	}
	for _, e := range in.SubPlugins {
		out.SubPlugins = append(out.SubPlugins, cpexplugin.SubPluginOutcome{
			Name:    e.Name,
			Action:  cpexplugin.SubAction(e.Action),
			Reason:  e.Reason,
			Message: e.Message,
			Details: e.Details,
		})
	}
	return out
}
