package main

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const footerA = "Sent by agent `straza-agent-build` through ghapp-mcp-server."

// sentBody is the body field of the one write the call sent, and whether
// the request had one.
func sentBody(t *testing.T, f *fakeGitHub) (string, bool) {
	t.Helper()
	_, calls := f.snapshot()
	d := doCalls(calls)
	if len(d) != 1 {
		t.Fatalf("%d writes, want 1", len(d))
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(d[0].Body), &req); err != nil {
		t.Fatalf("request body %q: %v", d[0].Body, err)
	}
	body, ok := req["body"].(string)
	return body, ok
}

// For an agent with a known client, pr_create, pr_comment, issue_create and
// issue_comment end the body with a blank line and the footer, because
// GitHub shows only ghapp's bot. pr_merge sends no text and gets none.
func TestAgentFooter(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"pr_create", map[string]any{"repo": pay, "title": "Fix the fee", "head": "fix", "base": "main", "body": "Fixes #3.\n"}, "Fixes #3.\n\n" + footerA},
		{"pr_create", map[string]any{"repo": pay, "title": "Fix the fee", "head": "fix", "base": "main"}, footerA},
		{"pr_comment", map[string]any{"repo": pay, "number": 7, "body": "Looks good."}, "Looks good.\n\n" + footerA},
		{"issue_create", map[string]any{"repo": pay, "title": "Fees are wrong", "body": "See the invoice."}, "See the invoice.\n\n" + footerA},
		{"issue_create", map[string]any{"repo": pay, "title": "Fees are wrong"}, footerA},
		{"issue_comment", map[string]any{"repo": pay, "number": 3, "body": "Confirmed."}, "Confirmed.\n\n" + footerA},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			f := fakeWithRoutes()
			s := connectLogged(t, f, Config{})
			out := callOK(t, s.cs, tc.tool, tc.args, payMeta(true))
			if got, _ := sentBody(t, f); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			// The result shows the body as sent.
			if b, _ := out["body"].(map[string]any); b["body"] != tc.want {
				t.Errorf("result body = %v", out["body"])
			}
		})
	}

	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	callOK(t, s.cs, "pr_merge", map[string]any{"repo": pay, "number": 7}, payMeta(true))
	if body, ok := sentBody(t, f); ok {
		t.Errorf("pr_merge sent body %q", body)
	}
}

// No footer when it is off, or when no agent is known: the gateway secret,
// the listing token, or no caller authentication. The body goes as written.
func TestAgentFooterAbsent(t *testing.T) {
	comment := map[string]any{"repo": pay, "number": 7, "body": "Looks good."}
	create := map[string]any{"repo": pay, "title": "Fix the fee", "head": "fix", "base": "main"}
	for _, tc := range []struct {
		name string
		cfg  Config
		meta mcp.Meta
	}{
		{"off", Config{NoAgentFooter: true}, payMeta(true)},
		{"gateway", Config{}, asCaller(callerGateway, withID("812345678"))},
		{"listing", Config{}, asCaller(callerListing, withID("812345678"))},
		{"local", Config{}, withID("812345678")},
		{"agent without a client", Config{}, asAgent("", "sa", withID("812345678"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeWithRoutes()
			s := connectLogged(t, f, tc.cfg)
			callOK(t, s.cs, "pr_comment", comment, tc.meta)
			if got, _ := sentBody(t, f); got != "Looks good." {
				t.Errorf("pr_comment body = %q", got)
			}
			f.reset()
			callOK(t, s.cs, "pr_create", create, tc.meta)
			if got, ok := sentBody(t, f); ok {
				t.Errorf("pr_create without a body sent %q", got)
			}
		})
	}
}

// A client id cannot break out of the code span or add a line.
func TestAgentFooterClientIsPlain(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	meta := maps.Clone(payMeta(true))
	meta[testCallerKey] = callerAgent + "|a`b\nc @team|sa"
	callOK(t, s.cs, "issue_comment", map[string]any{"repo": pay, "number": 3, "body": "x"}, meta)
	if got, want := mustBody(t, f), "x\n\nSent by agent `ab c @team` through ghapp-mcp-server."; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func mustBody(t *testing.T, f *fakeGitHub) string {
	t.Helper()
	b, _ := sentBody(t, f)
	return b
}
