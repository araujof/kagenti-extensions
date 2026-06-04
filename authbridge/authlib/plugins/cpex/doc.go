//go:build cpex

// Package cpex bridges AuthBridge's plugin pipeline to the
// ContextForge Plugin Extensibility (CPEX) runtime, exposing CPEX as
// a single first-class AuthBridge plugin named "cpex". Internally the
// plugin runs a configurable chain of CPEX sub-plugins — the Rust
// core plus the bundled APL governance layer (validator/pii-scan,
// audit/logger, identity/jwt, delegator/oauth, PDP cedar-direct) —
// and reports each sub-plugin as its own Invocation row so abctl
// renders the chain inline with the rest of the AuthBridge pipeline.
//
// Threat model. CPEX is a runtime substrate for policy / guardrail /
// transform sub-plugins. AuthBridge embeds it so APL policy
// evaluation, PII redaction, audit logging, and identity / delegator
// resolution can all participate in the same pipeline that already
// runs auth gates and protocol parsers. The plugin contributes no
// new threat-model surface beyond what its configured sub-plugins
// add: it ships traffic context (CMF / Extensions) into CPEX and
// translates CPEX's verdict into AuthBridge actions.
//
// Build constraint. The package is built only with the `cpex` build
// tag because the embedded CPEX runtime is CGO + a linked static
// library (`libcpex_ffi.a`). The default `go test ./...` /
// `go build ./...` continues to ignore this package, so the standard
// AuthBridge images stay pure-Go cross-compileable. The new
// `cmd/authbridge-cpex/` binary sets `-tags cpex` and
// `CGO_ENABLED=1` to link the FFI library; unit tests run with
// `-tags cpex` against a fake Manager and need neither CGO nor the
// `.a`.
//
// Manager indirection. The plugin talks to CPEX through the small
// Manager interface in this package; the real CGO-backed
// implementation lives in `cmd/authbridge-cpex/` and is wired in via
// SetManagerFactory at process boot. This keeps `authlib`'s go.mod
// free of the CPEX Go binding dependency and lets unit tests inject
// a fake Manager without dragging the linked library into the test
// binary.
package cpex
