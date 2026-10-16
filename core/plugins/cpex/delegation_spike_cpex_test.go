// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/vmihailenco/msgpack/v5"
)

// The delegation tests below drive the CPEX engine the way the cpex
// plugin does (InvokeResolved, loopback IdP, private destinations on)
// and pin down what a Go host can see after a granted delegate() step.
// Unless the host asks for delegated credentials (CPEX 0.3.1's
// WithDelegatedCredentials, which the plugin does not use), the minted
// token stays in Rust memory: the result
// carries its metadata in raw_credentials.delegated_tokens, never its
// value, which is why the plugin denies with cpex.delegation_unavailable.

const (
	spikeIssuer   = "https://idp.spike.test"
	spikeAudience = "cortex-spike"
	spikeMinted   = "spike-only-minted-workday-token"
)

var spikeSecret = []byte("cortex-spike-only-hs256-secret-0123456789")

type spikeIdP struct {
	srv       *httptest.Server
	mu        sync.Mutex
	exchanges []url.Values
}

// newSpikeIdP serves an RFC 8693 token endpoint granting audience
// workday-api and refusing every other audience.
func newSpikeIdP(t *testing.T) *spikeIdP {
	t.Helper()
	m := &spikeIdP{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.exchanges = append(m.exchanges, r.PostForm)
		m.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		if r.PostForm.Get("audience") != "workday-api" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_target"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      spikeMinted,
			"token_type":        "Bearer",
			"expires_in":        300,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *spikeIdP) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.exchanges)
}

func spikeJWT(sub string, roles []string) string {
	b64 := base64.RawURLEncoding.EncodeToString
	now := time.Now().Unix()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{
		"iss": spikeIssuer, "aud": spikeAudience, "sub": sub,
		"iat": now - 5, "exp": now + 600, "roles": roles,
	})
	signing := b64(hdr) + "." + b64(body)
	mac := hmac.New(sha256.New, spikeSecret)
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

func spikePolicy(tokenURL string) string {
	jwk, _ := json.Marshal(map[string]string{"kty": "oct", "alg": "HS256", "k": base64.RawURLEncoding.EncodeToString(spikeSecret)})
	return `
plugins:
  - name: idp
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      claim_mapper: standard
      trusted_issuers:
        - issuer: ` + spikeIssuer + `
          audiences: [` + spikeAudience + `]
          algorithms: [HS256]
          decoding_key: {kind: jwk, jwk: ` + string(jwk) + `}
  - name: workday-oauth
    kind: delegator/oauth
    hooks: [token.delegate]
    capabilities: [read_inbound_credentials, write_delegated_tokens, perform_http]
    config:
      token_endpoint: "` + tokenURL + `"
      client_id: cortex
      client_secret_source: {kind: literal, secret: spike-only}
      insecure_http: true
routes:
  - tool: get_compensation
    authentication: [idp]
    authorization:
      pre_invocation:
        - "require(role.hr)"
        - "delegate(workday-oauth, target: workday-api, audience: workday-api)"
        - "require(delegation.granted)"
`
}

func spikeEngine(t *testing.T, policy string) *rcpex.PolicyEngine {
	t.Helper()
	eng, err := rcpex.NewPolicyEngineDefault()
	if err != nil {
		t.Fatalf("NewPolicyEngineDefault: %v", err)
	}
	t.Cleanup(eng.Shutdown)
	if err := eng.EnableAPL(rcpex.WithPrivateDestinations()); err != nil {
		t.Fatalf("EnableAPL: %v", err)
	}
	if err := eng.LoadConfig(policy); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := eng.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return eng
}

func spikeCall(t *testing.T, eng *rcpex.PolicyEngine, bearer, tool string) *rcpex.PipelineResult {
	t.Helper()
	idp := rcpex.NewIdentityPayload(rcpex.TokenSourceBearer, map[string]string{"authorization": "Bearer " + bearer})
	ext := &rcpex.Extensions{Meta: &rcpex.MetaExtension{EntityType: "tool", EntityName: tool}}
	payload := rcpex.MessagePayload{Message: rcpex.NewMessage("assistant", rcpex.NewToolCallPart(rcpex.ToolCall{
		ToolCallID: "spike-1", Name: tool, Arguments: map[string]any{"employee_id": "e-1"},
	}))}
	res, ct, bg, err := eng.InvokeResolved(idp, "cmf.tool_pre_invoke", rcpex.PayloadCMFMessage, payload, ext, nil)
	if err != nil {
		t.Fatalf("InvokeResolved(%s): %v", tool, err)
	}
	if bg != nil {
		bg.Close()
	}
	if ct != nil {
		ct.Close()
	}
	return res
}

// A granted delegation exchanges the token and reports its metadata, but
// no channel of the Go result carries the minted value.
func TestDelegationMintedTokenNotObservable(t *testing.T) {
	idp := newSpikeIdP(t)
	eng := spikeEngine(t, spikePolicy(idp.srv.URL+"/token"))

	res := spikeCall(t, eng, spikeJWT("alice", []string{"hr"}), "get_compensation")
	if res.IsDenied() {
		t.Fatalf("expected allowed, got %+v", res.Violation)
	}
	if got := idp.count(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}

	var ext struct {
		Http *struct {
			RequestHeaders map[string]string `msgpack:"request_headers"`
		} `msgpack:"http"`
		RawCredentials *struct {
			DelegatedTokens []any `msgpack:"delegated_tokens"`
		} `msgpack:"raw_credentials"`
	}
	if err := msgpack.Unmarshal(res.ModifiedExtensions, &ext); err != nil {
		t.Fatalf("decode modified extensions: %v", err)
	}
	if ext.RawCredentials == nil || len(ext.RawCredentials.DelegatedTokens) != 1 {
		t.Fatalf("expected one delegated token's metadata, got %+v", ext.RawCredentials)
	}
	meta := fmt.Sprint(ext.RawCredentials.DelegatedTokens[0])
	for _, want := range []string{"workday-api", "Authorization"} {
		if !strings.Contains(meta, want) {
			t.Errorf("delegated token metadata %s lacks %q", meta, want)
		}
	}
	if ext.Http != nil {
		for k, v := range ext.Http.RequestHeaders {
			if strings.Contains(v, spikeMinted) {
				t.Errorf("minted token surfaced in request header %q", k)
			}
		}
	}

	// Every byte channel and the printed result: none carries the value.
	needle := []byte(spikeMinted)
	for name, b := range map[string][]byte{
		"modified_extensions": res.ModifiedExtensions,
		"modified_payload":    res.ModifiedPayload,
		"printed result":      []byte(fmt.Sprintf("%+v %#v", res, res)),
	} {
		if bytes.Contains(b, needle) {
			t.Errorf("minted token found in %s", name)
		}
	}
	if meta, _ := json.Marshal(res.Metadata); bytes.Contains(meta, needle) {
		t.Error("minted token found in metadata")
	}

	// Without the value the plugin cannot forward the delegated
	// credential, so it fails closed with cpex.delegation_unavailable.
	if withheld, err := delegatedTokensWithheld(res); err != nil || !withheld {
		t.Fatalf("delegatedTokensWithheld = %v, %v; want true", withheld, err)
	}
}

// A role denial ahead of the delegate() step never reaches the IdP.
func TestDelegationDeniedBeforeExchange(t *testing.T) {
	idp := newSpikeIdP(t)
	eng := spikeEngine(t, spikePolicy(idp.srv.URL+"/token"))

	res := spikeCall(t, eng, spikeJWT("evan", []string{"engineer"}), "get_compensation")
	if !res.IsDenied() {
		t.Fatal("expected denied")
	}
	if got := idp.count(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
	if withheld, _ := delegatedTokensWithheld(res); withheld {
		t.Fatal("a denied result must not report a withheld delegation")
	}
}
