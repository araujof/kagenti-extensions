# CPEX

> **Deployment note:** `cortex-cpex` is a **build variant** of the
> AuthBridge proxy-sidecar (`cortex`), deployed *in place of*
> `cortex` — not an additional sidecar. The operator selects
> the image when CPEX policy enforcement is needed for a workload.

The CPEX plugin routes AuthBridge pipeline hooks through the [CPEX](https://github.com/contextforge-org/cpex) framework,
so operators can define authorization flows inline and declaratively, and turn decisions into ordered effects (including invoking CPEX plugins).

## Why CPEX

PDPs like OPA and Cedar are designed for stateless policy decisions (allow/deny).
CPEX leverages PDPs and organizes policy decisions for agentic workloads.
It calls your PDPs at the level of agentic entities (tools, prompts, models, A2A methods),
composes their verdicts with custom policy written inline, and turns each decision into ordered effects.

That orchestration allows you to see the full scope of your authorization flow (not obfuscated in code, not hidden behind an API):

- **Cross-call session reasoning.** An agent reads compensation data, then sends
  an email. Each call is allowed on its own. CPEX carries session labels across
  calls, so a policy can see the sequence and block the exfiltration.
- **Pre and post-result enforcement.** A tool returns a bulk directory dump the gateway
  never saw before the call fired. The pre-invoke check ran before the result
  existed, so CPEX adds a post-result phase that evaluates the actual response.
- **Ordered effects beyond allow/deny.** A decision drives concrete actions:
  redact a response field, call a guardrail, write an audit record, in a defined order.
  A PDP's allow or deny becomes the trigger for these effects through `on_allow` / `on_deny`.

## How it works

AuthBridge parses traffic into a typed pipeline context ([inspired by CPEX's specification](https://github.com/rossoctl/cortex/commit/4c53164d02809ddb19f3b79b3abf9c288a8bc4fb)).
The cpex plugin projects that context into CMF (a normalized, typed policy context), invokes the
CPEX hooks the operator configured, applies any modifications CPEX returns
(redacted bodies, mutated headers, session labels), and maps the CPEX outcome
back into a pipeline action.

```text
request ─► token-exchange ─► parsers ─► cpex ─────────► upstream ─► response
                              (MCP,        │
                               inference,  │ context
                               A2A)        ▼
                                          CMF        what you evaluate
                                           │
                                           ▼
                                          APL        how you define policy
```

- **CMF** is what you evaluate: a typed view of identity, content, and metadata.
- **APL** is how you define policy: the DSL plus named CPEX plugins.
- **cpex** is where enforcement happens in the AuthBridge pipeline.

The plugin fires the CPEX hooks listed in its `hooks` block on each phase. Hooks
run in declaration order and the chain short-circuits on the first sub-plugin
that returns deny. An empty hook list makes the phase a no-op, so you can install
the plugin and ship a policy update later.

### Build tag

The CPEX backend uses cgo and links `libcpex_ffi.a`. Only the `cortex-cpex`
binary compiles this package with `-tags cpex`; the `cortex`,
`cortex-envoy`, and `authbridge-lite` binaries do not import it. Configuring
the `cpex` plugin in any other binary fails at boot with a clear "build the
cpex binary" error rather than registering a silent no-op. The package itself
compiles tag-free for unit tests via a fake manager, so
`go test ./core/plugins/cpex/...` runs with `CGO_ENABLED=0`.

## CPEX hooks

AuthBridge dispatches operator-selected subsets of these hooks on each phase. The
constants live in `hooks.go`.

| Hook | Fires | APL home |
|---|---|---|
| `cmf.tool_pre_invoke` | before an MCP tool call is forwarded | tool-args validation, PDP checks, delegation |
| `cmf.tool_post_invoke` | after a tool call returns, before it reaches the agent | post-result checks, audit |
| `cmf.llm_input` | on an LLM request before it leaves the pod | prompt PII redaction |
| `cmf.llm_output` | on an LLM response before it reaches the agent | output redaction, audit |

CPEX also runs its own `identity.resolve` hook ahead of the entity hooks when
identity resolvers are wired. AuthBridge calls the fused resolve-then-invoke path
so resolved credentials reach delegation in CPEX memory.

## CPEX plugins

APL routes pull from a toolbox of named CPEX sub-plugins declared in the CPEX
YAML. Routes invoke them by name through APL steps: `run(pii-scan)`,
`delegate(workday-oauth, ...)`, `cedar: { ... }`. See the HR demo for examples.

## CPEX APL

APL is a per-entity policy that orchestrates ABAC and ReBAC predicates, ordered effects, and
sequencing, over the common CMF vocabulary. A route binds a policy to one entity
(a tool, a prompt, a model, an A2A method). From the HR demo:

```yaml
routes:
  - tool: get_compensation
    authentication:
      - jwt-user                            # identity plugins that resolve the caller
      - jwt-client
    authorization:
      pre_invocation:
        - "require(role.hr)"
        - "delegate(workday-oauth, target: workday-api, audience: workday-api, permissions: [read_compensation])"
        - "taint(secret, session)"          # session label persists across calls
        - "run(audit-log)"
    args:
      ssn: "str | redact(!perm.view_ssn)"   # redact request arg when caller lacks the perm
    result:
      ssn: "str | redact(!perm.view_ssn)"   # redact the response field too

  - tool: send_email
    authentication:
      - jwt-user
      - jwt-client
    authorization:
      pre_invocation:
        - "require(perm.email_send)"
        - "run(pii-scan)"
        - "security.labels contains \"secret\": deny('external email blocked', 'session_tainted_secret')"
        - "run(audit-log)"
```

`get_compensation` taints the session with `secret`. The label persists in the
CPEX session store keyed by the session id, threaded from `X-Session-Id`. A later
`send_email` in the same session reads that label and denies, blocking the
exfiltration at the session level rather than with a per-call filter.

### External PDPs (BYOP)

APL steps can call out to external policy decision points and act on their
verdict with the same effect vocabulary:

```yaml
pre_invocation:
  - require(authenticated)
  - opa("http://opa:8181/v1/data/hr/compensation/deny"):
      on_deny:
        - deny
        - taint(compensation_violation, session)
        - run(audit-log)
  - cedar:
      action: "Jans::Action::Read"
      resource_type: "Jans::CompensationRecord"
      on_deny: [deny, run(compliance_alert)]
  - authzen("https://authz.corp.com/access/v1/evaluation"):
      on_deny: [deny]
```

Local structural checks run fast and first; external PDPs run when the local
checks pass; their decisions feed the same `on_deny` / `on_allow` effect lists.

> **OPA in two places.** AuthBridge ships a native `opa` pipeline plugin
> (standalone, no CPEX dependency) for simple per-request OPA checks.
> The CPEX `opa(...)` step shown above runs OPA as a CPEX sub-plugin —
> within APL's effect framework, so its verdict feeds `on_deny`/`on_allow`
> effects, session tainting, and ordered composition with other PDPs.
> Use the native `opa` plugin for standalone binary OPA gates; use
> CPEX's `opa(...)` when OPA participates in a multi-PDP orchestration
> flow. Note: the CPEX `opa(...)` and `authzen(...)` steps above are
> doc-level BYOP examples — the hr-cpex demo exercises only the embedded
> `cedar` PDP, not external OPA/AuthZEN.

## CMF / extension mapping

The AuthBridge pipeline context is mapped into the CMF Message plus
typed extensions that CPEX policies read.

| AuthBridge source | CPEX CMF / extension target | Notes |
|---|---|---|
| MCP `tools/call`, `prompts/get`, `resources/read`, result | `ContentPart` tool_call / prompt_request / resource_ref / tool_result | structured; rewritable on write-back |
| MCP tool / prompt / resource name | `MetaExtension.{EntityType, EntityName}` | drives per-route dispatch |
| Inference request messages | CMF text parts + `AgentExtension.Conversation.History` | drives `cmf.llm_input` |
| Inference request tool output (Anthropic `tool_result` blocks, OpenAI chat `tool` messages) | CMF tool_result parts + `Extensions.Custom["tool_results"]`, `["tool_result_count"]` | read-only; APL and CEL on `llm:` routes read the custom keys (see below) |
| Inference model | `MetaExtension.{EntityType: llm, EntityName}` + `LLMExtension.{ModelID, Provider}` | `llm:` routes; provider derived from model id |
| Inference finish reason + token usage | `CompletionExtension.{StopReason, Tokens, Model}` | response phase |
| A2A text parts | CMF text parts | role from phase |
| A2A, MCP protocol methods (`tools/list`, `initialize`), unparsed traffic (with `synthetic_egress`, inbound only) | `MetaExtension.{EntityType: http, EntityName: *}` | CPEX's global entity; `fallback_identification: none` sends no meta, which CPEX denies as `unidentified_request` |
| A2A session / task / message id | `AgentExtension.{SessionID, ConversationID}` + `ProvenanceExtension.MessageID` | |
| Identity subject + scopes | `SecurityExtension.Subject.{ID, Roles}` | always mapped |
| Identity auth method + curated claims | `SecurityExtension.AuthMethod`, `Subject.Claims` | issuer / audience / exp, not the raw claim map |
| Agent workload identity | `SecurityExtension.ThisWorkload.{SpiffeID, TrustDomain, ClientID}` | the agent's own identity, distinct from the caller |
| Delegation chain | `DelegationExtension.{Chain, Depth, OriginSubjectID, ActorSubjectID}` | populated by token exchange |
| Request id / trace headers | `RequestExtension.{RequestID, TraceID, SpanID}` | from `X-Request-Id` / B3 / traceparent |
| Session id | `AgentExtension.SessionID` | from A2A natively or `X-Session-Id`; with `session_source: resolved`, the listener's session when an agent session header or the process chain named it |
| HTTP headers | `HttpExtension.RequestHeaders` | Authorization and Cookie stripped |
| `pctx.Extensions.Custom["cpex/<key>"]` | `Extensions.Custom["<key>"]` | only the `cpex/`-prefixed entries cross |

Tool-result content parts are not visible to APL or CEL on an `llm:` route, so
an inference request also carries its tool results in the custom extension:
`custom.tool_result_count` (always set) and `custom.tool_results`, an object
keyed by position (`"0"`, `"1"`, ...) because the attribute bag cannot hold a
list of objects. Each entry has `tool_use_id` (empty for OpenAI chat),
`content` (the first 8 KiB), `truncated` and `is_error`. They are policy input
only, never recorded, and no other plugin can supply them. Guard on the count,
since `custom.tool_results` is absent when there are none:

```yaml
- cel:
    expr: "custom.tool_result_count == 0 || !custom.tool_results.exists(k, custom.tool_results[k].content.matches('AKIA[0-9A-Z]{16}'))"
    on_deny: ["taint(secret, session)", "deny('secret in a tool result', 'secret_in_tool_result')"]
```

A CEL step allows when its expression is true and denies when it is false, so
this step taints the session and denies the inference request together; a
later `http_egress` route can then deny on `security.labels contains "secret"`.

Caller identity lives in `SecurityExtension.Subject`. Policies read it from
`security.subject`, not `agent.agent_id`. The `agent` slot models the agent's own
execution identity.

CMF extension slots are visible to all plugins (unrestricted) or require a
declared capability (capability-gated). `http`, `security`, `agent`, and
`delegation` are gated; a plugin must hold `read_headers`, `read_agent`,
`read_delegation`, and so on to see them. Sensitive headers are stripped at the
AuthBridge boundary before the slot is built, so a misconfigured grant or the
unauthenticated session API cannot leak cookies or api-keys.

## Pipeline configuration

Place `cpex` after the protocol parsers and any content filters on the phase you
want to enforce. The parsers populate `pctx.Extensions`; cpex reads from there to
build CMF. The plugin declares `RequiresAny: [mcp-parser, inference-parser,
a2a-parser]`, so `pipeline.Build` fails fast if no parser feeds it.

The HR demo enforces on the agent's egress (the forward proxy), with inbound left
as passthrough:

```text
outbound:  mcp-parser ──► cpex ──► (token exchange happens as part of the policy via delegate)
```

```yaml
pipeline:
  inbound:
    plugins: []                 # passthrough; forward identity headers to the agent
  outbound:
    plugins:
      - name: mcp-parser        # populates pctx.Extensions.MCP
      - name: cpex
        config:
          hooks:
            on_request:  [cmf.tool_pre_invoke]
            on_response: [cmf.tool_post_invoke]
          config_file: /etc/cpex/cpex.yaml
          fail_open: false
```

Identity resolution, policy, delegation, redaction, PII scanning, and session
taint all run inside cpex via its own sub-plugins, so the AuthBridge-side
`jwt-validation` and `token-exchange` plugins are not needed on this path.

### Plugin config

| Field | Default | Purpose |
|---|---|---|
| `hooks.on_request` | empty | CPEX hooks fired during OnRequest, in order |
| `hooks.on_response` | empty | CPEX hooks fired during OnResponse, in order |
| `config` | empty | CPEX runtime YAML inline; mutually exclusive with `config_file` |
| `config_file` | empty | path to CPEX YAML; mounted from a ConfigMap in production |
| `fail_open` | `false` | on a CPEX internal error: deny (false) or continue (true). The rejection renders per the listener — MCP error frame for MCP requests, else HTTP 502 (see Decisions and denials) |
| `worker_threads` | `0` | CPEX tokio worker pool size; 0 lets CPEX pick |
| `bypass_paths` | health + `.well-known` | URL path globs that skip CPEX entirely |
| `bypass_hosts` | keycloak / SPIRE / observability | host globs that skip CPEX, outbound only |
| `fallback_identification` | `global` | identification for requests naming no tool, prompt, resource or model: `global` (CPEX's `http`/`*` entity) or `none` |
| `session_source` | `header` | session id for CPEX session state: `header` (`X-Session-Id` or the A2A context id) or `resolved` (the listener's session, only when a dedicated agent session header or the process chain named it) |
| `private_destinations` | `false` | let CPEX builtins (JWKS, token exchange, CIBA, Vault) reach loopback, private and link-local addresses |
| `synthetic_egress` | `false` | present requests no parser claimed as calls to the tool `http_egress` (see below) |
| `principal.token_file` | empty | file holding a caller JWT that Cortex asserts on outbound tool calls and inference requests; empty turns it off |
| `principal.header` | `x-user-token` | identity header the token is presented under; must not be `Authorization` |

A `cpex` plugin with no hooks, no config, and no config file is installed but
inert. Configure succeeds and both phases return continue. `config` and
`config_file` carry the CPEX YAML verbatim, exactly what CPEX's own docs
describe, with no AuthBridge-side reshaping. Unknown config keys are rejected at
boot.

`bypass_hosts` is honored on the outbound phase only. On the inbound
reverse-proxy phase the Host header is attacker-controlled and identity is
resolved inside the bypassed chain, so a spoofed Host must not skip CPEX.
`bypass_paths` applies on both phases, except that with `synthetic_egress` it
applies inbound only: the outbound path is the caller's choice for any host. For
the same reason `synthetic_egress` drops the default `bypass_hosts`, whose
wildcards a caller can match with a host of its own (`keycloak.evil.example`);
an explicit list is kept. Neither list accepts `*` or empty; to disable the
plugin, remove it from the pipeline.

A request a parser claimed is denied before CPEX sees it when its JSON body has
an object with a repeated key or two keys equal under case folding
(`cpex.ambiguous_request`): the parsers match keys case-insensitively and keep
the last repeat, a server may not, and policy would judge a different request
from the one the server runs. Inside tool arguments and schemas only exact
repeats count. An MCP `tools/call` or `prompts/get` with no name, or a
`resources/read` with no URI, is denied as `cpex.unnamed_entity` rather than
taking the global identification. Neither denial is subject to `fail_open`.

### Plain egress as a tool call

With `synthetic_egress: true`, an outbound request that names no tool, prompt,
resource or model (plaintext HTTP or a bridged HTTPS request) is presented to
CPEX as a call to the tool `http_egress`, on the request phase. That includes
bodies a parser claimed as an MCP protocol method (`tools/list`, `initialize`)
or an A2A message, so shaping a body as one does not route around the egress
policy; a changed `http_egress` payload has no re-serializer and is an error
that `fail_open` governs. Inbound requests are never egress. Its arguments:

| Argument | Value |
|---|---|
| `method` | upper-case HTTP method |
| `host` | lower-case host, port stripped |
| `path` | URL path, never the query |
| `body` | the first 8 KiB of the body, as UTF-8 |
| `truncated` | whether the body was longer than the excerpt |
| `content_encoding` | the lower-case `Content-Encoding`, empty when absent |

An opaque `CONNECT` tunnel is presented with its method and host, an empty path
and no body, so a host allowlist governs tunnels too. On a `tool:` route CEL
reads the arguments as `args.<name>`:

```yaml
global:
  pdp:
    - kind: cel
routes:
  - tool: http_egress
    authorization:
      pre_invocation:
        - cel:
            expr: "args.host in allowed"
            allowed: ["support.local", "github.com"]
            on_deny: ["deny('host not allowed', 'egress_host')"]
```

The excerpt is policy input only. The invocation records its size and whether
it was cut (`egress_excerpt_bytes`, `egress_truncated`), never its content. A
policy that inspects bodies should deny when `truncated` is true or
`content_encoding` is set, since it cannot see the rest.

### Cortex-asserted principal

`principal.token_file` lets Cortex assert who the caller is, for an agent that
must not hold the credential. For requests presented as tool calls (MCP tools
and `http_egress`) and for inference requests, the token is added under
`principal.header` to the headers CPEX identity resolution reads, so an
`identity/jwt` plugin configured with that `header:` resolves the subject, and a
`taint` on an `llm:` route has one. Only outbound requests get it: an inbound
caller is never asserted as the principal. The token is never written to the
forwarded request, and a caller's own copy of the header reaches neither
identity resolution nor the `http` extension. When the file cannot be read,
Cortex logs a warning at startup and denies those requests with
`cpex.principal_unavailable` until it can.

## Decisions and denials

The plugin maps the CPEX outcome onto AuthBridge's five-value invocation
vocabulary:

- **allow**: all sub-plugins continued and the forwarded message is unchanged.
  A result that only adds labels (`taint`) or runs a `delegate` step is an
  allow, as is one returning the headers and payload as sent, which CPEX 0.3.0
  does on every allowed call.
- **deny**: a sub-plugin returned a policy violation with a `cpex.<code>` code
  (`cpex.pii_detected`, `cpex.session_tainted_secret`, and so on). The plugin
  emits a `Reject`; how it appears on the wire is the listener's call, not the
  plugin's (see below). The response and the invocation carry only that code
  and the message `request denied by policy`: the policy's own reason can quote
  the request, so it goes to the debug log.
- **modify**: policy changed the forwarded body or headers. Changes are
  applied, the request continues. A body rewrite of an inference request is
  supported on OpenAI chat requests with a `messages` array only, and of an
  inference response on a single non-streaming completion's text only; one on
  any other dialect or shape, a changed tool result or tool call, or a removed
  part is an error that `fail_open` governs. Request headers are written back as a diff against
  those sent to CPEX, so an assertion can strip or replace a header. Headers
  never sent (Cookie, API keys), the hop-by-hop and framing headers (`host`,
  `content-length`, `transfer-encoding`, `connection`, `keep-alive`, `te`,
  `trailer`, `upgrade`), `proxy-*`, `x-authbridge-*` and `principal.header`
  are never written; such writes are dropped and logged by name. A returned
  header name or value that is invalid on the wire (a CR or LF, say) is an
  error that `fail_open` governs, and nothing is written.
- **observe**: audit-only outcome, recorded, request continues.

**How a rejection is rendered.** The plugin sets the decision and a status
(`403` for a policy deny, `502` for a CPEX-internal error); the HTTP/proxy
listeners then choose the wire shape (`core/listener/httpx/render.go`):

- **Classified MCP request** (an MCP parser populated the MCP extension with a
  JSON-RPC `method` and `id`) → an **MCP JSON-RPC 2.0 error frame at HTTP 200**,
  with the original id echoed back:
  `{"jsonrpc":"2.0","id":<id>,"error":{"code":-32000,"message":<reason>,"data":{"error":"cpex.<code>","plugin":"cpex"}}}`.
  The caller's MCP client surfaces this as a failed tool call rather than a
  transport break. This covers both a policy deny and a fail-closed internal
  error — they differ in the frame's `message` / `data.error`, not the HTTP
  status.
- **Everything else** (non-MCP requests, or JSON-RPC notifications with no id) →
  a plain HTTP response carrying the status the plugin set: **403 Forbidden** for
  a policy deny, **502 Bad Gateway** for a CPEX-internal error, with a
  `{"error":"cpex.<code>",...}` body.

(The gRPC listener — extproc — doesn't use this renderer and surfaces
denials per its own protocol.)

A CPEX policy `deny` is a normal outcome and is always honored. `fail_open` only
governs CPEX-internal failures (an FFI error, an unreachable backend, a
modification that cannot be applied): with `fail_open: false` those are rejected
(rendered as above — an MCP error frame for MCP requests, otherwise HTTP 502);
with `true` they log and continue. The default is fail-closed because a CPEX
error usually means a misconfigured policy or an unreachable PDP, and silently
allowing traffic in that state is rarely intended.

## Building and testing

```sh
# Unit tests (no cgo) cover every CMF mapping and re-serializer via a fake manager
CGO_ENABLED=0 go -C core test ./plugins/cpex/...

# Build the real backend: links libcpex_ffi.a, -tags cpex, CGO on
podman build -f cmd/cortex-cpex/Dockerfile -t authbridge-cpex:latest .
```

The pinned CPEX FFI ABI version lives in `cmd/cortex-cpex/CPEX_FFI_VERSION`.

## See also

- `demos/hr-cpex` — end-to-end demo with Bob, Eve, and Alice personas
  covering the Workday flow, GitHub flow with Cedar, PII scanning, and session
  taint propagation.
