package github

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestInvalidInputRefusedWithoutRequest(t *testing.T) {
	f := newFake(t)
	f.handle("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused argument reached GitHub: %s %s", r.Method, r.URL.EscapedPath())
	})
	app := newTestApp(t, f)
	ctx := t.Context()
	tok := testTok()

	type call struct {
		name string
		run  func() error
	}
	var calls []call

	badFullNames := []string{
		"", "pay-api", "/", "Strazahq/", "/pay-api", "Strazahq//pay-api", "Strazahq/pay-api/",
		"Strazahq/pay-api/pulls", "Strazahq/..", "../pay-api", "Strazahq/a..b", "Strazahq/.",
		".", "..", "Strazahq/pay api", "Straza hq/pay-api", "Strazahq/pay%2fapi", "Strazahq/pay-api?x=1",
		"Strazahq/pay-api#x", "Straza.hq/pay-api", "Strazahq/pay\napi", "Strazahq/pāy", "Strazahq\\pay-api",
		"Strazahq/" + strings.Repeat("a", 101), strings.Repeat("o", 101) + "/pay-api",
	}
	for _, name := range badFullNames {
		calls = append(calls,
			call{"InstallationForRepo " + quote(name), func() error { _, err := app.InstallationForRepo(ctx, name); return err }},
			call{"Repository " + quote(name), func() error { _, err := app.Repository(ctx, tok, name); return err }},
		)
	}
	for _, org := range []string{"", "Straza/hq", "..", ".", "Straza.hq", "Straza hq", "Straza%2Fhq", strings.Repeat("o", 101)} {
		calls = append(calls,
			call{"PropertySchema " + quote(org), func() error { _, err := app.PropertySchema(ctx, tok, org); return err }},
			call{"PropertyValues " + quote(org), func() error { _, _, err := app.PropertyValues(ctx, tok, org, 1); return err }},
		)
	}
	for _, path := range []string{
		"", "repos/a/b", "//evil.example/x", "https://evil.example/x", "/repos/a/../b", "/repos/a/./b",
		"/repos/a/%2e%2e/b", "/repos/a/%2E%2e", "/repos/a/b%2F..%2Fc", "/repos/a/b%5C..", "/repos/a//b", "/repos/a/b/",
		"/repos/a/b?per_page=1", "/repos/a/b#x", "/repos/a b", "/repos/a\\b", "/repos/a\nb", "/repos/ā", "/repos/%zz",
		"/repos/a/%00",
	} {
		calls = append(calls, call{"Do path " + quote(path), func() error {
			return app.Do(ctx, tok, http.MethodGet, path, nil, nil, nil)
		}})
	}
	for _, method := range []string{"", "get", "TRACE", "CONNECT", "GET /x HTTP/1.1"} {
		calls = append(calls, call{"Do method " + quote(method), func() error {
			return app.Do(ctx, tok, method, "/x", nil, nil, nil)
		}})
	}
	calls = append(calls,
		call{"Do with an empty token", func() error { return app.Do(ctx, Token{}, http.MethodGet, "/x", nil, nil, nil) }},
		call{"Do with a token holding a line break", func() error {
			return app.Do(ctx, Token{Value: "ghs_a\r\nX-Evil: 1"}, http.MethodGet, "/x", nil, nil, nil)
		}},
		call{"Do with the token in the path", func() error { return app.Do(ctx, tok, http.MethodGet, "/x/"+testToken, nil, nil, nil) }},
		call{"Repository with an empty token", func() error { _, err := app.Repository(ctx, Token{}, "a/b"); return err }},
		call{"RepositoryByID 0", func() error { _, err := app.RepositoryByID(ctx, tok, 0); return err }},
		call{"RepositoryByID -1", func() error { _, err := app.RepositoryByID(ctx, tok, -1); return err }},
		call{"InstallationRepos page 0", func() error { _, _, err := app.InstallationRepos(ctx, tok, 0); return err }},
		call{"InstallationRepos page too large", func() error { _, _, err := app.InstallationRepos(ctx, tok, maxPage+1); return err }},
		call{"PropertyValues page 0", func() error { _, _, err := app.PropertyValues(ctx, tok, "Strazahq", 0); return err }},
		call{"MintToken nil perms", func() error { _, err := app.MintToken(ctx, 1, []int64{2}, nil); return err }},
		call{"MintToken empty perms", func() error { _, err := app.MintToken(ctx, 1, nil, Permissions{}); return err }},
		call{"MintToken bad level", func() error { _, err := app.MintToken(ctx, 1, nil, Permissions{"contents": "all"}); return err }},
		call{"MintToken bad name", func() error { _, err := app.MintToken(ctx, 1, nil, Permissions{"Contents": "read"}); return err }},
		call{"MintToken empty name", func() error { _, err := app.MintToken(ctx, 1, nil, Permissions{"": "read"}); return err }},
		call{"MintToken installation 0", func() error { _, err := app.MintToken(ctx, 0, nil, Permissions{"contents": "read"}); return err }},
		call{"MintToken repository 0", func() error {
			_, err := app.MintToken(ctx, 1, []int64{5, 0}, Permissions{"contents": "read"})
			return err
		}},
	)

	for _, c := range calls {
		err := c.run()
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %s, want ErrInvalidInput", c.name, safe(err))
		}
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("%s: the error text holds the token", c.name)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests reached GitHub, want none", n)
	}
}

func TestValidNamesAccepted(t *testing.T) {
	f := newFake(t)
	f.handle("GET /repos/{o}/{r}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 1, "full_name": r.PathValue("o") + "/" + r.PathValue("r")})
	})
	app := newTestApp(t, f)
	for _, name := range []string{
		"Strazahq/pay-api", "Strazahq/.github", "octocat/dot.files", "a_b/c_d", "EMU_user_short/x-1",
		"o/" + strings.Repeat("r", 100), strings.Repeat("o", 100) + "/r",
	} {
		r, err := app.Repository(t.Context(), testTok(), name)
		if err != nil {
			t.Errorf("Repository(%q): %s", name, safe(err))
			continue
		}
		if r.FullName != name {
			t.Errorf("Repository(%q) reached %q", name, r.FullName)
		}
	}
}

func TestRepoPath(t *testing.T) {
	cases := []struct {
		fullName string
		segments []string
		want     string
	}{
		{"Strazahq/pay-api", nil, "/repos/Strazahq/pay-api"},
		{"Strazahq/pay-api", []string{"pulls", "12"}, "/repos/Strazahq/pay-api/pulls/12"},
		{"Strazahq/pay-api", []string{"branches", "feature/x"}, "/repos/Strazahq/pay-api/branches/feature%2Fx"},
		{"Strazahq/pay-api", []string{"contents", "docs", "a b?.md"}, "/repos/Strazahq/pay-api/contents/docs/a%20b%3F.md"},
		{"Strazahq/pay-api", []string{"compare", "main...feature"}, "/repos/Strazahq/pay-api/compare/main...feature"},
	}
	for _, tc := range cases {
		got, err := RepoPath(tc.fullName, tc.segments...)
		if err != nil {
			t.Errorf("RepoPath(%q, %q): %v", tc.fullName, tc.segments, err)
			continue
		}
		if got != tc.want {
			t.Errorf("RepoPath(%q, %q) = %q, want %q", tc.fullName, tc.segments, got, tc.want)
		}
		if err := checkPath(got); err != nil {
			t.Errorf("checkPath refuses RepoPath's %q: %v", got, err)
		}
	}
	if _, err := RepoPath("Strazahq/a..b"); err == nil || !strings.Contains(err.Error(), `holds ".."`) {
		t.Errorf(`RepoPath("Strazahq/a..b"): %v, want a refusal that names ".."`, err)
	}
	for _, segs := range [][]string{{""}, {"."}, {".."}, {"pulls", ""}} {
		if _, err := RepoPath("Strazahq/pay-api", segs...); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("RepoPath with segments %q: %v, want ErrInvalidInput", segs, err)
		}
	}
}
