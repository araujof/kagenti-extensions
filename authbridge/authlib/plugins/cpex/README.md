# cpex plugin

AuthBridge plugin that embeds the [ContextForge Plugin Extensibility
(CPEX)](https://github.com/contextforge-org/cpex) runtime as a
first-class entry in the AuthBridge pipeline. The plugin owns one
CPEX `PluginManager` and forwards each configured AuthBridge
hook (request / response phase) to a CPEX hook (`on-input`,
`tool-pre-invoke`, `tool-post-invoke`, `on-output`). Each CPEX
sub-plugin that ran in the hook's pipeline surfaces as its own
`Invocation` row in `/v1/sessions`, so abctl renders the chain
inline alongside AuthBridge-native plugins.

The bundled APL governance layer (`validator/pii-scan`,
`audit/logger`, `identity/jwt`, `delegator/oauth`, PDP
`cedar-direct`) ships in the same `libcpex_ffi.a` artifact and is
available out of the box — see `docs/cpex-plugin.md` for the full
config reference and threat model.

## Build

This package is gated behind the `cpex` build tag because it relies
on a CGO-linked static library. The default `go build ./...` /
`go test ./...` ignores the package; the dedicated
`cmd/authbridge-cpex/` binary builds with `-tags cpex` and
`CGO_ENABLED=1`. Unit tests run with `-tags cpex` against a fake
`Manager`, so neither CGO nor the `.a` is required to exercise the
plugin's decode + CMF mapping logic.

## Minimal config

```yaml
- name: cpex
  config:
    hooks:
      on_request:  ["on-input", "tool-pre-invoke"]
      on_response: ["tool-post-invoke", "on-output"]
    apl:
      identity:
        kind: identity/jwt
        config:
          jwks_url: ${KEYCLOAK_URL}/realms/${KEYCLOAK_REALM}/protocol/openid-connect/certs
      pdp:
        kind: cedar-direct
        config:
          policies_file: /etc/cpex/policies.cedar
    pipelines:
      - hook: tool-pre-invoke
        steps:
          - kind: pdp/cedar-direct
            config:
              action: tool:invoke
```

## Enabling

Switch the sidecar image to
`ghcr.io/kagenti/kagenti-extensions/authbridge-cpex:<tag>`. The
default `authbridge` image does not link the FFI library; using `cpex`
with it boots-fails with an unknown-plugin error.

## See also

- `docs/cpex-plugin.md` — full operator + integrator doc
- [CPEX upstream](https://github.com/contextforge-org/cpex) —
  sub-plugin authoring, FFI release artifacts, FFI ABI policy
