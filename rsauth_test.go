package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/oidcauth"
)

// Test credentials. Made up for these tests only.
const (
	authTestAudience = "ghapp-mcp"
	authTestClient   = "straza-agent-build"
	authTestSubject  = "service-account-build"
	authTestListing  = "listing-token-for-tests-only"
	authTestGateway  = "gateway-secret-for-tests-only"
)

// authTestIssuer is a fake identity provider on httptest. It serves the
// discovery document and the JWKS for its key, and signs tokens with
// go-jose. Its issuer is its own URL.
type authTestIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer
}

func newAuthTestIssuer(t *testing.T) *authTestIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-kid"))
	if err != nil {
		t.Fatal(err)
	}
	iss := &authTestIssuer{key: key, signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.server.URL,
			"token_endpoint":                        iss.server.URL + "/token",
			"jwks_uri":                              iss.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "test-kid", Algorithm: "RS256", Use: "sig",
		}}})
	})
	iss.server = httptest.NewServer(mux)
	t.Cleanup(iss.server.Close)
	return iss
}

// mint signs a client credentials token from a listed client, as Keycloak
// issues one, after mutate changes its claims.
func (iss *authTestIssuer) mint(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": iss.server.URL,
		"aud": authTestAudience,
		"azp": authTestClient,
		"sub": authTestSubject,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
	if mutate != nil {
		mutate(claims)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := iss.signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// authTestRecorder records each fake tool call with the caller the context
// carried.
type authTestRecorder struct {
	mu    sync.Mutex
	calls []authTestCall
}

type authTestCall struct {
	tool   string
	caller Caller
	known  bool
}

func (r *authTestRecorder) record(ctx context.Context, tool string) {
	c, ok := callerFromContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, authTestCall{tool: tool, caller: c, known: ok})
}

func (r *authTestRecorder) snapshot() []authTestCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type authTestListIn struct {
	Cursor string `json:"cursor,omitempty"`
}

type authTestListOut struct {
	Repos      []string `json:"repos"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

type authTestGetIn struct {
	Repo string `json:"repo"`
}

type authTestGetOut struct {
	Title string `json:"title"`
}

// newAuthTestServer is an MCP server with two fake tools, repo_list, which
// answers two pages, and pr_get. Neither reaches GitHub.
func newAuthTestServer(rec *authTestRecorder) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "repo_list", Description: "fake"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in authTestListIn) (*mcp.CallToolResult, authTestListOut, error) {
			rec.record(ctx, "repo_list")
			if in.Cursor == "" {
				return nil, authTestListOut{Repos: []string{"Strazahq/pay-api"}, NextCursor: "page-2"}, nil
			}
			return nil, authTestListOut{Repos: []string{"Strazahq/web"}}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "pr_get", Description: "fake"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in authTestGetIn) (*mcp.CallToolResult, authTestGetOut, error) {
			rec.record(ctx, "pr_get")
			return nil, authTestGetOut{Title: "fake pull request"}, nil
		})
	return server
}

// authTestConfig turns on every kind of caller authentication.
func authTestConfig(iss *authTestIssuer) Config {
	return Config{
		OIDCIssuer:      iss.server.URL,
		OIDCAudience:    authTestAudience,
		OIDCClients:     []string{authTestClient, "straza-agent-deploy"},
		OIDCClientClaim: "azp",
		GatewaySecret:   authTestGateway,
		ListingToken:    authTestListing,
	}
}

// authTestEnv is the HTTP handler with caller authentication, on httptest.
type authTestEnv struct {
	iss *authTestIssuer
	rec *authTestRecorder
	url string
}

func newAuthTestEnv(t *testing.T) *authTestEnv {
	t.Helper()
	iss := newAuthTestIssuer(t)
	cfg := authTestConfig(iss)
	authn, err := oidcauth.New(context.Background(), cfg.OIDCIssuer, cfg.OIDCDiscoveryURL, cfg.OIDCAudience, cfg.OIDCClientClaim, cfg.OIDCClients)
	if err != nil {
		t.Fatalf("oidcauth.New: %v", err)
	}
	rec := &authTestRecorder{}
	srv := httptest.NewServer(mcpHTTPHandler(newAuthTestServer(rec), cfg, authn))
	t.Cleanup(srv.Close)
	return &authTestEnv{iss: iss, rec: rec, url: srv.URL + "/mcp"}
}

type authTestBearer struct {
	token string
	base  http.RoundTripper
}

func (b authTestBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

// connect opens an MCP session with token as the bearer.
func (e *authTestEnv) connect(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             e.url,
		HTTPClient:           &http.Client{Transport: authTestBearer{token: token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "t"}, nil).
		Connect(context.Background(), transport, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

// mustConnect is connect for a token that must be accepted.
func (e *authTestEnv) mustConnect(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	cs, err := e.connect(t, token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return cs
}

// post sends one JSON-RPC message as a raw HTTP request, with token as the
// bearer when set and sessionID when set, and returns the status and body.
func (e *authTestEnv) post(t *testing.T, token, sessionID, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", e.url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const (
	authTestInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"t"}}}`
	authTestCallPRGet  = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"pr_get","arguments":{"repo":"Strazahq/pay-api"}}}`
)

func callAuthTestTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// authTestCode is the code in an error result's _meta, as a client decodes it.
func authTestCode(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	payload, ok := res.Meta[errorMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("result _meta has no %q: %v", errorMetaKey, res.Meta)
	}
	code, _ := payload["code"].(string)
	return code
}

func authTestText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// authTestNoSecret fails when text holds any credential the tests use.
func authTestNoSecret(t *testing.T, text string, presented ...string) {
	t.Helper()
	for _, s := range append([]string{authTestListing, authTestGateway}, presented...) {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("text echoes a credential: %q", text)
		}
	}
}

func TestCallerAuthNoTokenIs401(t *testing.T) {
	e := newAuthTestEnv(t)
	status, body := e.post(t, "", "", authTestInitialize)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", status, body)
	}
	if _, err := e.connect(t, ""); err == nil {
		t.Fatal("an MCP client connected without a token")
	}
	if got := e.rec.snapshot(); len(got) != 0 {
		t.Errorf("tools ran: %v", got)
	}
}

// The listing token pages through repo_list, and on any other tool it gets
// the coded refusal.
func TestListingTokenOpensRepoListOnly(t *testing.T) {
	e := newAuthTestEnv(t)
	cs := e.mustConnect(t, authTestListing)

	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("tools/list with the listing token: %v", err)
	}

	var repos []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatal("repo_list never stopped returning nextCursor")
		}
		args := map[string]any{}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res := callAuthTestTool(t, cs, "repo_list", args)
		if res.IsError {
			t.Fatalf("repo_list with the listing token: %s", authTestText(res))
		}
		var out authTestListOut
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("repo_list output: %v", err)
		}
		repos = append(repos, out.Repos...)
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if want := []string{"Strazahq/pay-api", "Strazahq/web"}; !slices.Equal(repos, want) {
		t.Errorf("repos = %v, want %v", repos, want)
	}

	res := callAuthTestTool(t, cs, "pr_get", map[string]any{"repo": "Strazahq/pay-api"})
	if !res.IsError {
		t.Fatalf("pr_get with the listing token was not refused: %v", res.StructuredContent)
	}
	if code := authTestCode(t, res); code != CodeListingTokenOnly {
		t.Errorf("code = %q, want %q", code, CodeListingTokenOnly)
	}
	if got := authTestText(res); got != listingTokenRefusal {
		t.Errorf("text = %q, want %q", got, listingTokenRefusal)
	}

	// A tool that does not exist is refused the same way, before the SDK
	// can say whether it exists.
	res = callAuthTestTool(t, cs, "no_such_tool", map[string]any{})
	if !res.IsError || authTestCode(t, res) != CodeListingTokenOnly {
		t.Errorf("an unknown tool with the listing token: %+v", res)
	}

	for _, c := range e.rec.snapshot() {
		if c.tool != "repo_list" {
			t.Errorf("%s ran with the listing token", c.tool)
		}
		if !c.known || c.caller != (Caller{Kind: callerListing}) {
			t.Errorf("repo_list saw caller %+v (known %v), want the listing caller", c.caller, c.known)
		}
	}
}

func TestGatewaySecretPasses(t *testing.T) {
	e := newAuthTestEnv(t)
	cs := e.mustConnect(t, authTestGateway)
	for tool, args := range map[string]map[string]any{
		"pr_get":    {"repo": "Strazahq/pay-api"},
		"repo_list": {},
	} {
		if res := callAuthTestTool(t, cs, tool, args); res.IsError {
			t.Fatalf("%s with the gateway secret: %s", tool, authTestText(res))
		}
	}
	calls := e.rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2", calls)
	}
	for _, c := range calls {
		if !c.known || c.caller != (Caller{Kind: callerGateway}) {
			t.Errorf("%s saw caller %+v (known %v), want the gateway caller without an agent", c.tool, c.caller, c.known)
		}
	}
}

func TestOIDCTokenFromListedClientPasses(t *testing.T) {
	e := newAuthTestEnv(t)
	cs := e.mustConnect(t, e.iss.mint(t, nil))
	if res := callAuthTestTool(t, cs, "pr_get", map[string]any{"repo": "Strazahq/pay-api"}); res.IsError {
		t.Fatalf("pr_get with an agent token: %s", authTestText(res))
	}
	calls := e.rec.snapshot()
	want := Caller{Kind: callerAgent, Client: authTestClient, Subject: authTestSubject}
	if len(calls) != 1 || !calls[0].known || calls[0].caller != want {
		t.Errorf("calls = %+v, want one pr_get by %+v", calls, want)
	}
}

// A token from another issuer, audience or client is refused with HTTP 401
// before any tool runs, as for no token.
func TestCallerAuthRejectsBadTokens(t *testing.T) {
	e := newAuthTestEnv(t)
	stranger := newAuthTestIssuer(t)

	tests := []struct {
		name string
		// signer mints the token, e.iss unless set. raw, when set, is sent
		// in place of a minted token.
		signer *authTestIssuer
		mutate func(map[string]any)
		raw    string
		// want is a part of the 401 body that says what to fix.
		want string
	}{
		{
			name:   "wrong issuer",
			signer: stranger,
			mutate: func(c map[string]any) { c["iss"] = stranger.server.URL },
			want:   envOIDCIssuer,
		},
		{
			name:   "issuer claim of another provider",
			mutate: func(c map[string]any) { c["iss"] = "https://idp.example/realms/other" },
			want:   "different provider",
		},
		{
			name:   "wrong audience",
			mutate: func(c map[string]any) { c["aud"] = "another-service" },
			want:   "audience",
		},
		{
			name:   "expired",
			mutate: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
			want:   "expired",
		},
		{
			name:   "client not listed",
			mutate: func(c map[string]any) { c["azp"] = "agent-own-login" },
			want:   envOIDCClients,
		},
		{
			name:   "client claim missing",
			mutate: func(c map[string]any) { delete(c, "azp") },
			want:   envOIDCClientClaim,
		},
		{
			name:   "signed by another key",
			signer: stranger,
			want:   "signature",
		},
		{name: "near miss of the gateway secret", raw: authTestGateway + "x", want: "nor a JWT"},
		{name: "prefix of the listing token", raw: authTestListing[:10], want: "nor a JWT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := tt.raw
			if token == "" {
				signer := tt.signer
				if signer == nil {
					signer = e.iss
				}
				token = signer.mint(t, func(c map[string]any) {
					c["iss"] = e.iss.server.URL
					if tt.mutate != nil {
						tt.mutate(c)
					}
				})
			}
			status, body := e.post(t, token, "", authTestInitialize)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", status, body)
			}
			t.Logf("401 body: %s", body)
			if !strings.Contains(body, tt.want) {
				t.Errorf("401 body %q does not contain %q", body, tt.want)
			}
			authTestNoSecret(t, body, token)
		})
	}
	if got := e.rec.snapshot(); len(got) != 0 {
		t.Errorf("tools ran: %v", got)
	}
}

// A bad token on an open session is refused too, before the tool runs.
func TestBadTokenOnOpenSessionRunsNoTool(t *testing.T) {
	e := newAuthTestEnv(t)
	cs := e.mustConnect(t, authTestGateway)
	bad := e.iss.mint(t, func(c map[string]any) { c["aud"] = "another-service" })
	status, body := e.post(t, bad, cs.ID(), authTestCallPRGet)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", status, body)
	}
	if got := e.rec.snapshot(); len(got) != 0 {
		t.Errorf("tools ran: %v", got)
	}
}

// A session belongs to the caller that opened it. Another caller's token,
// even an accepted one, cannot reuse it.
func TestSessionBoundToCaller(t *testing.T) {
	e := newAuthTestEnv(t)
	listing := e.mustConnect(t, authTestListing)
	gateway := e.mustConnect(t, authTestGateway)
	agent := e.mustConnect(t, e.iss.mint(t, nil))

	otherSubject := e.iss.mint(t, func(c map[string]any) { c["sub"] = "service-account-other" })
	otherClient := e.iss.mint(t, func(c map[string]any) { c["azp"] = "straza-agent-deploy" })
	sameAgent := e.iss.mint(t, func(c map[string]any) { c["iat"] = time.Now().Unix() })

	tests := []struct {
		name    string
		session string
		token   string
		want    int
	}{
		{name: "gateway secret on the listing session", session: listing.ID(), token: authTestGateway, want: http.StatusForbidden},
		{name: "agent token on the listing session", session: listing.ID(), token: e.iss.mint(t, nil), want: http.StatusForbidden},
		{name: "listing token on the gateway session", session: gateway.ID(), token: authTestListing, want: http.StatusForbidden},
		{name: "gateway secret on the agent session", session: agent.ID(), token: authTestGateway, want: http.StatusForbidden},
		{name: "another subject on the agent session", session: agent.ID(), token: otherSubject, want: http.StatusForbidden},
		{name: "another client on the agent session", session: agent.ID(), token: otherClient, want: http.StatusForbidden},
		{name: "no token on the gateway session", session: gateway.ID(), token: "", want: http.StatusUnauthorized},
		{name: "a fresh token of the same agent", session: agent.ID(), token: sameAgent, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := e.post(t, tt.token, tt.session, authTestCallPRGet)
			if status != tt.want {
				t.Fatalf("status = %d, want %d: %s", status, tt.want, body)
			}
		})
	}
	// Only the last row ran a tool, as the agent that opened the session.
	calls := e.rec.snapshot()
	want := Caller{Kind: callerAgent, Client: authTestClient, Subject: authTestSubject}
	if len(calls) != 1 || calls[0].caller != want {
		t.Errorf("calls = %+v, want one by %+v", calls, want)
	}
}

func TestStaticSecretMatches(t *testing.T) {
	s := newStaticSecret("s3cret-value")
	for token, want := range map[string]bool{
		"s3cret-value":  true,
		"s3cret-valu":   false,
		"s3cret-valuee": false,
		"S3cret-value":  false,
		"":              false,
	} {
		if got := s.matches(token); got != want {
			t.Errorf("matches(%q) = %v, want %v", token, got, want)
		}
	}
	unset := newStaticSecret("")
	if unset.matches("") || unset.matches("anything") {
		t.Error("an unset secret matched")
	}
}

// The static bearers are compared in constant time: staticSecret.matches
// goes through subtle.ConstantTimeCompare, and rsauth.go never compares a
// token or a configured secret with == or != other than against "", nor
// with a comparison from strings or bytes.
func TestStaticSecretsComparedInConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "rsauth.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sensitive := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name == "token" || x.Name == "secret"
		case *ast.SelectorExpr:
			return x.Sel.Name == "ListingToken" || x.Sel.Name == "GatewaySecret"
		}
		return false
	}
	emptyString := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Value == `""`
	}
	var constantTime bool
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == "matches" {
				ast.Inspect(x.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok {
						if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
							constantTime = true
						}
					}
					return true
				})
			}
		case *ast.BinaryExpr:
			if x.Op != token.EQL && x.Op != token.NEQ {
				break
			}
			if (sensitive(x.X) && !emptyString(x.Y)) || (sensitive(x.Y) && !emptyString(x.X)) {
				t.Errorf("%s: a credential compared with %s", fset.Position(x.Pos()), x.Op)
			}
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && (pkg.Name == "strings" || pkg.Name == "bytes") &&
				slices.Contains([]string{"Compare", "EqualFold", "Equal", "HasPrefix", "HasSuffix"}, x.Sel.Name) {
				t.Errorf("%s: %s.%s in rsauth.go", fset.Position(x.Pos()), pkg.Name, x.Sel.Name)
			}
		}
		return true
	})
	if !constantTime {
		t.Error("staticSecret.matches does not call subtle.ConstantTimeCompare")
	}
}

func TestCallerUserIDsDiffer(t *testing.T) {
	ids := map[string]Caller{}
	for _, c := range []Caller{
		{Kind: callerListing},
		{Kind: callerGateway},
		{Kind: callerAgent, Client: "a", Subject: "b"},
		{Kind: callerAgent, Client: "a b", Subject: ""},
		{Kind: callerAgent, Client: "a", Subject: "b c"},
		{Kind: callerAgent, Client: `a" "b`, Subject: "c"},
		{Kind: callerAgent, Client: "a", Subject: `b" "c`},
	} {
		id := callerUserID(c)
		if prev, dup := ids[id]; dup {
			t.Errorf("callers %+v and %+v share the session owner %q", prev, c, id)
		}
		ids[id] = c
	}
}
