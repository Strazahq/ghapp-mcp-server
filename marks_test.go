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
)

// listTools returns tools/list by name.
func listTools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	out := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		out[tool.Name] = tool
	}
	return out
}

// Case 1: every repo tool carries the mark and a required top-level string
// repo, its annotations are set deliberately, and its schema forbids other
// properties. repo_list carries the listing mark only.
func TestToolsListMarks(t *testing.T) {
	cs := connectFake(t, newFakeGitHub(), Config{})
	tools := listTools(t, cs)

	readOnly := map[string]bool{
		"repo_get": true, "branch_list": true, "file_get": true, "pr_list": true, "pr_get": true,
		"issue_list": true, "issue_get": true, "search_code": true, "search_issues": true,
		"pr_create": false, "pr_comment": false, "pr_merge": false, "issue_create": false, "issue_comment": false,
	}
	destructive := map[string]bool{"pr_merge": true}

	names := slices.Sorted(maps.Keys(tools))
	want := append(slices.Collect(maps.Keys(readOnly)), "repo_list")
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	for name, ro := range readOnly {
		tool := tools[name]
		t.Run(name, func(t *testing.T) {
			mark, _ := tool.Meta[metaObject].(map[string]any)
			if !reflect.DeepEqual(mark, map[string]any{"type": "repo", "argument": "repo"}) {
				t.Errorf("mark = %v", tool.Meta[metaObject])
			}
			schema, _ := tool.InputSchema.(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			repo, _ := props["repo"].(map[string]any)
			if repo["type"] != "string" {
				t.Errorf("repo property = %v, want a string", props["repo"])
			}
			if req, _ := schema["required"].([]any); !slices.Contains(req, any("repo")) {
				t.Errorf("required = %v, want repo in it", schema["required"])
			}
			if schema["additionalProperties"] != false {
				t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
			}
			a := tool.Annotations
			if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
				t.Fatalf("annotations = %+v, want every hint set", a)
			}
			if a.ReadOnlyHint != ro || a.IdempotentHint != ro || *a.DestructiveHint != destructive[name] || !*a.OpenWorldHint {
				t.Errorf("annotations = readOnly %v, idempotent %v, destructive %v, openWorld %v",
					a.ReadOnlyHint, a.IdempotentHint, *a.DestructiveHint, *a.OpenWorldHint)
			}
			if strings.Contains(tool.Description, "\u2014") {
				t.Errorf("description has an em dash: %q", tool.Description)
			}
		})
	}

	list := tools["repo_list"]
	if mark, _ := list.Meta[metaObject].(map[string]any); mark["type"] != "repo" || mark["reach"] != "all" || mark["argument"] != nil {
		t.Errorf("repo_list %s = %v, want type repo and reach all", metaObject, list.Meta[metaObject])
	}
	lists, _ := list.Meta[metaListsObjects].(map[string]any)
	if lists["type"] != "repo" {
		t.Errorf("repo_list %s = %v", metaListsObjects, list.Meta[metaListsObjects])
	}
	if a := list.Annotations; a == nil || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("repo_list annotations = %+v", a)
	}
	schema, _ := list.InputSchema.(map[string]any)
	if req, _ := schema["required"].([]any); len(req) != 0 {
		t.Errorf("repo_list requires %v, want only an optional cursor", req)
	}
}

// Read-only mode lists no write tool, and still lists every read tool and
// repo_list. A write tool is then unknown: a call to it mints no token and
// reaches nothing.
func TestReadOnlyListsNoWriteTool(t *testing.T) {
	f := fakeWithRoutes()
	cs := connectFake(t, f, Config{ReadOnly: true})
	tools := listTools(t, cs)

	want := []string{"repo_list"}
	for _, c := range allToolCases() {
		if !c.write {
			want = append(want, c.tool)
		}
	}
	slices.Sort(want)
	if names := slices.Sorted(maps.Keys(tools)); !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for name, tool := range tools {
		if a := tool.Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is listed in read-only mode but does not say readOnlyHint", name)
		}
	}

	for _, c := range allToolCases() {
		if !c.write {
			continue
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err == nil && !res.IsError {
			t.Errorf("%s answered in read-only mode: %s", c.tool, resultText(res))
		}
	}
	if mints, calls := f.snapshot(); len(mints) != 0 || len(calls) != 0 {
		t.Errorf("a write tool in read-only mode minted %+v and called %+v", mintsWithout(mints), calls)
	}
}

// Case 7: a repo tool called without repo is refused by the schema, before
// any token.
func TestMissingRepoRefusedBySchema(t *testing.T) {
	f := newFakeGitHub()
	cs := connectFake(t, f, Config{})
	for _, args := range []map[string]any{
		{"query": "fee"},
		{"repo": 7, "query": "fee"},
	} {
		text, payload := callErr(t, cs, "search_code", args, nil)
		if payload["code"] != CodeInvalidInput || !strings.HasPrefix(text, sdkValidationPrefix) {
			t.Errorf("%v: text %q, payload %v", args, text, payload)
		}
	}
	if mints, _ := f.snapshot(); len(mints) != 0 {
		t.Errorf("a refusal minted %+v", mintsWithout(mints))
	}
}

// The raw arguments carry exactly one key "repo": a case variant and a
// duplicate are refused before any token. The schema catches a case variant
// first; the duplicate passes the schema and is caught by ghapp.
func TestRawRepoKeyRefused(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		sdk       bool // refused by the SDK's schema check
	}{
		{"Repo next to repo", `{"repo":"Strazahq/pay-api","Repo":"Strazahq/other","number":7}`, true},
		{"Repo alone", `{"Repo":"Strazahq/pay-api","number":7}`, true},
		{"REPO alone", `{"REPO":"Strazahq/pay-api","number":7}`, true},
		{"a duplicate repo", `{"repo":"Strazahq/other","repo":"Strazahq/pay-api","number":7}`, false},
		{"a duplicate through an escape", `{"repo":"Strazahq/other","repo":"Strazahq/pay-api","number":7}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeWithRoutes()
			cs := connectFake(t, f, Config{})
			text, payload := callErr(t, cs, "pr_get", json.RawMessage(tc.raw), nil)
			if payload["code"] != CodeInvalidInput {
				t.Errorf("payload = %v, want %s (text %q)", payload, CodeInvalidInput, text)
			}
			if got := strings.HasPrefix(text, sdkValidationPrefix); got != tc.sdk {
				t.Errorf("refused by the SDK = %v, want %v (text %q)", got, tc.sdk, text)
			}
			if mints, _ := f.snapshot(); len(mints) != 0 {
				t.Errorf("a refusal minted %+v", mintsWithout(mints))
			}
		})
	}
}

func TestCheckRepoKey(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{``, true},
		{`null`, true},
		{`{}`, true},
		{`{"repo":"a/b"}`, true},
		{`{"repo":"a/b","body":{"repo":"c/d","Repo":"e/f"}}`, true}, // only the top level counts
		{`{"repository":"a/b","repo":"a/b"}`, true},
		{`{"repo":"a/b","repo":"a/b"}`, false},
		{`{"rEpO":"a/b"}`, false},
		{`{"repo":"a/b","Repo":"a/b"}`, false},
		{`{"Repo":"a/b"}`, false},
		{`[1]`, false},
	} {
		err := checkRepoKey(json.RawMessage(tc.raw))
		if (err == nil) != tc.ok {
			t.Errorf("checkRepoKey(%s) = %v, want ok %v", tc.raw, err, tc.ok)
		}
		if err != nil {
			if code, _ := errorCode(err); code != CodeInvalidInput {
				t.Errorf("checkRepoKey(%s) code = %s", tc.raw, code)
			}
		}
	}
}

func TestCheckRepoName(t *testing.T) {
	for _, tc := range []struct {
		repo string
		ok   bool
	}{
		{"Strazahq/pay-api", true},
		{"a/b", true},
		{"a-1/b.c_d-e", true},
		{"o/.github", true},
		{"o/..x", true},
		{"o/.", false},
		{"o/..", false},
		{"/b", false},
		{"a/", false},
		{"a", false},
		{"a/b/c", false},
		{"a b/c", false},
		{"a_b/c", false},
		{"a.b/c", false},
		{"a/b c", false},
		{"a/b%2Fc", false},
		{"a/b\n", false},
	} {
		if err := checkRepoName(tc.repo); (err == nil) != tc.ok {
			t.Errorf("checkRepoName(%q) = %v, want ok %v", tc.repo, err, tc.ok)
		}
	}
}
