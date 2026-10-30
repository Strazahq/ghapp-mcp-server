package main

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

var gitOn = Config{Git: testGitConfig("https://ghapp.example", "https://github.com", testIssuer+strazaJWKSPath)}

// The git tools exist only with git transport on.
func TestGitToolsOnlyWithGitTransport(t *testing.T) {
	off := listTools(t, connectFake(t, newFakeGitHub(), Config{}))
	for _, name := range []string{"git_fetch", "git_push"} {
		if _, ok := off[name]; ok {
			t.Errorf("%s is listed without %s", name, envPublicURL)
		}
	}
	on := listTools(t, connectFake(t, newFakeGitHub(), gitOn))
	for _, name := range []string{"git_fetch", "git_push"} {
		if _, ok := on[name]; !ok {
			t.Errorf("%s is not listed with git transport on", name)
		}
	}
}

// Both tools carry the repo mark and a required top-level repo, and a push
// an optional branch. Neither changes anything, so both carry read hints,
// and their descriptions say how to use plain git.
func TestGitToolsMarks(t *testing.T) {
	tools := listTools(t, connectFake(t, newFakeGitHub(), gitOn))
	for name, want := range map[string][]string{"git_fetch": {"repo"}, "git_push": {"branch", "repo"}} {
		tool := tools[name]
		if tool == nil {
			t.Fatalf("%s is not listed", name)
		}
		mark, _ := tool.Meta[metaObject].(map[string]any)
		if !reflect.DeepEqual(mark, map[string]any{"type": "repo", "argument": "repo"}) {
			t.Errorf("%s mark = %v", name, tool.Meta[metaObject])
		}
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if got := slices.Sorted(maps.Keys(props)); !slices.Equal(got, want) {
			t.Errorf("%s properties = %v, want %v", name, got, want)
		}
		for _, p := range props {
			if p.(map[string]any)["type"] != "string" {
				t.Errorf("%s properties = %v, want strings", name, props)
			}
		}
		if req, _ := schema["required"].([]any); !slices.Equal(req, []any{"repo"}) || schema["additionalProperties"] != false {
			t.Errorf("%s schema = %v", name, schema)
		}
		if !strings.HasPrefix(tool.Description, "Check that you may") || !strings.Contains(tool.Description, "plain git") ||
			strings.Contains(tool.Description, "ticket") {
			t.Errorf("%s description = %q, want how to use plain git", name, tool.Description)
		}
		a := tool.Annotations
		if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
			t.Fatalf("%s annotations = %+v, want every hint set", name, a)
		}
		if !a.ReadOnlyHint || *a.DestructiveHint || !a.IdempotentHint || !*a.OpenWorldHint {
			t.Errorf("%s hints: readOnly %v, destructive %v, idempotent %v", name, a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint)
		}
		if strings.Contains(tool.Description, "\u2014") {
			t.Errorf("%s description has an em dash", name)
		}
	}
}

// Without an id, a metadata token for the named repo resolves the name
// first, as for every repo tool.
func TestGitToolsWithoutID(t *testing.T) {
	f := newFakeGitHub()
	cs := connectFake(t, f, gitOn)
	out := callOK(t, cs, "git_fetch", map[string]any{"repo": pay}, nil)
	if out["repoId"] != "812345678" || out["command"] != "git clone https://github.com/Strazahq/pay-api.git" {
		t.Errorf("output = %v", out)
	}
	mints, _ := f.snapshot()
	want := []mintRec{nameMintOf(orgInst, pay, payID, permMetadataRead), mintOf(orgInst, []int64{payID}, permContentsRead)}
	if got := mintsWithout(mints); !reflect.DeepEqual(got, want) {
		t.Errorf("mints = %+v, want %+v", got, want)
	}
}

// withAgent makes every call on server come from an agent's token, as
// callerGate does behind Straza.
func withAgent(server *mcp.Server) *mcp.Server {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			return next(withCaller(ctx, Caller{Kind: callerAgent, Client: "ghapp-agent-7", Subject: "4b1c"}), method, req)
		}
	})
	return server
}

// The access checks refuse as for every repo tool.
func TestGitToolsAccessRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo string
		meta mcp.Meta
		code string
	}{
		{"a case variant", "strazahq/pay-api", nil, CodeRepoNotCanonical},
		{"a case variant with the id", "strazahq/PAY-api", withID("812345678"), CodeRepoNotCanonical},
		{"a renamed repo", "Strazahq/pay-old", nil, CodeRepoRenamed},
		{"the id of another repo", pay, withID("812345679"), CodeRepoIDMismatch},
		{"an unknown repo", "Strazahq/nope", nil, CodeNotFound},
		{"not owner/name", "pay-api", nil, CodeInvalidInput},
		{"a bad id", pay, withID(812345678), CodeInvalidInput},
	} {
		for _, tool := range []string{"git_fetch", "git_push"} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				cs := connectFake(t, newFakeGitHub(), gitOn)
				text, payload := callErr(t, cs, tool, map[string]any{"repo": tc.repo}, tc.meta)
				if payload["code"] != tc.code {
					t.Errorf("code = %v, want %s: %s", payload["code"], tc.code, text)
				}
			})
		}
	}
	// A second repo key is refused as everywhere.
	cs := connectFake(t, newFakeGitHub(), gitOn)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "git_push",
		Arguments: json.RawMessage(`{"repo":"Strazahq/other","repo":"Strazahq/pay-api"}`)})
	if err != nil || !res.IsError {
		t.Errorf("a duplicate repo key: %v, %s", err, resultText(res))
	}
}

// A branch that git or a shell would take differently is invalid-input
// before any GitHub call, and quoted safely.
func TestGitToolsBranchRefused(t *testing.T) {
	for _, branch := range []string{"-f", "a b", "a;rm -rf x", "$(id)", "`id`", "a..b", "a/", "/a", "a//b", ".hidden", "a.lock",
		"a.", "refs/heads/x:y", "a~1", "a^", "a?", "a*", "a[", `a\b`, "@", "a@{1}", "a'b", `a"b`, "a\nb", "é", strings.Repeat("a", 201)} {
		t.Run(branch, func(t *testing.T) {
			f := newFakeGitHub()
			text, payload := callErr(t, connectFake(t, f, gitOn), "git_push", map[string]any{"repo": pay, "branch": branch}, withID("812345678"))
			if payload["code"] != CodeInvalidInput || payload["field"] != branchArgument || !strings.Contains(text, "branch must be") {
				t.Errorf("payload %v, text %q", payload, text)
			}
			if mints, calls := f.snapshot(); len(mints) != 0 || len(calls) != 0 {
				t.Errorf("a bad branch reached GitHub")
			}
		})
	}
	// The fetch tool takes no branch.
	_, payload := callErr(t, connectFake(t, newFakeGitHub(), gitOn), "git_fetch", map[string]any{"repo": pay, "branch": "main"}, withID("812345678"))
	if payload["code"] != CodeInvalidInput {
		t.Errorf("git_fetch with a branch: %v", payload)
	}
}

// shellWord leaves plain words bare and quotes the rest for a POSIX shell.
func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"https://ghapp.example/git/Strazahq/pay-api.git": "https://ghapp.example/git/Strazahq/pay-api.git",
		"HEAD:refs/heads/feature/x":                      "HEAD:refs/heads/feature/x",
		"HEAD:refs/heads/<branch>":                       "HEAD:refs/heads/<branch>",
		"+refs/heads/*:refs/remotes/origin/*":            "'+refs/heads/*:refs/remotes/origin/*'",
		"https://g.example/a b/git/o/n.git":              "'https://g.example/a b/git/o/n.git'",
		"it's":                                           `'it'\''s'`,
		"$HOME":                                          "'$HOME'",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// The listing token never opens a git tool: callerGate refuses it first.
func TestGitToolsNotForTheListingToken(t *testing.T) {
	f := newFakeGitHub()
	server := newMCPServer(f, gitOn)
	server.AddReceivingMiddleware(callerGate)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if call, ok := req.(*mcp.CallToolRequest); ok {
				call.Extra = &mcp.RequestExtra{TokenInfo: staticTokenInfo(Caller{Kind: callerListing})}
			}
			return next(ctx, method, req)
		}
	})
	_, payload := callErr(t, connect(t, server), "git_push", map[string]any{"repo": pay}, nil)
	if payload["code"] != CodeListingTokenOnly {
		t.Errorf("payload = %v", payload)
	}
	if mints, _ := f.snapshot(); len(mints) != 0 {
		t.Errorf("minted %+v", mints)
	}
}

// git_push and git_fetch run the repo checks and the token mint of every
// repo tool, hand out nothing, and answer with the plain git command for
// Straza's holder. git_push stays out in read-only mode.
func TestGitTools(t *testing.T) {
	for _, tc := range []struct {
		tool, op string
		args     map[string]any
		perms    github.Permissions
		command  string
	}{
		{"git_push", gitOpPush, map[string]any{"repo": pay, "branch": "fix-login"}, github.Permissions{"contents": "write"},
			"git push https://github.com/Strazahq/pay-api.git HEAD:refs/heads/fix-login"},
		{"git_push", gitOpPush, map[string]any{"repo": pay}, github.Permissions{"contents": "write"},
			"git push https://github.com/Strazahq/pay-api.git HEAD:refs/heads/<branch>"},
		{"git_fetch", gitOpFetch, map[string]any{"repo": pay}, github.Permissions{"contents": "read"},
			"git clone https://github.com/Strazahq/pay-api.git"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			f := newFakeGitHub()
			cs := connect(t, withAgent(newMCPServer(f, gitOn)))
			res := callTool(t, cs, tc.tool, tc.args, withID("812345678"))
			if res.IsError {
				t.Fatalf("%s: %s", tc.tool, resultText(res))
			}
			b, _ := json.Marshal(res.StructuredContent)
			var out map[string]any
			_ = json.Unmarshal(b, &out)
			if out["op"] != tc.op || out["repo"] != pay || out["repoId"] != "812345678" ||
				out["remote"] != "https://github.com/Strazahq/pay-api.git" || out["command"] != tc.command {
				t.Errorf("output = %v", out)
			}
			for _, key := range []string{"ticket", "header", "username", "gitUrl"} {
				if _, ok := out[key]; ok {
					t.Errorf("output holds %s", key)
				}
			}
			text := resultText(res)
			if !strings.Contains(text, "You may") || !strings.Contains(text, "No token is needed") {
				t.Errorf("text = %q", text)
			}
			mints, _ := f.snapshot()
			if got := mintsWithout(mints); !reflect.DeepEqual(got, []mintRec{mintOf(orgInst, []int64{payID}, tc.perms)}) {
				t.Errorf("mints = %+v", got)
			}
		})
	}

	ro := gitOn
	ro.ReadOnly = true
	roTools := listTools(t, connectFake(t, newFakeGitHub(), ro))
	if _, ok := roTools["git_push"]; ok {
		t.Error("git_push is listed in read-only mode")
	}
	if _, ok := roTools["git_fetch"]; !ok {
		t.Error("git_fetch is missing in read-only mode")
	}
}
