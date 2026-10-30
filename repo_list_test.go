package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// listingFake has three installations, listed out of order, two repos a
// page, and custom properties on Strazahq.
func listingFake() *fakeGitHub {
	f := newFakeGitHub()
	f.pageSize = 2
	f.installations = append(f.installations, github.Installation{ID: 9, Account: "acme", AccountType: "Organization"})
	f.installations[0], f.installations[2] = f.installations[2], f.installations[0] // 9, 3, 7
	f.repos = append(f.repos,
		fakeRepo{Repo: repo(812345680, "Strazahq", "web", "Organization"), Installation: orgInst},
		fakeRepo{Repo: repo(42, "acme", "x", "Organization"), Installation: 9},
	)
	f.values["Strazahq"] = []github.RepoProperties{
		{RepoID: payID, FullName: pay, Values: map[string][]string{
			"team": {"payments"}, "env": {"prod", "eu"}, "secret": {"not configured"}}},
		{RepoID: otherID, FullName: "Strazahq/other", Values: map[string][]string{
			"team": {strings.Repeat("t", 200)},
			"env":  strings.Split(strings.Repeat("e,", 16)+"e", ",")}}, // 17 values
		{RepoID: 812345680, FullName: "Strazahq/web", Values: map[string][]string{
			"team": {strings.Repeat("w", 128)}, "env": strings.Split(strings.Repeat("e,", 15)+"e", ",")}}, // at the limits
	}
	f.schemas["Strazahq"] = []github.PropertyDef{{Name: "team", ValuesEditableBy: "org_actors"}, {Name: "env"}}
	f.schemas["acme"] = []github.PropertyDef{{Name: "team"}}
	return f
}

// logRec collects what the lister logs.
type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// listingSession serves repo_list alone, as main builds it, with a clock and
// a log of its own.
func listingSession(t *testing.T, f *fakeGitHub, attrs []string, now *time.Time) (*mcp.ClientSession, *logRec) {
	t.Helper()
	logs := &logRec{}
	l := newRepoLister(f, newTokenCache(f, newAuditLogger(io.Discard)), attrs)
	l.logf = logs.logf
	if now != nil {
		l.now = func() time.Time { return *now }
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "t"}, nil)
	registerRepoList(server, l)
	server.AddReceivingMiddleware(errorCodes)
	return connect(t, server), logs
}

func objectsOf(out map[string]any) []map[string]any {
	var objs []map[string]any
	list, _ := out["objects"].([]any)
	for _, o := range list {
		objs = append(objs, o.(map[string]any))
	}
	return objs
}

// Case 10 and 12: repo_list walks the installations by id, one page of one
// installation a call, until nextCursor is absent; ids are strings, and each
// installation's token reaches the installation with metadata read only,
// plus custom properties for an organization when attributes are set.
func TestRepoListPaging(t *testing.T) {
	f := listingFake()
	cs, _ := listingSession(t, f, []string{"team"}, nil)
	f.reset() // the schema reads at build time

	var pages [][]string
	cursor := ""
	for range 10 {
		var args map[string]any
		if cursor != "" {
			args = map[string]any{"cursor": cursor}
		}
		out := callOK(t, cs, "repo_list", args, nil)
		var page []string
		for _, o := range objectsOf(out) {
			page = append(page, o["value"].(string)+"="+o["id"].(string))
		}
		pages = append(pages, page)
		next, more := out["nextCursor"].(string)
		if !more {
			break
		}
		cursor = next
	}
	want := [][]string{
		{"joe/dotfiles=5550001"},
		{"Strazahq/pay-api=812345678", "Strazahq/other=812345679"},
		{"Strazahq/web=812345680"},
		{"acme/x=42"},
	}
	if !reflect.DeepEqual(pages, want) {
		t.Errorf("pages = %v, want %v", pages, want)
	}

	// Each page's token reaches its whole installation with metadata read,
	// plus custom properties for an organization. Tokens are reused: the
	// schema read at build time already minted the organizations' tokens
	// for this caller, so only the user account's is new, and the two
	// pages of Strazahq share one.
	mints, calls := f.snapshot()
	props := github.Permissions{"metadata": "read", permCustomProperties: "read"}
	if got, want := mintsWithout(mints), []mintRec{mintOf(usrInst, nil, github.Permissions{"metadata": "read"})}; !reflect.DeepEqual(got, want) {
		t.Errorf("mints = %+v, want %+v", got, want)
	}
	wantPerms := map[int64]github.Permissions{usrInst: {"metadata": "read"}, orgInst: props, 9: props}
	used := map[string]bool{}
	for _, c := range calls {
		if c.Op != "InstallationRepos" {
			continue
		}
		m := f.tokens[c.Token]
		if m.RepoIDs != nil || !reflect.DeepEqual(m.Perms, wantPerms[m.Installation]) {
			t.Errorf("page %s used a token minted as %+v", c.Path, m)
		}
		used[c.Token] = true
	}
	if len(used) != 3 {
		t.Errorf("the 4 pages used %d tokens, want 3, one per installation", len(used))
	}
}

// Without attributes, no token asks for custom properties and none are read.
func TestRepoListWithoutAttributes(t *testing.T) {
	f := listingFake()
	cs, _ := listingSession(t, f, nil, nil)
	if mints, _ := f.snapshot(); len(mints) != 0 {
		t.Errorf("building the server minted %+v", mintsWithout(mints))
	}
	out := callOK(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: orgInst, Page: 1}.encode()}, nil)
	for _, o := range objectsOf(out) {
		if attrs := o["attributes"].(map[string]any); len(attrs) != 0 {
			t.Errorf("%s attributes = %v, want none", o["value"], attrs)
		}
	}
	mints, calls := f.snapshot()
	if got, want := mintsWithout(mints), []mintRec{mintOf(orgInst, nil, github.Permissions{"metadata": "read"})}; !reflect.DeepEqual(got, want) {
		t.Errorf("mints = %+v, want %+v", got, want)
	}
	for _, c := range calls {
		if c.Op == "PropertyValues" || c.Op == "PropertySchema" {
			t.Errorf("read properties without attributes: %+v", c)
		}
	}
	tools := listTools(t, cs)
	lists := tools["repo_list"].Meta[metaListsObjects].(map[string]any)
	if attrs, _ := lists["attributes"].([]any); attrs == nil || len(attrs) != 0 {
		t.Errorf("lists-objects attributes = %v, want []", lists["attributes"])
	}
}

// Case 12, 13 and 13a: attributes come only from the configured properties,
// a multi-select is a list, and a value over the limits is dropped and
// logged while the listing succeeds.
func TestRepoListAttributes(t *testing.T) {
	f := listingFake()
	cs, logs := listingSession(t, f, []string{"team", "env"}, nil)
	out := callOK(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: orgInst, Page: 1}.encode()}, nil)
	objs := objectsOf(out)
	if len(objs) != 2 {
		t.Fatalf("objects = %v", objs)
	}
	if got, want := objs[0]["attributes"], map[string]any{"team": "payments", "env": []any{"prod", "eu"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("pay-api attributes = %v, want %v", got, want)
	}
	if got := objs[1]["attributes"]; !reflect.DeepEqual(got, map[string]any{}) {
		t.Errorf("other attributes = %v, want both dropped", got)
	}
	if len(logs.lines) != 2 || !strings.Contains(logs.lines[0]+logs.lines[1], "200 bytes") || !strings.Contains(logs.lines[0]+logs.lines[1], "17 values") {
		t.Errorf("logs = %q, want one line per dropped value", logs.lines)
	}

	// At the limits, the values stay.
	out = callOK(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: orgInst, Page: 2}.encode()}, nil)
	attrs := objectsOf(out)[0]["attributes"].(map[string]any)
	if attrs["team"] != strings.Repeat("w", 128) || len(attrs["env"].([]any)) != 16 {
		t.Errorf("web attributes = %v", attrs)
	}

	// A user account's repo has no custom properties.
	out = callOK(t, cs, "repo_list", nil, nil)
	if o := objectsOf(out)[0]; o["value"] != "joe/dotfiles" || len(o["attributes"].(map[string]any)) != 0 {
		t.Errorf("user repo = %v", o)
	}
}

// An organization's values are read in full, every page, once a minute.
func TestRepoListPropertyCache(t *testing.T) {
	f := listingFake()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cs, _ := listingSession(t, f, []string{"team"}, &now)
	reads := func() []string {
		_, calls := f.snapshot()
		var out []string
		for _, c := range calls {
			if c.Op == "PropertyValues" {
				out = append(out, c.Path)
			}
		}
		return out
	}
	page1 := map[string]any{"cursor": listCursor{Installation: orgInst, Page: 1}.encode()}
	page2 := map[string]any{"cursor": listCursor{Installation: orgInst, Page: 2}.encode()}

	callOK(t, cs, "repo_list", page1, nil)
	if got := reads(); !reflect.DeepEqual(got, []string{"Strazahq/1", "Strazahq/2"}) {
		t.Errorf("reads = %v, want both pages", got)
	}
	now = now.Add(59 * time.Second)
	callOK(t, cs, "repo_list", page2, nil)
	if got := reads(); len(got) != 2 {
		t.Errorf("reads within a minute = %v, want the cached values", got)
	}
	now = now.Add(2 * time.Second)
	callOK(t, cs, "repo_list", page2, nil)
	if got := reads(); len(got) != 4 {
		t.Errorf("reads after a minute = %v, want a fresh read", got)
	}
}

// A failed property read fails the whole listing, so Straza keeps its last
// good list.
func TestRepoListPropertyFailure(t *testing.T) {
	f := listingFake()
	f.valuesErr["Strazahq"] = &github.StatusError{Method: "GET", Path: "/orgs/Strazahq/properties/values", StatusCode: 502, Status: "502 Bad Gateway"}
	cs, _ := listingSession(t, f, []string{"team"}, nil)
	_, payload := callErr(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: orgInst, Page: 1}.encode()}, nil)
	if payload["code"] != CodeGitHubUnavailable {
		t.Errorf("payload = %v", payload)
	}
	// The user account's page needs no properties.
	callOK(t, cs, "repo_list", nil, nil)
}

// A cursor repo_list did not make is invalid input. A cursor whose
// installation is gone goes on with the next one.
func TestRepoListCursor(t *testing.T) {
	f := listingFake()
	cs, _ := listingSession(t, f, nil, nil)
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, c := range []string{
		"!!!",
		"p2",
		b64(`not json`),
		b64(`{"i":0,"p":1}`),
		b64(`{"i":7,"p":0}`),
		b64(`{"i":7,"p":1,"x":1}`),
		b64(`{"i":7,"p":1}{}`),
		b64(`{"i":"7","p":1}`),
	} {
		_, payload := callErr(t, cs, "repo_list", map[string]any{"cursor": c}, nil)
		if payload["code"] != CodeInvalidInput || payload["field"] != "cursor" {
			t.Errorf("cursor %q: payload %v", c, payload)
		}
	}
	out := callOK(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: 8, Page: 3}.encode()}, nil)
	if objs := objectsOf(out); len(objs) != 1 || objs[0]["value"] != "acme/x" {
		t.Errorf("after a gone installation = %v, want acme/x", objs)
	}
	if _, more := out["nextCursor"]; more {
		t.Errorf("nextCursor on the last page: %v", out["nextCursor"])
	}
	out = callOK(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: 10, Page: 1}.encode()}, nil)
	if objs := objectsOf(out); len(objs) != 0 || out["nextCursor"] != nil {
		t.Errorf("past the last installation = %v, want an empty last page", out)
	}
}

// A title is the name cut to 256 bytes at a character boundary.
func TestRepoListTitle(t *testing.T) {
	f := newFakeGitHub()
	long := strings.Repeat("é", 200) // 400 bytes
	f.repos = []fakeRepo{{Repo: github.Repo{ID: 1, FullName: "Strazahq/x", Name: long}, Installation: orgInst}}
	cs, _ := listingSession(t, f, nil, nil)
	out := callOK(t, cs, "repo_list", map[string]any{"cursor": listCursor{Installation: orgInst, Page: 1}.encode()}, nil)
	title := objectsOf(out)[0]["title"].(string)
	if len(title) != 256 || !utf8.ValidString(title) {
		t.Errorf("title is %d bytes, valid %v", len(title), utf8.ValidString(title))
	}
}

// setBy comes from values_editable_by in every organization's schema, read
// when the server is built: org only when every organization says org.
func TestRepoListSetBy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeGitHub)
		want  map[string]string
	}{
		{name: "the schemas agree or disagree",
			setup: func(f *fakeGitHub) {
				f.schemas["Strazahq"] = []github.PropertyDef{
					{Name: "team", ValuesEditableBy: "org_actors"}, {Name: "env", ValuesEditableBy: "org_and_repo_actors"},
					{Name: "cost"}, {Name: "tier", ValuesEditableBy: "org_actors"}, {Name: "odd", ValuesEditableBy: "everyone"}}
				f.schemas["acme"] = []github.PropertyDef{
					{Name: "team", ValuesEditableBy: "org_actors"}, {Name: "env", ValuesEditableBy: "org_actors"},
					{Name: "cost", ValuesEditableBy: "org_actors"}, {Name: "tier", ValuesEditableBy: "org_and_repo_actors"}}
			},
			want: map[string]string{"team": "org", "env": "object", "cost": "org", "tier": "object", "odd": "object", "ghost": "object"}},
		{name: "a schema read fails",
			setup: func(f *fakeGitHub) {
				f.schemas["Strazahq"] = []github.PropertyDef{{Name: "team", ValuesEditableBy: "org_actors"}}
				f.schemaErr["acme"] = &github.StatusError{StatusCode: 403, Status: "403 Forbidden"}
			},
			want: map[string]string{"team": "object", "env": "object", "cost": "object", "tier": "object", "odd": "object", "ghost": "object"}},
		{name: "the installations cannot be read",
			setup: func(f *fakeGitHub) {
				f.schemas["Strazahq"] = []github.PropertyDef{{Name: "team", ValuesEditableBy: "org_actors"}}
				f.errs["Installations"] = &github.StatusError{StatusCode: 500, Status: "500 Internal Server Error"}
			},
			want: map[string]string{"team": "object", "env": "object", "cost": "object", "tier": "object", "odd": "object", "ghost": "object"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := listingFake()
			f.schemas = map[string][]github.PropertyDef{}
			tc.setup(f)
			cs, _ := listingSession(t, f, []string{"team", "env", "cost", "tier", "odd", "ghost", "team"}, nil)
			lists := listTools(t, cs)["repo_list"].Meta[metaListsObjects].(map[string]any)
			got := map[string]string{}
			var order []string
			for _, a := range lists["attributes"].([]any) {
				a := a.(map[string]any)
				got[a["name"].(string)] = a["setBy"].(string)
				order = append(order, a["name"].(string))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("setBy = %v, want %v", got, tc.want)
			}
			if want := []string{"team", "env", "cost", "tier", "odd", "ghost"}; !reflect.DeepEqual(order, want) {
				t.Errorf("attributes = %v, want %v once each, in configured order", order, want)
			}
			mints, _ := f.snapshot()
			for _, m := range mints {
				if m.RepoIDs != nil || !reflect.DeepEqual(m.Perms, github.Permissions{"metadata": "read", permCustomProperties: "read"}) {
					t.Errorf("schema read minted %+v", m)
				}
			}
		})
	}
}

// Straza takes at most 16 attributes an object, so only the first 16
// configured are reported.
func TestRepoListAttributeCap(t *testing.T) {
	var attrs []string
	for i := range 20 {
		attrs = append(attrs, fmt.Sprintf("p%d", i))
	}
	f := newFakeGitHub()
	l := newRepoLister(f, newTokenCache(f, newAuditLogger(io.Discard)), attrs)
	if len(l.attrs) != maxAttributes || l.attrs[15] != "p15" {
		t.Errorf("attrs = %v", l.attrs)
	}
}
