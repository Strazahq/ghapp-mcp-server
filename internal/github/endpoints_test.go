package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInstallationForRepo(t *testing.T) {
	f := newFake(t)
	f.handle("GET /repos/{o}/{r}/installation", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("o") != "Strazahq" || r.PathValue("r") != "pay-api" {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 4242, "account": map[string]string{"login": "Strazahq", "type": "Organization"}})
	})
	app := newTestApp(t, f)

	id, err := app.InstallationForRepo(t.Context(), "Strazahq/pay-api")
	if err != nil {
		t.Fatalf("InstallationForRepo: %s", safe(err))
	}
	if id != 4242 {
		t.Errorf("id %d, want 4242", id)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/repos/Strazahq/pay-api/installation" {
		t.Fatalf("%d requests, want one GET /repos/Strazahq/pay-api/installation", len(reqs))
	}
	if verifyJWT(t, reqs[0].Auth).Issuer != testAppID {
		t.Error("the call is not signed in as the App")
	}

	if _, err := app.InstallationForRepo(t.Context(), "Strazahq/other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown repository: %s, want ErrNotFound", safe(err))
	}
}

func TestMintTokenBody(t *testing.T) {
	expires := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		repoIDs  []int64
		perms    Permissions
		wantBody string
	}{
		{
			name:     "one repository",
			repoIDs:  []int64{812345678},
			perms:    Permissions{"pull_requests": "write", "contents": "read"},
			wantBody: `{"repository_ids":[812345678],"permissions":{"contents":"read","pull_requests":"write"}}`,
		},
		{
			name:     "two repositories",
			repoIDs:  []int64{1, 2},
			perms:    Permissions{"issues": "read"},
			wantBody: `{"repository_ids":[1,2],"permissions":{"issues":"read"}}`,
		},
		{
			name:     "no repository ids",
			perms:    Permissions{"contents": "read"},
			wantBody: `{"permissions":{"contents":"read"}}`,
		},
		{
			name:     "an empty list of repository ids",
			repoIDs:  []int64{},
			perms:    Permissions{"metadata": "read"},
			wantBody: `{"permissions":{"metadata":"read"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.handle("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusCreated, map[string]any{
					"token": testToken, "expires_at": expires.Format(time.RFC3339),
					"permissions": tc.perms, "repository_selection": "selected",
				})
			})
			app := newTestApp(t, f)

			tok, err := app.MintToken(t.Context(), 4242, tc.repoIDs, tc.perms)
			if err != nil {
				t.Fatalf("MintToken: %s", safe(err))
			}
			if tok.Value != testToken {
				t.Error("the token value is not GitHub's")
			}
			if !tok.ExpiresAt.Equal(expires) || tok.InstallationID != 4242 {
				t.Errorf("token %v, want installation 4242 expiring %v", tok, expires)
			}

			reqs := f.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			req := reqs[0]
			if req.Method != http.MethodPost || req.Path != "/app/installations/4242/access_tokens" {
				t.Errorf("request %s %s, want POST /app/installations/4242/access_tokens", req.Method, req.Path)
			}
			if string(req.Body) != tc.wantBody {
				t.Errorf("body\n %s\nwant\n %s", req.Body, tc.wantBody)
			}
			if ct := req.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q, want application/json", ct)
			}
			verifyJWT(t, req.Auth)
		})
	}
}

// MintTokenForRepo sends the name without its owner, and the token keeps
// the repository GitHub names in its answer, as GitHub spells it.
func TestMintTokenForRepo(t *testing.T) {
	f := newFake(t)
	f.handle("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": testToken, "expires_at": "2026-10-19T13:00:00Z",
			"repositories": []map[string]any{{"id": 812345678, "name": "pay-api", "full_name": "Strazahq/pay-api"}},
		})
	})
	app := newTestApp(t, f)
	tok, err := app.MintTokenForRepo(t.Context(), 4242, "Strazahq/PAY-API", Permissions{"metadata": "read"})
	if err != nil {
		t.Fatalf("MintTokenForRepo: %s", safe(err))
	}
	if want := []Repo{{ID: 812345678, FullName: "Strazahq/pay-api", Name: "pay-api"}}; len(tok.Repos) != 1 ||
		tok.Repos[0].ID != want[0].ID || tok.Repos[0].FullName != want[0].FullName {
		t.Errorf("Repos = %+v, want %+v", tok.Repos, want)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	if want := `{"repositories":["PAY-API"],"permissions":{"metadata":"read"}}`; string(reqs[0].Body) != want {
		t.Errorf("body\n %s\nwant\n %s", reqs[0].Body, want)
	}

	for _, bad := range []string{"pay-api", "Strazahq/", "Strazahq/a/b", "Strazahq/../x"} {
		if _, err := app.MintTokenForRepo(t.Context(), 4242, bad, Permissions{"metadata": "read"}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("MintTokenForRepo(%q): %v, want ErrInvalidInput", bad, err)
		}
	}
	if _, err := app.MintTokenForRepo(t.Context(), 4242, "Strazahq/pay-api", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("no permissions: %v, want ErrInvalidInput", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests after refusals, want still 1", n)
	}
}

func TestMintTokenBadAnswer(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no expires_at", `{"token":"` + testToken + `"}`},
		{"broken expires_at", `{"token":"` + testToken + `","expires_at":"soon"}`},
		{"token not a string", `{"token":12,"expires_at":"2026-10-18T13:00:00Z"}`},
		{"no token", `{"expires_at":"2026-10-18T13:00:00Z"}`},
		{"not JSON", `token=` + testToken},
		{"empty", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.handle("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(tc.body))
			})
			app := newTestApp(t, f)
			tok, err := app.MintToken(t.Context(), 1, nil, Permissions{"contents": "read"})
			if err == nil {
				t.Fatal("a broken token answer succeeded")
			}
			if tok.Value != "" {
				t.Error("a failed mint returned a token value")
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("the error text holds the token")
			}
			if !strings.Contains(err.Error(), "POST /app/installations/1/access_tokens") {
				t.Errorf("error %q does not name the call", safe(err))
			}
		})
	}
}

func TestRepository(t *testing.T) {
	answer := map[string]any{
		"id": 812345678, "full_name": "Strazahq/pay-api", "name": "pay-api",
		"owner":   map[string]any{"login": "Strazahq", "type": "Organization", "id": 9},
		"private": true, "archived": false, "default_branch": "main",
		"html_url": "https://github.com/Strazahq/pay-api",
	}
	want := Repo{
		ID: 812345678, FullName: "Strazahq/pay-api", Name: "pay-api", Owner: "Strazahq",
		OwnerType: "Organization", Private: true, DefaultBranch: "main",
		HTMLURL: "https://github.com/Strazahq/pay-api",
	}
	f := newFake(t)
	f.handle("GET /repos/Strazahq/pay-api", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, answer) })
	f.handle("GET /repositories/812345678", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, answer) })
	f.handle("GET /repositories/1", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]any{}) })
	app := newTestApp(t, f)

	got, err := app.Repository(t.Context(), testTok(), "Strazahq/pay-api")
	if err != nil {
		t.Fatalf("Repository: %s", safe(err))
	}
	if got != want {
		t.Errorf("Repository\n %+v\nwant\n %+v", got, want)
	}
	got, err = app.RepositoryByID(t.Context(), testTok(), 812345678)
	if err != nil {
		t.Fatalf("RepositoryByID: %s", safe(err))
	}
	if got != want {
		t.Errorf("RepositoryByID\n %+v\nwant\n %+v", got, want)
	}
	for _, r := range f.requests() {
		if r.Auth != "Bearer "+testToken {
			t.Errorf("%s %s is not made with the installation token", r.Method, r.Path)
		}
	}
	if _, err := app.RepositoryByID(t.Context(), testTok(), 1); err == nil || !strings.Contains(err.Error(), "no repository id") {
		t.Errorf("an answer without an id: %s, want a refusal", safe(err))
	}
}

func TestInstallationsPagination(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the client followed a Link header to another server")
	}))
	defer evil.Close()

	const total = 250
	f := newFake(t)
	f.handle("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var batch []map[string]any
		for i := (page - 1) * 100; i < min(page*100, total); i++ {
			acct := map[string]any{"login": fmt.Sprintf("org%d", i), "type": "Organization"}
			if i == 0 {
				acct = map[string]any{"login": "octocat", "type": "User"}
			}
			batch = append(batch, map[string]any{"id": i + 1, "account": acct, "target_type": "Organization"})
		}
		if page*100 < total {
			w.Header().Set("Link", fmt.Sprintf(`<%s/app/installations?per_page=100&page=%d>; rel="next", <%s/app/installations?per_page=100&page=3>; rel="last"`, evil.URL, page+1, evil.URL))
		}
		writeJSON(w, http.StatusOK, batch)
	})
	app := newTestApp(t, f)

	got, err := app.Installations(t.Context())
	if err != nil {
		t.Fatalf("Installations: %s", safe(err))
	}
	if len(got) != total {
		t.Fatalf("%d installations, want %d", len(got), total)
	}
	if got[0] != (Installation{ID: 1, Account: "octocat", AccountType: "User"}) {
		t.Errorf("first %+v, want the user installation", got[0])
	}
	if got[249] != (Installation{ID: 250, Account: "org249", AccountType: "Organization"}) {
		t.Errorf("last %+v", got[249])
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for i, r := range reqs {
		if want := fmt.Sprintf("page=%d&per_page=100", i+1); r.Query != want {
			t.Errorf("request %d query %q, want %q", i+1, r.Query, want)
		}
		verifyJWT(t, r.Auth)
	}
}

func TestInstallationsEnterprise(t *testing.T) {
	f := newFake(t)
	f.handle("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 3, "account": map[string]any{"slug": "straza-ent", "name": "Straza"}, "target_type": "Enterprise"},
		})
	})
	got, err := newTestApp(t, f).Installations(t.Context())
	if err != nil {
		t.Fatalf("Installations: %s", safe(err))
	}
	want := []Installation{{ID: 3, Account: "straza-ent", AccountType: "Enterprise"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestInstallationReposPagination(t *testing.T) {
	const total = 250
	f := newFake(t)
	f.handle("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var repos []map[string]any
		for i := (page - 1) * 100; i < min(page*100, total); i++ {
			repos = append(repos, map[string]any{
				"id": 1000 + i, "full_name": fmt.Sprintf("Strazahq/r%d", i), "name": fmt.Sprintf("r%d", i),
				"owner": map[string]any{"login": "Strazahq", "type": "Organization"},
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": total, "repositories": repos})
	})
	app := newTestApp(t, f)

	cases := []struct {
		page     int
		wantLen  int
		wantMore bool
	}{
		{1, 100, true},
		{2, 100, true},
		{3, 50, false},
		{4, 0, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("page %d", tc.page), func(t *testing.T) {
			repos, more, err := app.InstallationRepos(t.Context(), testTok(), tc.page)
			if err != nil {
				t.Fatalf("InstallationRepos: %s", safe(err))
			}
			if len(repos) != tc.wantLen || more != tc.wantMore {
				t.Errorf("%d repos, more %v; want %d, %v", len(repos), more, tc.wantLen, tc.wantMore)
			}
			if len(repos) > 0 {
				first := repos[0]
				i := (tc.page - 1) * 100
				if first.ID != int64(1000+i) || first.Owner != "Strazahq" || first.OwnerType != "Organization" {
					t.Errorf("first repo %+v", first)
				}
			}
		})
	}
	for i, r := range f.requests() {
		if want := fmt.Sprintf("page=%d&per_page=100", i+1); r.Query != want {
			t.Errorf("request %d query %q, want %q", i+1, r.Query, want)
		}
		if r.Auth != "Bearer "+testToken {
			t.Errorf("request %d is not made with the installation token", i+1)
		}
	}
}

func TestPropertySchema(t *testing.T) {
	f := newFake(t)
	f.handle("GET /orgs/Strazahq/properties/schema", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"property_name": "team", "value_type": "single_select", "values_editable_by": "org_actors", "allowed_values": []string{"payments"}},
			{"property_name": "env", "value_type": "multi_select", "values_editable_by": "org_and_repo_actors"},
			{"property_name": "legacy", "value_type": "string"},
		})
	})
	got, err := newTestApp(t, f).PropertySchema(t.Context(), testTok(), "Strazahq")
	if err != nil {
		t.Fatalf("PropertySchema: %s", safe(err))
	}
	want := []PropertyDef{
		{Name: "team", ValuesEditableBy: "org_actors"},
		{Name: "env", ValuesEditableBy: "org_and_repo_actors"},
		{Name: "legacy"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPropertyValues(t *testing.T) {
	f := newFake(t)
	f.handle("GET /orgs/Strazahq/properties/values", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", `<https://elsewhere.example/orgs/Strazahq/properties/values?page=2>; rel="next"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"repository_id": 812345678, "repository_name": "pay-api", "repository_full_name": "Strazahq/pay-api",
				 "properties": [
					{"property_name": "team", "value": "payments"},
					{"property_name": "env", "value": ["prod", "eu"]},
					{"property_name": "owner", "value": null},
					{"property_name": "tags", "value": []},
					{"property_name": "note", "value": ""},
					{"property_name": "missing"}
				 ]},
				{"repository_id": 2, "repository_name": "docs", "repository_full_name": "Strazahq/docs", "properties": []}
			]`))
		case "2":
			writeJSON(w, http.StatusOK, []map[string]any{{
				"repository_id": 3, "repository_full_name": "Strazahq/web",
				"properties": []map[string]any{{"property_name": "team", "value": "web"}},
			}})
		default:
			writeJSON(w, http.StatusOK, []any{})
		}
	})
	app := newTestApp(t, f)

	got, more, err := app.PropertyValues(t.Context(), testTok(), "Strazahq", 1)
	if err != nil {
		t.Fatalf("PropertyValues: %s", safe(err))
	}
	want := []RepoProperties{
		{RepoID: 812345678, FullName: "Strazahq/pay-api", Values: map[string][]string{
			"team":    {"payments"},
			"env":     {"prod", "eu"},
			"owner":   {},
			"tags":    {},
			"note":    {""},
			"missing": {},
		}},
		{RepoID: 2, FullName: "Strazahq/docs", Values: map[string][]string{}},
	}
	if !reflect.DeepEqual(got, want) || !more {
		t.Errorf("page 1\n %+v more %v\nwant\n %+v more true", got, more, want)
	}

	got, more, err = app.PropertyValues(t.Context(), testTok(), "Strazahq", 2)
	if err != nil {
		t.Fatalf("PropertyValues page 2: %s", safe(err))
	}
	if len(got) != 1 || got[0].RepoID != 3 || more {
		t.Errorf("page 2 %+v more %v, want one repo and no more", got, more)
	}
	got, more, err = app.PropertyValues(t.Context(), testTok(), "Strazahq", 3)
	if err != nil || len(got) != 0 || more {
		t.Errorf("page 3 %+v more %v err %s, want an empty last page", got, more, safe(err))
	}
	for i, r := range f.requests() {
		if want := fmt.Sprintf("page=%d&per_page=100", i+1); r.Query != want {
			t.Errorf("request %d query %q, want %q", i+1, r.Query, want)
		}
	}
}

func TestPropertyValuesRefusesOtherTypes(t *testing.T) {
	cases := []string{`42`, `true`, `{"a":"b"}`, `["a", null]`, `["a", 1]`, `[["a"]]`}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			f := newFake(t)
			f.handle("GET /orgs/Strazahq/properties/values", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`[{"repository_id": 5, "repository_full_name": "Strazahq/x",
					"properties": [{"property_name": "team", "value": ` + value + `}]}]`))
			})
			_, _, err := newTestApp(t, f).PropertyValues(t.Context(), testTok(), "Strazahq", 1)
			if err == nil || !strings.Contains(err.Error(), `property "team" of repository 5`) {
				t.Errorf("value %s: %s, want a refusal naming the property", value, safe(err))
			}
		})
	}
}

func TestHasNext(t *testing.T) {
	cases := []struct {
		link string
		want bool
	}{
		{``, false},
		{`<https://api.github.com/x?page=2>; rel="next"`, true},
		{`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=5>; rel="last"`, true},
		{`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=1>; rel="first"`, false},
		{`<https://api.github.com/x?page=1>; rel="first", <https://api.github.com/x?page=3>; rel="next"`, true},
		{`<https://api.github.com/x?a=1,2>; rel="last"`, false},
		{`<https://api.github.com/x?page=2>; REL=NEXT`, true},
		{`<https://api.github.com/x?page=2>; rel="prefetch next"`, true},
		{`<https://api.github.com/next>; rel="last"`, false},
		{`garbage`, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.link != "" {
			h.Set("Link", tc.link)
		}
		if got := hasNext(h); got != tc.want {
			t.Errorf("hasNext(%q) = %v, want %v", tc.link, got, tc.want)
		}
	}
}
