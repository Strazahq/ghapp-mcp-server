package main

import (
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// The resolution of a repo name and id, through pr_create: each refusal
// with its code, and the tokens minted on the way.
func TestAccessRefusals(t *testing.T) {
	write := permPRCreate
	meta := github.Permissions{"metadata": "read"}
	for _, tc := range []struct {
		name  string
		repo  string
		meta  mcp.Meta
		setup func(*fakeGitHub)
		code  string
		text  string
		mints []mintRec
	}{
		{name: "case 3, a case variant without an id", repo: "strazahq/pay-api",
			code: CodeRepoNotCanonical, text: "Send the repo as Strazahq/pay-api, exactly as GitHub names it.",
			mints: []mintRec{nameMintOf(orgInst, "strazahq/pay-api", payID, meta)}},
		{name: "a case variant with the id", repo: "strazahq/PAY-api", meta: withID("812345678"),
			code: CodeRepoNotCanonical, text: "Send the repo as Strazahq/pay-api, exactly as GitHub names it.",
			mints: []mintRec{mintOf(orgInst, []int64{payID}, write)}},
		{name: "case 4, a redirect without an id", repo: "Strazahq/pay-old",
			code: CodeRepoRenamed, text: "Strazahq/pay-old was renamed on GitHub. Call it by its new name."},
		{name: "a redirect with an id", repo: "Strazahq/pay-old", meta: withID("812345678"),
			code: CodeRepoRenamed},
		{name: "a redirect on the repo read", repo: "Strazahq/pay-api",
			setup: func(f *fakeGitHub) { f.readRedirects["strazahq/pay-api"] = true },
			code:  CodeRepoRenamed, mints: []mintRec{nameMintOf(orgInst, pay, payID, meta)}},
		{name: "another repo without a redirect", repo: "Strazahq/pay-api",
			setup: func(f *fakeGitHub) { f.aliases[pay] = "Strazahq/other" },
			code:  CodeRepoNotCanonical, mints: []mintRec{nameMintOf(orgInst, pay, otherID, meta)}},
		{name: "case 5, the id of another repo", repo: pay, meta: withID("812345679"),
			code:  CodeRepoIDMismatch,
			text:  "Strazahq/pay-api is not the repo Straza approved: it was probably deleted and created again. A person has to add the new repo to the role.",
			mints: []mintRec{mintOf(orgInst, []int64{otherID}, write)}},
		{name: "an id GitHub does not know", repo: pay, meta: withID("999"),
			code: CodeRepoIDMismatch, mints: []mintRec{mintOf(orgInst, []int64{999}, write)}},
		{name: "an id in another installation", repo: pay, meta: withID("5550001"),
			code: CodeRepoIDMismatch, mints: []mintRec{mintOf(orgInst, []int64{userID}, write)}},
		{name: "a deleted and recreated repo", repo: pay, meta: withID("812345678"),
			setup: func(f *fakeGitHub) { f.repos[0].ID = 900000001 },
			code:  CodeRepoIDMismatch, mints: []mintRec{mintOf(orgInst, []int64{payID}, write)}},
		{name: "an id the installation cannot reach, answered 422 as GitHub does", repo: pay, meta: withID("999"),
			setup: func(f *fakeGitHub) {
				f.idMintErrs[999] = &github.StatusError{Method: "POST", Path: "/app/installations/1/access_tokens",
					StatusCode: 422, Status: "422 Unprocessable Entity"}
			},
			code:  CodeRepoIDMismatch,
			mints: []mintRec{mintOf(orgInst, []int64{999}, write), mintOf(orgInst, []int64{999}, meta)}},
		{name: "an unknown repo", repo: "Strazahq/nope", code: CodeNotFound},
		{name: "an id that is a number", repo: pay, meta: withID(812345678), code: CodeInvalidInput},
		{name: "an id with a leading zero", repo: pay, meta: withID("0812345678"), code: CodeInvalidInput},
		{name: "a negative id", repo: pay, meta: withID("-1"), code: CodeInvalidInput},
		{name: "an id of zero", repo: pay, meta: withID("0"), code: CodeInvalidInput},
		{name: "an empty id", repo: pay, meta: withID(""), code: CodeInvalidInput},
		{name: "an id with a space", repo: pay, meta: withID(" 812345678"), code: CodeInvalidInput},
		{name: "an id of null", repo: pay, meta: withID(nil), code: CodeInvalidInput},
		{name: "no owner", repo: "pay-api", code: CodeInvalidInput},
		{name: "a third segment", repo: "Strazahq/pay-api/x", code: CodeInvalidInput},
		{name: "a dot-dot name", repo: "Strazahq/..", code: CodeInvalidInput},
		{name: "a dot name", repo: "Strazahq/.", code: CodeInvalidInput},
		{name: "an underscore in the owner", repo: "Straza_hq/pay-api", code: CodeInvalidInput},
		{name: "a percent-encoded slash", repo: "Strazahq%2Fother/x", code: CodeInvalidInput},
		{name: "a non-ASCII letter", repo: "Strazahq/pay-аpi", code: CodeInvalidInput},
		{name: "an empty repo", repo: "", code: CodeInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeWithRoutes()
			if tc.setup != nil {
				tc.setup(f)
			}
			cs := connectFake(t, f, Config{})
			text, payload := callErr(t, cs, "pr_create", map[string]any{"repo": tc.repo, "title": "x", "head": "fix", "base": "main"}, tc.meta)
			if payload["code"] != tc.code {
				t.Errorf("payload = %v, want %s (text %q)", payload, tc.code, text)
			}
			if tc.text != "" && text != tc.text {
				t.Errorf("text = %q, want %q", text, tc.text)
			}
			mints, calls := f.snapshot()
			want := tc.mints
			if want == nil {
				want = []mintRec{}
			}
			if got := mintsWithout(mints); !reflect.DeepEqual(got, want) {
				t.Errorf("mints = %+v, want %+v", got, want)
			}
			// A refusal never reaches the tool's own call.
			if d := doCalls(calls); len(d) != 0 {
				t.Errorf("a refusal sent %+v", d)
			}
		})
	}
}

// With an id, every token is minted for exactly that id, for every tool, even
// when the name points at another repo: no token ever reaches the repo the
// name resolves to, and none reaches the whole installation.
func TestObjectIDMintsApprovedIDOnly(t *testing.T) {
	for _, c := range allToolCases() {
		t.Run(c.tool, func(t *testing.T) {
			f := fakeWithRoutes()
			cs := connectFake(t, f, Config{})
			// Straza approved Strazahq/other; the call names pay-api.
			_, payload := callErr(t, cs, c.tool, c.args, withID("812345679"))
			if payload["code"] != CodeRepoIDMismatch {
				t.Errorf("payload = %v, want %s", payload, CodeRepoIDMismatch)
			}
			mints, calls := f.snapshot()
			if len(mints) != 1 || !reflect.DeepEqual(mints[0].RepoIDs, []int64{otherID}) {
				t.Errorf("mints = %+v, want one, for the approved id %d only", mintsWithout(mints), otherID)
			}
			if d := doCalls(calls); len(d) != 0 {
				t.Errorf("a refusal sent %+v", d)
			}
		})
	}
}

// The repo of a user account works like an organization's.
func TestAccessUserRepo(t *testing.T) {
	f := newFakeGitHub()
	f.routes["GET /repos/joe/dotfiles/branches"] = `[]`
	cs := connectFake(t, f, Config{})
	out := callOK(t, cs, "branch_list", map[string]any{"repo": "joe/dotfiles"}, withID("5550001"))
	if out["repo"] != "joe/dotfiles" {
		t.Errorf("branch_list = %v", out)
	}
	mints, _ := f.snapshot()
	if got, want := mintsWithout(mints), []mintRec{mintOf(usrInst, []int64{userID}, permContentsRead)}; !reflect.DeepEqual(got, want) {
		t.Errorf("mints = %+v, want %+v", got, want)
	}
}

// Paths are built from GitHub's canonical name with every segment escaped.
func TestRepoPathEscapes(t *testing.T) {
	for _, tc := range []struct {
		full string
		rest []string
		want string
	}{
		{pay, []string{"contents", "docs", "a b#c?.md"}, "/repos/Strazahq/pay-api/contents/docs/a%20b%23c%3F.md"},
		{pay, []string{"contents", "a/b"}, "/repos/Strazahq/pay-api/contents/a%2Fb"},
		{"o/n.x_y", nil, "/repos/o/n.x_y"},
	} {
		if got := repoPath(tc.full, tc.rest...); got != tc.want {
			t.Errorf("repoPath(%q, %q) = %q, want %q", tc.full, tc.rest, got, tc.want)
		}
	}
}
