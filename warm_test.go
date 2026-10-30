package main

import (
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

const recreatedID int64 = 900000001

// payMeta is the _meta of a call by agent A, with Straza's id for pay-api
// when withID.
func payMeta(withStrazaID bool) mcp.Meta {
	if withStrazaID {
		return asAgent(agentA.Client, agentA.Subject, withID(strconv.FormatInt(payID, 10)))
	}
	return asAgent(agentA.Client, agentA.Subject, nil)
}

// The first call spends what every call spent before the caches: 4 GitHub
// calls with Straza's id (installation, mint, id read, the tool's call), 5
// without (installation, metadata mint, name read, mint, the tool's call).
// The same call again spends one, the tool's own, with or without the id.
func TestWarmPathOneGitHubCall(t *testing.T) {
	for _, withStrazaID := range []bool{true, false} {
		for _, c := range allToolCases() {
			name := c.tool + "/with id"
			cold := 4
			if !withStrazaID {
				name, cold = c.tool+"/without id", 5
			}
			t.Run(name, func(t *testing.T) {
				f := fakeWithRoutes()
				s := connectLogged(t, f, Config{})
				callOK(t, s.cs, c.tool, c.args, payMeta(withStrazaID))
				if got := f.gitHubCalls(); len(got) != cold {
					t.Errorf("first call: %d GitHub calls %q, want %d", len(got), got, cold)
				}
				f.reset()
				callOK(t, s.cs, c.tool, c.args, payMeta(withStrazaID))
				if got, want := f.gitHubCalls(), []string{c.op + " " + c.path}; !reflect.DeepEqual(got, want) {
					t.Errorf("warm call: GitHub calls %q, want %q", got, want)
				}
			})
		}
	}
}

// Another agent on a warm repo mints its own token and skips the lookups: 2
// GitHub calls. Every caller gets its own token, and keeps it.
func TestWarmPathPerCallerTokens(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	c := allToolCases()[4] // pr_get
	id := withID(strconv.FormatInt(payID, 10))
	callers := []mcp.Meta{
		asAgent(agentA.Client, agentA.Subject, id),
		asAgent(agentB.Client, agentB.Subject, id),
		asCaller(callerGateway, id),
		id, // local
	}
	tokenOf := map[int]string{}
	for round := range 2 {
		for i, meta := range callers {
			f.reset()
			callOK(t, s.cs, c.tool, c.args, meta)
			got := f.gitHubCalls()
			switch {
			case round == 0 && i == 0:
				// The first call of all is cold.
			case round == 0:
				want := []string{"MintToken 7 [812345678]", c.op + " " + c.path}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("caller %d, first call: %q, want %q", i, got, want)
				}
			default:
				if want := []string{c.op + " " + c.path}; !reflect.DeepEqual(got, want) {
					t.Errorf("caller %d, second call: %q, want %q", i, got, want)
				}
			}
			_, calls := f.snapshot()
			tok := toolCall(t, c, calls).Token
			if round == 0 {
				for j, other := range tokenOf {
					if other == tok {
						t.Errorf("caller %d got caller %d's token", i, j)
					}
				}
				tokenOf[i] = tok
			} else if tokenOf[i] != tok {
				t.Errorf("caller %d got another token on its second call", i)
			}
		}
	}
}

// The lookups last 30 seconds (id to name), 60 seconds (name to id) and 10
// minutes (name to installation). Past each, that one lookup is read again.
func TestLookupTTLs(t *testing.T) {
	c := allToolCases()[4] // pr_get
	tool := c.op + " " + c.path
	for _, tc := range []struct {
		name     string
		strazaID bool
		after    time.Duration
		want     []string
	}{
		{"id within 30 s", true, 29 * time.Second, []string{tool}},
		{"id past 30 s", true, 31 * time.Second, []string{"RepositoryByID 812345678", tool}},
		{"name within 60 s", false, 59 * time.Second, []string{tool}},
		{"name past 60 s", false, 61 * time.Second, []string{"Repository Strazahq/pay-api", tool}},
		{"installation past 10 min", true, 10*time.Minute + time.Second, []string{"InstallationForRepo Strazahq/pay-api", "RepositoryByID 812345678", tool}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeWithRoutes()
			s := connectLogged(t, f, Config{})
			now := t0
			s.setClock(func() time.Time { return now })
			callOK(t, s.cs, c.tool, c.args, payMeta(tc.strazaID))
			f.reset()
			now = t0.Add(tc.after)
			callOK(t, s.cs, c.tool, c.args, payMeta(tc.strazaID))
			if got := f.gitHubCalls(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// Why each cache is safe, shown on the fake GitHub: a repo that moved, was
// renamed or was deleted and created again after the caches filled. Each
// case warms the caches with one call, changes GitHub, and calls again.
func TestStaleEntries(t *testing.T) {
	prGet := allToolCases()[4]
	issueCreate := allToolCases()[10]
	for _, tc := range []struct {
		name     string
		tool     toolCase
		strazaID bool
		caller   mcp.Meta // the second call's caller, when another than agent A
		change   func(*fakeGitHub)
		code     string   // the refusal, or "" for success
		text     string   // the refusal's sentence, when the case checks it
		repoID   string   // the repoId of a write that succeeds
		want     []string // the second call's GitHub calls
	}{
		{
			// A cached installation that lost the repo: the token for it no
			// longer reaches the repo, GitHub answers 404, and the call runs
			// again from cold, on the new installation.
			name: "transfer, with the id", tool: prGet, strazaID: true,
			change: func(f *fakeGitHub) { f.setRepo(payID, func(r *fakeRepo) { r.Installation = 11 }) },
			want: []string{"InstallationForRepo Strazahq/pay-api", "MintToken 11 [812345678]",
				"GET /repos/Strazahq/pay-api/pulls/7", "RepositoryByID 812345678", "GET /repos/Strazahq/pay-api/pulls/7"},
		},
		{
			// Another agent on a cached installation that lost the repo:
			// GitHub answers its mint with 422, and access runs from cold.
			name: "transfer, a new caller's mint answered 422", tool: prGet, strazaID: true,
			caller: asAgent(agentB.Client, agentB.Subject, withID("812345678")),
			change: func(f *fakeGitHub) {
				f.strictMint = true
				f.setRepo(payID, func(r *fakeRepo) { r.Installation = 11 })
			},
			want: []string{"InstallationForRepo Strazahq/pay-api", "MintToken 7 [812345678]", "MintToken 11 [812345678]",
				"RepositoryByID 812345678", "GET /repos/Strazahq/pay-api/pulls/7"},
		},
		{
			// A cached name whose repo was deleted and created again: the
			// old token does not reach the new id, GitHub answers 404, and
			// the retry resolves the new id and writes there.
			name: "recreate, without an id", tool: issueCreate,
			change: func(f *fakeGitHub) { f.setRepo(payID, func(r *fakeRepo) { r.ID = recreatedID }) },
			repoID: "900000001",
			// The cached name token still reaches the old id, so the name
			// is minted for again.
			want: []string{"InstallationForRepo Strazahq/pay-api", "MintTokenForRepo 7 Strazahq/pay-api", "MintToken 7 [900000001]",
				"POST /repos/Strazahq/pay-api/issues", "Repository Strazahq/pay-api", "Repository Strazahq/pay-api", "POST /repos/Strazahq/pay-api/issues"},
		},
		{
			// Straza still approves the old id: the old token reaches no
			// repo under the name, and the retry refuses the id.
			name: "recreate, with the old id", tool: issueCreate, strazaID: true,
			change: func(f *fakeGitHub) { f.setRepo(payID, func(r *fakeRepo) { r.ID = recreatedID }) },
			code:   CodeRepoIDMismatch,
			want: []string{"InstallationForRepo Strazahq/pay-api",
				"POST /repos/Strazahq/pay-api/issues", "RepositoryByID 812345678"},
		},
		{
			// A rename shows as a redirect on the old name, which is refused.
			// No retry: a redirect is never a stale entry's doing.
			name: "rename, with the id", tool: prGet, strazaID: true,
			change: renamePay, code: CodeRepoRenamed, text: "Strazahq/pay-api was renamed on GitHub. Call it by its new name.",
			want: []string{"GET /repos/Strazahq/pay-api/pulls/7"},
		},
		{
			name: "rename, without an id", tool: issueCreate,
			change: renamePay, code: CodeRepoRenamed, text: "Strazahq/pay-api was renamed on GitHub. Call it by its new name.",
			want: []string{"POST /repos/Strazahq/pay-api/issues"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeWithRoutes()
			s := connectLogged(t, f, Config{})
			callOK(t, s.cs, tc.tool.tool, tc.tool.args, payMeta(tc.strazaID))
			tc.change(f)
			f.reset()
			meta := payMeta(tc.strazaID)
			if tc.caller != nil {
				meta = tc.caller
			}
			if tc.code != "" {
				text, payload := callErr(t, s.cs, tc.tool.tool, tc.tool.args, meta)
				if payload["code"] != tc.code {
					t.Errorf("payload = %v, want %s", payload, tc.code)
				}
				if tc.text != "" && text != tc.text {
					t.Errorf("text = %q, want %q", text, tc.text)
				}
			} else {
				out := callOK(t, s.cs, tc.tool.tool, tc.tool.args, meta)
				if tc.repoID != "" && out["repoId"] != tc.repoID {
					t.Errorf("repoId = %v, want %s", out["repoId"], tc.repoID)
				}
			}
			if got := f.gitHubCalls(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("calls = %q\nwant    %q", got, tc.want)
			}
			// Every token any call used reaches the one repo id it was minted
			// for, and nothing else.
			_, calls := f.snapshot()
			for _, c := range calls {
				if m := f.tokens[c.Token]; len(m.RepoIDs) > 1 {
					t.Errorf("%s %s used a token for %v", c.Op, c.Path, m.RepoIDs)
				}
			}
		})
	}
}

func renamePay(f *fakeGitHub) {
	f.setRepo(payID, func(r *fakeRepo) { r.FullName, r.Name = "Strazahq/pay-new", "pay-new" })
	f.redirects["strazahq/pay-api"] = true
	f.routes["GET /repos/Strazahq/pay-new/pulls/7"] = f.routes["GET /repos/Strazahq/pay-api/pulls/7"]
}

// A cached token GitHub refuses with 401, as after a revoke, is replaced:
// the call runs again with a fresh mint.
func TestRevokedTokenIsReplaced(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	c := allToolCases()[4]
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	_, calls := f.snapshot()
	old := toolCall(t, c, calls).Token
	f.mu.Lock()
	delete(f.tokens, old)
	f.mu.Unlock()
	f.reset()
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	want := []string{"InstallationForRepo Strazahq/pay-api", "MintToken 7 [812345678]",
		"GET /repos/Strazahq/pay-api/pulls/7", "RepositoryByID 812345678", "GET /repos/Strazahq/pay-api/pulls/7"}
	if got := f.gitHubCalls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
	f.reset()
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	if got := f.gitHubCalls(); len(got) != 1 {
		t.Errorf("after the new mint: %q, want the tool's call only", got)
	}
}

// Refusals and errors are never cached: the same call is decided fresh each
// time, and a name GitHub spells differently never becomes a cached name.
func TestRefusalsAreNotCached(t *testing.T) {
	f := fakeWithRoutes()
	s := connectLogged(t, f, Config{})
	c := allToolCases()[4]
	variant := map[string]any{"repo": "strazahq/pay-api", "number": 7}
	for range 2 {
		f.reset()
		if _, payload := callErr(t, s.cs, c.tool, variant, payMeta(false)); payload["code"] != CodeRepoNotCanonical {
			t.Errorf("payload = %v", payload)
		}
		// The installation and the metadata token may come from the caches,
		// the read that refuses never does.
		if got := f.gitHubCalls(); !slices.Contains(got, "Repository strazahq/pay-api") {
			t.Errorf("the refusal was decided from a cache: %q", got)
		}
	}
	if _, ok := s.tools.repos.names.get("strazahq/pay-api", time.Now()); ok {
		t.Error("a name that is not canonical is cached")
	}

	f.errs["MintToken"] = &github.StatusError{StatusCode: 502, Status: "502 Bad Gateway"}
	if _, payload := callErr(t, s.cs, c.tool, c.args, payMeta(true)); payload["code"] != CodeGitHubUnavailable {
		t.Errorf("payload = %v", payload)
	}
	delete(f.errs, "MintToken")
	f.reset()
	callOK(t, s.cs, c.tool, c.args, payMeta(true))
	if got := f.gitHubCalls(); len(got) != 3 || got[0] != "MintToken 7 [812345678]" {
		t.Errorf("after a failed mint: %q, want a mint, the id read and the call", got)
	}
}
