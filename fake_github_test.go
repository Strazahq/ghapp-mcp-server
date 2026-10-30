package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// The repos of the fake GitHub.
const (
	payID   int64 = 812345678 // Strazahq/pay-api
	otherID int64 = 812345679 // Strazahq/other
	userID  int64 = 5550001   // joe/dotfiles
	orgInst int64 = 7
	usrInst int64 = 3
)

// mintRec is one MintToken call.
type mintRec struct {
	Installation int64
	RepoIDs      []int64
	Name         string // the owner/name a token minted by name was asked for
	Perms        github.Permissions
	Value        string // the token minted, "" when the mint failed
}

// callRec is one call made with a token: Do, or a read of the interface.
type callRec struct {
	Op    string // "Repository", "RepositoryByID", "InstallationRepos", "PropertySchema", "PropertyValues", or Do's method
	Path  string // Do's path, or the name or id read
	Query url.Values
	Body  string // Do's body as JSON
	Token string
}

type fakeRepo struct {
	github.Repo
	Installation int64
}

// fakeGitHub is a github.API that serves canned repos and answers, records
// every mint and every call, and lets a token reach only what GitHub would
// let it reach: its installation, and its repository ids when it has some.
type fakeGitHub struct {
	mu            sync.Mutex
	installations []github.Installation
	repos         []fakeRepo
	redirects     map[string]bool   // names, lower case, that GitHub redirects everywhere
	readRedirects map[string]bool   // names, lower case, that only the repo read redirects
	aliases       map[string]string // a name the repo read answers with another repo, without a redirect
	routes        map[string]string // "GET /repos/Strazahq/pay-api/pulls" to a JSON answer
	errs          map[string]error  // the same keys, or "MintToken", to an error
	idMintErrs    map[int64]error   // an error for a mint whose repository ids hold this id
	pageSize      int

	schemas   map[string][]github.PropertyDef
	schemaErr map[string]error
	values    map[string][]github.RepoProperties
	valuesErr map[string]error

	// now is the clock a token's ExpiresAt counts from: an hour, as GitHub.
	now func() time.Time
	// strictMint answers 422, as GitHub does, to a mint for a repo id that
	// is not in the installation.
	strictMint bool
	// permMintErrs fails every mint that asks for the permission, as GitHub
	// does for one the App lacks. A metadata mint never asks for another.
	permMintErrs map[string]error
	// beforeMint, when set, runs at the start of every mint, outside the
	// lock, so a test can hold a mint open.
	beforeMint func()

	minted   int
	mints    []mintRec
	calls    []callRec
	appCalls []string // the calls made with the App JWT: "InstallationForRepo name", "Installations"
	tokens   map[string]mintRec
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		installations: []github.Installation{
			{ID: orgInst, Account: "Strazahq", AccountType: "Organization"},
			{ID: usrInst, Account: "joe", AccountType: "User"},
		},
		repos: []fakeRepo{
			{Repo: repo(payID, "Strazahq", "pay-api", "Organization"), Installation: orgInst},
			{Repo: repo(otherID, "Strazahq", "other", "Organization"), Installation: orgInst},
			{Repo: repo(userID, "joe", "dotfiles", "User"), Installation: usrInst},
		},
		redirects:     map[string]bool{"strazahq/pay-old": true},
		readRedirects: map[string]bool{},
		aliases:       map[string]string{},
		routes:        map[string]string{},
		errs:          map[string]error{},
		idMintErrs:    map[int64]error{},
		permMintErrs:  map[string]error{},
		pageSize:      100,
		schemas:       map[string][]github.PropertyDef{},
		schemaErr:     map[string]error{},
		values:        map[string][]github.RepoProperties{},
		valuesErr:     map[string]error{},
		now:           time.Now,
		tokens:        map[string]mintRec{},
	}
}

func repo(id int64, owner, name, ownerType string) github.Repo {
	return github.Repo{
		ID: id, FullName: owner + "/" + name, Name: name, Owner: owner, OwnerType: ownerType,
		Private: true, DefaultBranch: "main", HTMLURL: "https://github.com/" + owner + "/" + name,
	}
}

var _ github.API = (*fakeGitHub)(nil)

func (f *fakeGitHub) byName(fullName string) (fakeRepo, bool) {
	for _, r := range f.repos {
		if strings.EqualFold(r.FullName, fullName) { // GitHub resolves names in any case
			return r, true
		}
	}
	return fakeRepo{}, false
}

// reaches reports whether tok reaches r, and records nothing.
func (f *fakeGitHub) reaches(tok github.Token, r fakeRepo) bool {
	m, ok := f.tokens[tok.Value]
	if !ok || m.Installation != r.Installation {
		return false
	}
	return len(m.RepoIDs) == 0 || slices.Contains(m.RepoIDs, r.ID)
}

func (f *fakeGitHub) record(c callRec) {
	f.calls = append(f.calls, c)
}

func (f *fakeGitHub) InstallationForRepo(_ context.Context, fullName string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appCalls = append(f.appCalls, "InstallationForRepo "+fullName)
	if f.redirects[strings.ToLower(fullName)] {
		return 0, github.ErrRedirect
	}
	if alias, ok := f.aliases[fullName]; ok {
		fullName = alias
	}
	r, ok := f.byName(fullName)
	if !ok {
		return 0, github.ErrNotFound
	}
	return r.Installation, nil
}

func (f *fakeGitHub) Installations(context.Context) ([]github.Installation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appCalls = append(f.appCalls, "Installations")
	if err := f.errs["Installations"]; err != nil {
		return nil, err
	}
	return slices.Clone(f.installations), nil
}

func (f *fakeGitHub) MintToken(_ context.Context, inst int64, repoIDs []int64, perms github.Permissions) (github.Token, error) {
	if f.beforeMint != nil {
		f.beforeMint()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := mintRec{Installation: inst, RepoIDs: slices.Clone(repoIDs), Perms: maps.Clone(perms)}
	if len(perms) == 0 {
		f.mints = append(f.mints, rec)
		return github.Token{}, errors.New("perms must not be empty")
	}
	if err := f.mintErr(perms); err != nil {
		f.mints = append(f.mints, rec)
		return github.Token{}, err
	}
	for _, id := range repoIDs {
		if err := f.idMintErrs[id]; err != nil {
			f.mints = append(f.mints, rec)
			return github.Token{}, err
		}
		if f.strictMint && !slices.ContainsFunc(f.repos, func(r fakeRepo) bool { return r.ID == id && r.Installation == inst }) {
			f.mints = append(f.mints, rec)
			return github.Token{}, &github.StatusError{Method: "POST", Path: fmt.Sprintf("/app/installations/%d/access_tokens", inst),
				StatusCode: 422, Status: "422 Unprocessable Entity", Message: "There is at least one repository that does not exist or is not accessible to the parent installation."}
		}
	}
	f.minted++ // never reset, so a token value is never minted twice
	rec.Value = fmt.Sprintf("ghs_fake%04d", f.minted)
	f.mints = append(f.mints, rec)
	f.tokens[rec.Value] = rec
	return github.Token{Value: rec.Value, InstallationID: inst, ExpiresAt: f.now().Add(time.Hour)}, nil
}

// MintTokenForRepo mints by name, matched without regard to case as GitHub
// does, and answers 422 for a name the installation doesn't hold. The token
// reaches that one repo, and its Repos names it as GitHub spells it.
func (f *fakeGitHub) MintTokenForRepo(_ context.Context, inst int64, fullName string, perms github.Permissions) (github.Token, error) {
	if f.beforeMint != nil {
		f.beforeMint()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := mintRec{Installation: inst, Name: fullName, Perms: maps.Clone(perms)}
	if err := f.mintErr(perms); err != nil {
		f.mints = append(f.mints, rec)
		return github.Token{}, err
	}
	target := fullName
	if alias, ok := f.aliases[fullName]; ok {
		target = alias
	}
	i := slices.IndexFunc(f.repos, func(r fakeRepo) bool {
		return r.Installation == inst && strings.EqualFold(r.FullName, target)
	})
	if i < 0 {
		f.mints = append(f.mints, rec)
		return github.Token{}, &github.StatusError{Method: "POST", Path: fmt.Sprintf("/app/installations/%d/access_tokens", inst),
			StatusCode: 422, Status: "422 Unprocessable Entity", Message: "There is at least one repository that does not exist or is not accessible to the parent installation."}
	}
	repo := f.repos[i].Repo
	f.minted++
	rec.RepoIDs = []int64{repo.ID}
	rec.Value = fmt.Sprintf("ghs_fake%04d", f.minted)
	f.mints = append(f.mints, rec)
	f.tokens[rec.Value] = rec
	return github.Token{Value: rec.Value, InstallationID: inst, ExpiresAt: f.now().Add(time.Hour), Repos: []github.Repo{repo}}, nil
}

// mintErr is the failure set for a mint with perms: errs["MintToken"] for
// every mint, or permMintErrs for a mint that asks for that permission.
func (f *fakeGitHub) mintErr(perms github.Permissions) error {
	if err := f.errs["MintToken"]; err != nil {
		return err
	}
	for name := range perms {
		if err := f.permMintErrs[name]; err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGitHub) RepositoryByID(_ context.Context, tok github.Token, id int64) (github.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(callRec{Op: "RepositoryByID", Path: fmt.Sprint(id), Token: tok.Value})
	for _, r := range f.repos {
		if r.ID == id && f.reaches(tok, r) {
			return r.Repo, nil
		}
	}
	return github.Repo{}, github.ErrNotFound
}

func (f *fakeGitHub) Repository(_ context.Context, tok github.Token, fullName string) (github.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(callRec{Op: "Repository", Path: fullName, Token: tok.Value})
	if f.redirects[strings.ToLower(fullName)] || f.readRedirects[strings.ToLower(fullName)] {
		return github.Repo{}, github.ErrRedirect
	}
	if alias, ok := f.aliases[fullName]; ok {
		fullName = alias
	}
	r, ok := f.byName(fullName)
	if !ok || !f.reaches(tok, r) {
		return github.Repo{}, github.ErrNotFound
	}
	return r.Repo, nil
}

func (f *fakeGitHub) InstallationRepos(_ context.Context, tok github.Token, page int) ([]github.Repo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(callRec{Op: "InstallationRepos", Path: fmt.Sprint(page), Token: tok.Value})
	m, ok := f.tokens[tok.Value]
	if !ok {
		return nil, false, &github.StatusError{StatusCode: 401, Status: "401 Unauthorized"}
	}
	var all []github.Repo
	for _, r := range f.repos {
		if f.reaches(tok, r) && r.Installation == m.Installation {
			all = append(all, r.Repo)
		}
	}
	return pageOf(all, page, f.pageSize)
}

func (f *fakeGitHub) PropertySchema(_ context.Context, tok github.Token, org string) ([]github.PropertyDef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(callRec{Op: "PropertySchema", Path: org, Token: tok.Value})
	if err := f.schemaErr[org]; err != nil {
		return nil, err
	}
	return slices.Clone(f.schemas[org]), nil
}

func (f *fakeGitHub) PropertyValues(_ context.Context, tok github.Token, org string, page int) ([]github.RepoProperties, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(callRec{Op: "PropertyValues", Path: fmt.Sprintf("%s/%d", org, page), Token: tok.Value})
	if err := f.valuesErr[org]; err != nil {
		return nil, false, err
	}
	return pageOf(f.values[org], page, f.pageSize)
}

func pageOf[T any](all []T, page, size int) ([]T, bool, error) {
	start := (page - 1) * size
	if start >= len(all) {
		return nil, false, nil
	}
	end := min(start+size, len(all))
	return slices.Clone(all[start:end]), end < len(all), nil
}

func (f *fakeGitHub) Do(_ context.Context, tok github.Token, method, path string, query url.Values, body, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	f.record(callRec{Op: method, Path: path, Query: query, Body: string(b), Token: tok.Value})
	if _, ok := f.tokens[tok.Value]; !ok {
		return &github.StatusError{Method: method, Path: path, StatusCode: 401, Status: "401 Unauthorized"}
	}
	if rest, ok := strings.CutPrefix(path, "/repos/"); ok {
		segs := strings.SplitN(rest, "/", 3)
		if len(segs) < 2 {
			return github.ErrNotFound
		}
		owner, err1 := url.PathUnescape(segs[0])
		name, err2 := url.PathUnescape(segs[1])
		if f.redirects[strings.ToLower(owner+"/"+name)] {
			return github.ErrRedirect
		}
		r, found := f.byName(owner + "/" + name)
		if err1 != nil || err2 != nil || !found || !f.reaches(tok, r) {
			return github.ErrNotFound
		}
	}
	key := method + " " + path
	if err := f.errs[key]; err != nil {
		return err
	}
	answer, ok := f.routes[key]
	if !ok {
		return github.ErrNotFound
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal([]byte(answer), out)
}

// snapshot returns the mints and calls so far.
func (f *fakeGitHub) snapshot() ([]mintRec, []callRec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.mints), slices.Clone(f.calls)
}

// reset forgets the mints and calls so far.
func (f *fakeGitHub) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints, f.calls, f.appCalls = nil, nil, nil
}

// gitHubCalls lists every call that would reach GitHub since the last
// reset, in kinds: the App JWT's calls, the mints, then the calls with a
// token, each as "Op path".
func (f *fakeGitHub) gitHubCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.appCalls)
	for _, m := range f.mints {
		if m.Name != "" {
			out = append(out, fmt.Sprintf("MintTokenForRepo %d %s", m.Installation, m.Name))
			continue
		}
		out = append(out, fmt.Sprintf("MintToken %d %v", m.Installation, m.RepoIDs))
	}
	for _, c := range f.calls {
		out = append(out, c.Op+" "+c.Path)
	}
	return out
}

// setRepo changes the fake repo with id, as a rename, a transfer or a
// recreate on GitHub would.
func (f *fakeGitHub) setRepo(id int64, change func(*fakeRepo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.repos {
		if f.repos[i].ID == id {
			change(&f.repos[i])
			return
		}
	}
	panic(fmt.Sprintf("no fake repo %d", id))
}

// --- the MCP side ---

// connect serves server over in-memory transports to a real client.
func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "t"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// connectFake builds the server main builds, on f.
func connectFake(t *testing.T, f *fakeGitHub, cfg Config) *mcp.ClientSession {
	t.Helper()
	return connect(t, newMCPServer(f, cfg))
}

// testCallerKey names, in a call's _meta, the caller testCallers puts in
// the context: "agent|client|subject", "gateway" or "listing".
const testCallerKey = "test/caller"

// testCallers stands in for callerGate in tests over the in-memory
// transport, which carries no bearer.
func testCallers(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
			if s, ok := call.Params.Meta[testCallerKey].(string); ok {
				parts := strings.SplitN(s, "|", 3)
				c := Caller{Kind: parts[0]}
				if len(parts) == 3 {
					c.Client, c.Subject = parts[1], parts[2]
				}
				ctx = withCaller(ctx, c)
			}
		}
		return next(ctx, method, req)
	}
}

// asAgent is a call's _meta for an agent caller, plus more.
func asAgent(client, subject string, more mcp.Meta) mcp.Meta {
	return asCaller(callerAgent+"|"+client+"|"+subject, more)
}

// asCaller is a call's _meta for the caller testCallers reads, plus more.
func asCaller(caller string, more mcp.Meta) mcp.Meta {
	m := mcp.Meta{testCallerKey: caller}
	maps.Copy(m, more)
	return m
}

// syncBuffer is a bytes.Buffer safe for a writer and a reader at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testServer is the server main builds on a fake GitHub, with its tools at
// hand and their audit log captured.
type testServer struct {
	f     *fakeGitHub
	tools *tools
	log   *syncBuffer
	cs    *mcp.ClientSession
}

// connectLogged builds the server as newMCPServer does, with the audit log
// in a buffer and testCallers in front.
func connectLogged(t *testing.T, f *fakeGitHub, cfg Config) *testServer {
	t.Helper()
	logs := &syncBuffer{}
	tl := newTools(f, cfg, newAuditLogger(logs))
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version},
		&mcp.ServerOptions{Instructions: serverInstructions})
	tl.register(server, cfg)
	server.AddReceivingMiddleware(errorCodes)
	server.AddReceivingMiddleware(testCallers)
	return &testServer{f: f, tools: tl, log: logs, cs: connect(t, server)}
}

// setClock makes now the clock of the caches and of the fake's tokens.
func (s *testServer) setClock(now func() time.Time) {
	s.tools.now, s.tools.tokens.now, s.f.now = now, now, now
}

// logLines are the audit log's lines with msg, decoded.
func (s *testServer) logLines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.log.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// withID is a call's _meta with the id Straza approved.
func withID(id any) mcp.Meta {
	return mcp.Meta{metaObjectID: id}
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any, meta mcp.Meta) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// callOK calls a tool expecting success and returns its structured content.
func callOK(t *testing.T, cs *mcp.ClientSession, name string, args any, meta mcp.Meta) map[string]any {
	t.Helper()
	res := callTool(t, cs, name, args, meta)
	if res.IsError {
		t.Fatalf("CallTool(%s) failed: %s", name, resultText(res))
	}
	if _, ok := res.Meta[errorMetaKey]; ok {
		t.Errorf("%s: a successful result carries an error payload %v", name, res.Meta)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("structured content of %s is not an object: %s", name, b)
	}
	return m
}

// callErr calls a tool expecting an error result, and returns its text and
// its error payload.
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args any, meta mcp.Meta) (string, map[string]any) {
	t.Helper()
	res := callTool(t, cs, name, args, meta)
	if !res.IsError {
		t.Fatalf("CallTool(%s) succeeded, want an error result: %s", name, resultText(res))
	}
	if res.StructuredContent != nil {
		t.Errorf("%s: error result carries structuredContent %v", name, res.StructuredContent)
	}
	payload, _ := res.Meta[errorMetaKey].(map[string]any)
	return resultText(res), payload
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n---\n")
}

// mintsWithout drops the token values, for comparing mints.
func mintsWithout(ms []mintRec) []mintRec {
	out := []mintRec{}
	for _, m := range ms {
		m.Value = ""
		out = append(out, m)
	}
	return out
}

func mintOf(inst int64, ids []int64, perms github.Permissions) mintRec {
	return mintRec{Installation: inst, RepoIDs: ids, Perms: perms}
}

// nameMintOf is a mint by the name fullName that reached the repo id.
func nameMintOf(inst int64, fullName string, id int64, perms github.Permissions) mintRec {
	return mintRec{Installation: inst, RepoIDs: []int64{id}, Name: fullName, Perms: perms}
}

// doCalls are the Do calls among calls.
func doCalls(calls []callRec) []callRec {
	var out []callRec
	for _, c := range calls {
		switch c.Op {
		case "Repository", "RepositoryByID", "InstallationRepos", "PropertySchema", "PropertyValues":
			continue
		}
		out = append(out, c)
	}
	return out
}
