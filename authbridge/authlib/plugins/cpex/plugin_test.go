//go:build cpex

package cpex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// fakeManager is the test-double Manager. Tests inject it via
// SetManagerFactory or by assigning p.manager directly. The fake
// records every call so assertions can verify lifecycle ordering
// (EnableAPL → LoadConfig → Initialize → InvokeByName → Shutdown)
// without an actual CPEX library on disk.
type fakeManager struct {
	enableAPLCalled  bool
	loadConfigCalled bool
	loadConfigYAML   []byte
	initialized      bool
	shutdown         bool

	// invokeResponses is a queue: the i-th call to InvokeByName
	// returns invokeResponses[i] (saturating at the last entry).
	invokeResponses []*Result
	invokeErr       error

	// invokeCalls accumulates the (hook, request) pairs.
	invokeCalls []invokeCall
}

type invokeCall struct {
	hook    string
	request *Request
}

func (m *fakeManager) EnableAPL() error {
	m.enableAPLCalled = true
	return nil
}

func (m *fakeManager) LoadConfig(_ context.Context, yaml []byte) error {
	m.loadConfigCalled = true
	m.loadConfigYAML = append([]byte(nil), yaml...)
	return nil
}

func (m *fakeManager) Initialize(_ context.Context) error {
	m.initialized = true
	return nil
}

func (m *fakeManager) InvokeByName(_ context.Context, hook string, req *Request) (*Result, error) {
	m.invokeCalls = append(m.invokeCalls, invokeCall{hook: hook, request: req})
	if m.invokeErr != nil {
		return nil, m.invokeErr
	}
	if len(m.invokeResponses) == 0 {
		return &Result{}, nil
	}
	idx := len(m.invokeCalls) - 1
	if idx >= len(m.invokeResponses) {
		idx = len(m.invokeResponses) - 1
	}
	return m.invokeResponses[idx], nil
}

func (m *fakeManager) Shutdown(_ context.Context) error {
	m.shutdown = true
	return nil
}

// withFactory installs a fakeManager via the package factory and
// returns a cleanup function that restores the previous factory.
func withFactory(t *testing.T, fm *fakeManager) func() {
	t.Helper()
	prev := managerFactory
	SetManagerFactory(func() (Manager, error) { return fm, nil })
	return func() { managerFactory = prev }
}

// configure builds a configured plugin against fm using the given
// config JSON.
func configure(t *testing.T, fm *fakeManager, cfg string) *CPEX {
	t.Helper()
	cleanup := withFactory(t, fm)
	t.Cleanup(cleanup)
	p := NewCPEX()
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

// makeRequestPctx builds an outbound pctx with an action-classified
// MCP extension so the runHooks path is exercised end-to-end. Tests
// override fields by mutating the returned pctx.
func makeRequestPctx(t *testing.T) *pipeline.Context {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    "POST",
		Scheme:    "http",
		Host:      "weather-tool.team1.svc",
		Path:      "/api/weather",
		Headers:   http.Header{},
		Body:      []byte(`{"city":"sf"}`),
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/call", IsAction: true}
	return pctx
}

func invokeOnRequest(p *CPEX, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

// --- Configure ---

func TestConfigure_DefaultsAndLifecycle(t *testing.T) {
	fm := &fakeManager{}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input"]}}`)

	if !fm.enableAPLCalled {
		t.Errorf("EnableAPL was not called during Configure")
	}
	// No apl/pipelines blocks → LoadConfig should be skipped.
	if fm.loadConfigCalled {
		t.Errorf("LoadConfig should be skipped when no apl/pipelines blocks present")
	}
	if len(p.cfg.BypassHosts) == 0 {
		t.Errorf("expected default BypassHosts populated")
	}
	if len(p.cfg.BypassPaths) == 0 {
		t.Errorf("expected default BypassPaths populated")
	}
}

func TestConfigure_ForwardsAPLAndPipelinesAsYAML(t *testing.T) {
	fm := &fakeManager{}
	cfg := `{
		"hooks": {"on_request":["on-input"]},
		"apl": {"identity":{"kind":"identity/jwt"}},
		"pipelines": [{"hook":"on-input","steps":[{"kind":"validator/pii-scan"}]}]
	}`
	configure(t, fm, cfg)

	if !fm.loadConfigCalled {
		t.Fatalf("LoadConfig should be called when apl/pipelines blocks are present")
	}
	got := string(fm.loadConfigYAML)
	if !strings.Contains(got, "apl:") {
		t.Errorf("LoadConfig YAML missing apl block: %q", got)
	}
	if !strings.Contains(got, "pipelines:") {
		t.Errorf("LoadConfig YAML missing pipelines block: %q", got)
	}
	if !strings.Contains(got, "validator/pii-scan") {
		t.Errorf("LoadConfig YAML missing pipeline step: %q", got)
	}
}

func TestConfigure_RejectsUnknownTopLevelField(t *testing.T) {
	fm := &fakeManager{}
	cleanup := withFactory(t, fm)
	t.Cleanup(cleanup)
	p := NewCPEX()
	err := p.Configure(json.RawMessage(`{"unknown":"x"}`))
	if err == nil {
		t.Fatalf("expected error for unknown top-level field")
	}
}

func TestConfigure_NoFactoryInstalledFailsLoud(t *testing.T) {
	prev := managerFactory
	defer func() { managerFactory = prev }()
	managerFactory = func() (Manager, error) { return nil, errNoManagerFactory }
	p := NewCPEX()
	err := p.Configure(json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no manager factory") {
		t.Fatalf("expected no-manager-factory error; got %v", err)
	}
}

func TestConfigure_WildcardBypassRejected(t *testing.T) {
	fm := &fakeManager{}
	cleanup := withFactory(t, fm)
	t.Cleanup(cleanup)
	p := NewCPEX()
	err := p.Configure(json.RawMessage(`{"bypass_hosts":["*"]}`))
	if err == nil || !strings.Contains(err.Error(), "matches everything") {
		t.Fatalf("expected wildcard bypass rejection; got %v", err)
	}
}

// --- Lifecycle ---

func TestInit_CallsManagerInitialize(t *testing.T) {
	fm := &fakeManager{}
	p := configure(t, fm, `{}`)
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !fm.initialized {
		t.Errorf("manager.Initialize was not called")
	}
}

func TestShutdown_CallsManagerShutdown(t *testing.T) {
	fm := &fakeManager{}
	p := configure(t, fm, `{}`)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !fm.shutdown {
		t.Errorf("manager.Shutdown was not called")
	}
}

// --- OnRequest ---

func TestOnRequest_NoHooksConfigured_PassesThrough(t *testing.T) {
	fm := &fakeManager{}
	p := configure(t, fm, `{}`)
	pctx := makeRequestPctx(t)
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue (no hooks → no-op)", action.Type)
	}
	if len(fm.invokeCalls) != 0 {
		t.Errorf("InvokeByName was called %d times; expected 0", len(fm.invokeCalls))
	}
}

func TestOnRequest_BypassPath(t *testing.T) {
	fm := &fakeManager{}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input"]}}`)
	pctx := makeRequestPctx(t)
	pctx.Path = "/healthz"
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue on bypass path", action.Type)
	}
	if len(fm.invokeCalls) != 0 {
		t.Errorf("InvokeByName called on bypass path; want 0 invocations")
	}
}

func TestOnRequest_BypassHost(t *testing.T) {
	fm := &fakeManager{}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input"]}}`)
	pctx := makeRequestPctx(t)
	pctx.Host = "keycloak.kagenti"
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Errorf("got %v, want Continue for default-bypass host", action.Type)
	}
	if len(fm.invokeCalls) != 0 {
		t.Errorf("InvokeByName called on bypass host; want 0 invocations")
	}
}

func TestOnRequest_RunsConfiguredHooksInOrder(t *testing.T) {
	fm := &fakeManager{
		invokeResponses: []*Result{
			{SubPlugins: []SubPluginOutcome{{Name: "validator/pii-scan", Action: SubObserve, Reason: "no_match"}}},
			{SubPlugins: []SubPluginOutcome{{Name: "pdp/cedar-direct", Action: SubAllow, Reason: "permitted"}}},
		},
	}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input","tool-pre-invoke"]}}`)
	pctx := makeRequestPctx(t)
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("got %v, want Continue (no sub-plugin denied)", action.Type)
	}
	if len(fm.invokeCalls) != 2 {
		t.Fatalf("InvokeByName called %d times; want 2", len(fm.invokeCalls))
	}
	if fm.invokeCalls[0].hook != "on-input" || fm.invokeCalls[1].hook != "tool-pre-invoke" {
		t.Errorf("hook order wrong: %v", fm.invokeCalls)
	}
	// Two sub-plugins → two invocations on the outbound bucket
	// (pctx.Direction == Outbound for makeRequestPctx).
	invs := pctx.Extensions.Invocations
	if invs == nil || len(invs.Outbound) != 2 {
		t.Fatalf("expected 2 outbound invocations; got %+v", invs)
	}
	if invs.Outbound[0].Plugin != "cpex/validator/pii-scan" {
		t.Errorf("first invocation plugin = %q, want cpex/validator/pii-scan", invs.Outbound[0].Plugin)
	}
	if invs.Outbound[1].Plugin != "cpex/pdp/cedar-direct" {
		t.Errorf("second invocation plugin = %q, want cpex/pdp/cedar-direct", invs.Outbound[1].Plugin)
	}
}

func TestOnRequest_DenyShortCircuits(t *testing.T) {
	fm := &fakeManager{
		invokeResponses: []*Result{
			{SubPlugins: []SubPluginOutcome{
				{Name: "pdp/cedar-direct", Action: SubDeny, Reason: "scope_missing", Message: "missing weather_read"},
			}},
		},
	}
	p := configure(t, fm, `{"hooks":{"on_request":["tool-pre-invoke","on-input"]}}`)
	pctx := makeRequestPctx(t)
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject on sub-plugin deny", action.Type)
	}
	if action.Violation == nil || action.Violation.Status != 403 {
		t.Errorf("expected 403 deny; got violation=%+v", action.Violation)
	}
	if len(fm.invokeCalls) != 1 {
		t.Errorf("expected 1 hook call (short-circuit); got %d", len(fm.invokeCalls))
	}
	if !strings.Contains(action.Violation.Code, "scope_missing") {
		t.Errorf("violation code missing reason: %q", action.Violation.Code)
	}
}

func TestOnRequest_TransportErrorFailClosed(t *testing.T) {
	fm := &fakeManager{invokeErr: errors.New("ffi panic")}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input"]}}`)
	pctx := makeRequestPctx(t)
	action := invokeOnRequest(p, pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("got %v, want Reject on manager error", action.Type)
	}
	if action.Violation.Status != 503 {
		t.Errorf("got status %d, want 503", action.Violation.Status)
	}
}

func TestOnRequest_BodyMutationRoutedThroughPctx(t *testing.T) {
	fm := &fakeManager{
		invokeResponses: []*Result{
			{
				SubPlugins:   []SubPluginOutcome{{Name: "validator/pii-scan", Action: SubModify, Reason: "redacted"}},
				ModifiedBody: []byte(`{"city":"REDACTED"}`),
			},
		},
	}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input"]}}`)
	pctx := makeRequestPctx(t)
	_ = invokeOnRequest(p, pctx)
	if !pctx.BodyMutated() {
		t.Errorf("expected pctx.BodyMutated() true after CPEX modify")
	}
	if string(pctx.Body) != `{"city":"REDACTED"}` {
		t.Errorf("pctx.Body not updated; got %q", pctx.Body)
	}
}

func TestOnRequest_HeaderMutationsApplied(t *testing.T) {
	fm := &fakeManager{
		invokeResponses: []*Result{
			{
				SubPlugins: []SubPluginOutcome{{Name: "delegator/oauth", Action: SubModify, Reason: "token_minted"}},
				ModifiedHeaders: map[string][]string{
					"X-Cpex-Test": {"applied"},
				},
			},
		},
	}
	p := configure(t, fm, `{"hooks":{"on_request":["tool-pre-invoke"]}}`)
	pctx := makeRequestPctx(t)
	_ = invokeOnRequest(p, pctx)
	if got := pctx.Headers.Get("X-Cpex-Test"); got != "applied" {
		t.Errorf("X-Cpex-Test header = %q, want %q", got, "applied")
	}
}

func TestOnRequest_LabelsMergedIntoCustom(t *testing.T) {
	fm := &fakeManager{
		invokeResponses: []*Result{
			{
				SubPlugins:  []SubPluginOutcome{{Name: "validator/pii-scan", Action: SubObserve, Reason: "tagged"}},
				AddedLabels: []string{"contains_pii"},
			},
		},
	}
	p := configure(t, fm, `{"hooks":{"on_request":["on-input"]}}`)
	pctx := makeRequestPctx(t)
	_ = invokeOnRequest(p, pctx)
	v, ok := pctx.Extensions.Custom["cpex/labels"].([]string)
	if !ok {
		t.Fatalf("cpex/labels missing or wrong type: %T", pctx.Extensions.Custom["cpex/labels"])
	}
	if len(v) != 1 || v[0] != "contains_pii" {
		t.Errorf("unexpected labels: %v", v)
	}
}

// --- CMF translation ---

func TestBuildRequest_StripsSecretHeaders(t *testing.T) {
	pctx := makeRequestPctx(t)
	pctx.Headers.Set("Authorization", "Bearer SECRET-VALUE")
	pctx.Headers.Set("Cookie", "session=SECRET")
	pctx.Headers.Set("X-Api-Key", "ALSO-SECRET")
	pctx.Headers.Set("Content-Type", "application/json")
	req := buildRequest(pctx)
	if req.SafeHeaders == nil {
		t.Fatalf("expected SafeHeaders populated with safe keys")
	}
	for k := range req.SafeHeaders {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "authorization") || strings.HasPrefix(lower, "cookie") || lower == "x-api-key" {
			t.Errorf("secret header %q leaked into SafeHeaders", k)
		}
	}
	if req.SafeHeaders["Content-Type"] == nil {
		t.Errorf("safe headers should retain Content-Type")
	}
}

func TestBuildRequest_ForwardsCustomCpexNamespace(t *testing.T) {
	pctx := makeRequestPctx(t)
	pctx.Extensions.Custom = map[string]any{
		"cpex/whatever":      "yes",
		"unrelated-plugin":   "no",
		"rate-limiter/event": "no",
	}
	req := buildRequest(pctx)
	if got := req.Custom["whatever"]; got != "yes" {
		t.Errorf("cpex/whatever did not pass through; got %v", got)
	}
	if _, ok := req.Custom["unrelated-plugin"]; ok {
		t.Errorf("non-cpex Custom key leaked: %v", req.Custom)
	}
}

func TestTranslateAction_UnknownActionFallsToObserve(t *testing.T) {
	got := translateAction(SubAction("invented"))
	if got != pipeline.ActionObserve {
		t.Errorf("translateAction(unknown) = %v, want observe", got)
	}
}
