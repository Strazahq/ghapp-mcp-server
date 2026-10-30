package main

import (
	"maps"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// The "token minted" line names the caller, the installation, the repo ids,
// the permissions, the expiry and the token's hash, and the "tool call" line
// the tool, the caller, the repo, its id and that Straza named it, the same
// hash, whether the token was cached, the outcome and the duration.
func TestAuditLogLines(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	now := t0
	s.setClock(func() time.Time { return now })
	c := allToolCases()[5] // pr_create
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	callOK(t, s.cs, c.tool, c.args, payMeta(true))

	mints, _ := f.snapshot()
	if len(mints) != 1 {
		t.Fatalf("%d mints, want 1", len(mints))
	}
	hash := TokenHash(tokenOf(mints[0]))
	minted := s.logLines(t, "token minted")
	if len(minted) != 1 {
		t.Fatalf("%d token minted lines, want 1", len(minted))
	}
	wantMinted := map[string]any{
		"caller":       map[string]any{"kind": "agent", "client": agentA.Client, "subject": agentA.Subject},
		"installation": float64(orgInst),
		"repo_ids":     []any{float64(payID)},
		"all_repos":    false,
		"perms":        map[string]any{"pull_requests": "write", "contents": "read"},
		"expires":      "2026-10-18T13:00:00Z",
		"token_hash":   hash,
	}
	for k, want := range wantMinted {
		if got := minted[0][k]; !reflect.DeepEqual(got, want) {
			t.Errorf("token minted %s = %#v, want %#v", k, got, want)
		}
	}

	calls := s.logLines(t, "tool call")
	if len(calls) != 2 {
		t.Fatalf("%d tool call lines, want 2", len(calls))
	}
	for i, line := range calls {
		want := map[string]any{
			"tool":         "pr_create",
			"caller":       wantMinted["caller"],
			"repo":         pay,
			"repo_id":      float64(payID),
			"repo_id_from": "straza",
			"token_hash":   hash,
			"cached":       i == 1,
			"outcome":      "ok",
			"retried":      false,
		}
		for k, w := range want {
			if got := line[k]; !reflect.DeepEqual(got, w) {
				t.Errorf("tool call %d: %s = %#v, want %#v", i, k, got, w)
			}
		}
		if _, ok := line["duration_ms"].(float64); !ok {
			t.Errorf("tool call %d: duration_ms = %#v", i, line["duration_ms"])
		}
	}
}

// tokenOf is the token the fake minted for m.
func tokenOf(m mintRec) github.Token { return github.Token{Value: m.Value} }

// A refusal logs its code and the repo argument, without a repo id or a
// token hash: no call went out with a token. The name path logs two mints,
// the metadata token for the whole installation and the tool's. repo_list
// logs its token.
func TestAuditLogOutcomes(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	c := allToolCases()[4] // pr_get
	callErr(t, s.cs, c.tool, c.args, asAgent(agentA.Client, agentA.Subject, withID("812345679")))
	callErr(t, s.cs, c.tool, map[string]any{"repo": "pay api\n", "number": 7}, nil)
	callOK(t, s.cs, c.tool, c.args, nil)
	callOK(t, s.cs, "repo_list", nil, asCaller(callerListing, nil))

	calls := s.logLines(t, "tool call")
	if len(calls) != 4 {
		t.Fatalf("%d tool call lines, want 4", len(calls))
	}
	if l := calls[0]; l["outcome"] != CodeRepoIDMismatch || l["repo"] != pay || l["repo_id"] != nil || l["token_hash"] != nil || l["cached"] != false {
		t.Errorf("refusal line = %v", l)
	}
	if l := calls[1]; l["outcome"] != CodeInvalidInput || l["repo"] != "pay api " || l["token_hash"] != nil {
		t.Errorf("invalid input line = %v", l)
	}
	if l := calls[2]; l["outcome"] != "ok" || !reflect.DeepEqual(l["caller"], map[string]any{"kind": "local"}) || l["repo_id_from"] != "lookup" {
		t.Errorf("local line = %v", l)
	}
	if l := calls[3]; l["tool"] != "repo_list" || l["repo"] != nil || l["token_hash"] == nil ||
		!reflect.DeepEqual(l["caller"], map[string]any{"kind": "listing"}) {
		t.Errorf("repo_list line = %v", l)
	}

	var allRepos []bool
	for _, l := range s.logLines(t, "token minted") {
		allRepos = append(allRepos, l["all_repos"].(bool))
	}
	// The mismatch's mint, then the name path's metadata mint for the one
	// repo and the tool's mint, then repo_list's, the only one on the whole
	// installation.
	if want := []bool{false, false, false, true}; !reflect.DeepEqual(allRepos, want) {
		t.Errorf("token minted all_repos = %v, want %v", allRepos, want)
	}
}

// A retried call logs its last attempt.
func TestAuditLogRetry(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	c := allToolCases()[4]
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	f.setRepo(payID, func(r *fakeRepo) { r.Installation = 11 })
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	calls := s.logLines(t, "tool call")
	if l := calls[1]; l["retried"] != true || l["outcome"] != "ok" || l["cached"] != false {
		t.Errorf("retried line = %v", l)
	}
	minted := s.logLines(t, "token minted")
	if l := minted[len(minted)-1]; l["installation"] != float64(11) || l["token_hash"] != calls[1]["token_hash"] {
		t.Errorf("the retry's mint = %v, call %v", l, calls[1])
	}
}

// The log never holds a token, a secret, or an argument other than repo,
// over HTTP with the gateway secret as over the in-memory transport.
func TestAuditLogHoldsNoSecret(t *testing.T) {
	f := fakeWithRoutes()
	cfg := Config{GatewaySecret: authTestGateway, ListingToken: authTestListing}
	logs := &syncBuffer{}
	tl := newTools(f, cfg, newAuditLogger(logs))
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, nil)
	tl.register(server, cfg)
	server.AddReceivingMiddleware(errorCodes)
	srv := httptest.NewServer(mcpHTTPHandler(server, cfg, nil))
	t.Cleanup(srv.Close)
	e := &authTestEnv{url: srv.URL + "/mcp"}

	const bodyText = "ghapp-body-text-for-tests-only"
	gw := e.mustConnect(t, authTestGateway)
	for _, c := range allToolCases() {
		args := maps.Clone(c.args)
		if _, ok := args["body"]; ok {
			args["body"] = "a body with " + bodyText
		}
		callOK(t, gw, c.tool, args, withID("812345678"))
		callOK(t, gw, c.tool, args, nil)
	}
	listing := e.mustConnect(t, authTestListing)
	callOK(t, listing, "repo_list", nil, nil)

	text := logs.String()
	f.mu.Lock()
	values := slices.Collect(maps.Keys(f.tokens))
	f.mu.Unlock()
	if len(values) < 3 {
		t.Fatalf("only %d tokens minted", len(values))
	}
	for _, secret := range append(values, authTestGateway, authTestListing, bodyText, "ghs_") {
		if strings.Contains(text, secret) {
			t.Errorf("the audit log holds %q", secret)
		}
	}
	s := &testServer{log: logs}
	lines := s.logLines(t, "tool call")
	if len(lines) != 2*len(allToolCases())+1 {
		t.Fatalf("%d tool call lines", len(lines))
	}
	for _, l := range lines[:len(lines)-1] {
		if !reflect.DeepEqual(l["caller"], map[string]any{"kind": "gateway"}) {
			t.Errorf("caller = %v, want the gateway", l["caller"])
		}
	}
	for _, l := range s.logLines(t, "token minted") {
		if hash, _ := l["token_hash"].(string); len(hash) != 44 || !strings.HasSuffix(hash, "=") {
			t.Errorf("token_hash %q is not base64 of 32 bytes with padding", hash)
		}
	}
}
