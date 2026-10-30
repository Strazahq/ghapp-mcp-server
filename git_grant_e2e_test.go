package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeHolder stands in for Straza's holder on the agent's machine. git talks
// smart HTTP to it, through an insteadOf rule for github.com, and it sends
// each request on to ghapp with a grant from Straza and its own DPoP proof: a
// fetch grant for upload-pack and its info/refs, a push grant without refs
// for receive-pack's info/refs and a push without commands, and a push grant
// naming the ref updates it read before the pack for a push with commands. It
// asks for one grant per request, where Straza's holder reuses a fetch grant
// and a push grant without refs for the requests of one operation. lie, when
// set, changes the new commit of every ref it asks a push grant for.
type fakeHolder struct {
	t      *testing.T
	straza *fakeStraza
	key    holderKey
	ghapp  string // ghapp's URL, the grants' aud
	lie    atomic.Bool

	mu     sync.Mutex
	grants []string // "op ref old new" per grant asked for
	seen   []string // every grant and proof, which must reach no output of git
}

func (h *fakeHolder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, status, msg := parseGitRoute(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	op, refs := rt.op(), []grantRef(nil)
	if !rt.InfoRefs && rt.Service == svcReceivePack {
		push, err := readPushRequest(bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, c := range push.Commands {
			ref := grantRef{Ref: c.Ref, Old: c.Old, New: c.New}
			if h.lie.Load() {
				ref.New = strings.Repeat("e", len(c.New))
			}
			refs = append(refs, ref)
		}
	}
	claims := grantClaimsFor(h.key, op, refs...)
	claims["aud"] = h.ghapp
	grant := h.straza.sign(h.t, claims)
	proof := h.key.proof(h.t, r.Method, h.ghapp+r.URL.Path, grant)
	h.mu.Lock()
	for _, ref := range refs {
		h.grants = append(h.grants, op+" "+ref.Ref+" "+ref.Old+" "+ref.New)
	}
	if refs == nil {
		h.grants = append(h.grants, op)
	}
	h.seen = append(h.seen, grant, proof)
	h.mu.Unlock()

	out, err := http.NewRequestWithContext(r.Context(), r.Method, h.ghapp+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, name := range gitRequestHeaders {
		if v := r.Header.Values(name); len(v) > 0 {
			out.Header[http.CanonicalHeaderKey(name)] = slices.Clone(v)
		}
	}
	out.Header.Set("Authorization", authSchemeDPoP+" "+grant)
	out.Header.Set("DPoP", proof)
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, name := range append([]string{"Content-Length"}, gitResponseHeaders...) {
		if v := resp.Header.Values(name); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(name)] = slices.Clone(v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Real git with no credential and no helper, through a fake holder and
// ghapp with grants, against a bare repo behind git http-backend as GitHub:
// a first push, a clone, a second push, and a push whose grant names other
// commits than git sends, which ghapp refuses per ref.
func TestGitGrantEndToEnd(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("needs git")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git http-backend is not installed")
	}

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=",
		"GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@example.com",
		"GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@example.com",
	}
	var outputs []string
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		outputs = append(outputs, string(out))
		return string(out), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := run(dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(out)
	}

	// GitHub: a bare repo behind git http-backend that takes only a token
	// minted for pay-api with the operation's contents permission.
	root := filepath.Join(tmp, "github")
	bare := filepath.Join(root, "Strazahq", "pay-api.git")
	must(tmp, "init", "--bare", "--initial-branch=main", bare)
	must(bare, "config", "http.receivepack", "true")
	f := newFakeGitHub()
	httpBackend := &cgi.Handler{Path: backend, Root: "/", Env: []string{
		"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + home,
	}}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perm := "read"
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == svcReceivePack {
			perm = "write"
		}
		user, pass, ok := r.BasicAuth()
		m, minted := f.mintedToken(pass)
		if !ok || user != "x-access-token" || !minted || !slices.Equal(m.RepoIDs, []int64{payID}) || len(m.Perms) != 1 || m.Perms["contents"] != perm {
			http.Error(w, "bad credentials", http.StatusForbidden)
			return
		}
		r.Header.Del("Authorization")
		httpBackend.ServeHTTP(w, r)
	}))
	t.Cleanup(github.Close)

	straza := newFakeStraza(t)
	cfg := testGitConfig("", github.URL, straza.jwksURL())
	endpoint := newGitEndpoint(f, cfg)
	logs := &syncBuffer{}
	endpoint.log = slog.New(slog.NewTextHandler(logs, nil))
	ghapp := httptest.NewServer(withGitEndpoint(http.NotFoundHandler(), endpoint))
	t.Cleanup(ghapp.Close)
	endpoint.publicURL = ghapp.URL
	endpoint.grants = newGrantVerifier(testIssuer, straza.jwksURL(), ghapp.URL, endpoint.log)

	holder := &fakeHolder{t: t, straza: straza, key: newHolderKey(t), ghapp: ghapp.URL}
	holderSrv := httptest.NewServer(holder)
	t.Cleanup(holderSrv.Close)
	// The one git setting the holder needs: github.com goes to the holder.
	insteadOf := []string{"-c", "url." + holderSrv.URL + "/git/.insteadOf=https://github.com/"}
	remote := "https://github.com/Strazahq/pay-api.git"

	work := filepath.Join(tmp, "work")
	must(tmp, "init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello from an agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	must(work, "add", "README.md")
	must(work, "commit", "-m", "first")
	first := must(work, "rev-parse", "HEAD")
	must(work, append(insteadOf, "push", remote, "main")...)
	if got := must(bare, "rev-parse", "main"); got != first {
		t.Fatalf("GitHub's main = %s, want %s", got, first)
	}

	clone := filepath.Join(tmp, "clone")
	must(tmp, append(insteadOf, "clone", remote, clone)...)
	if got := must(clone, "rev-parse", "HEAD"); got != first {
		t.Fatalf("clone HEAD = %s, want %s", got, first)
	}
	if err := os.WriteFile(filepath.Join(clone, "fix.txt"), []byte("a fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	must(clone, "add", "fix.txt")
	must(clone, "commit", "-m", "second")
	second := must(clone, "rev-parse", "HEAD")
	must(clone, append(insteadOf, "push", "origin", "main")...)
	if got := must(bare, "rev-parse", "main"); got != second {
		t.Fatalf("GitHub's main = %s, want %s", got, second)
	}

	holder.mu.Lock()
	grants := slices.Clone(holder.grants)
	holder.mu.Unlock()
	for _, want := range []string{
		"push refs/heads/main " + zeroID + " " + first,
		"push refs/heads/main " + first + " " + second,
		gitOpPush, gitOpFetch, // a push grant without refs, for info/refs
	} {
		if !slices.Contains(grants, want) {
			t.Errorf("grants asked for = %q, want %q among them", grants, want)
		}
	}

	// A grant for other commits than git pushes: refused per ref, and
	// GitHub's main stays.
	holder.lie.Store(true)
	if err := os.WriteFile(filepath.Join(clone, "fix.txt"), []byte("another fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	must(clone, "commit", "-am", "third")
	out, err := run(clone, append(insteadOf, "push", "origin", "main")...)
	if err == nil || !strings.Contains(out, reasonNotGranted) {
		t.Errorf("a push the grant does not name: err %v\n%s", err, out)
	}
	if got := must(bare, "rev-parse", "main"); got != second {
		t.Errorf("GitHub's main = %s after a refused push, want %s", got, second)
	}
	holder.lie.Store(false)

	// A tag and a delete are refused with ghapp's reasons, though Straza
	// granted them, and GitHub never sees them.
	must(work, append(insteadOf, "push", remote, "main:refs/heads/dev")...)
	must(work, "tag", "v1")
	for _, tc := range []struct{ refspec, reason string }{{"v1", reasonTag}, {":dev", reasonDelete}} {
		out, err := run(work, append(insteadOf, "push", remote, tc.refspec)...)
		if err == nil || !strings.Contains(out, "[remote rejected]") || !strings.Contains(out, tc.reason) {
			t.Errorf("push %s: %v\n%s", tc.refspec, err, out)
		}
	}
	if out, err := run(bare, "rev-parse", "--verify", "-q", "refs/tags/v1"); err == nil {
		t.Errorf("the tag reached GitHub: %s", out)
	}
	if got := must(bare, "rev-parse", "dev"); got != first {
		t.Errorf("GitHub's dev = %s, want %s: the delete reached GitHub", got, first)
	}

	// A fetch into a repo that has some of the history negotiates with what
	// it has, and gets the rest.
	must(work, append(insteadOf, "fetch", remote, "+refs/heads/*:refs/remotes/origin/*")...)
	if got := must(work, "rev-parse", "refs/remotes/origin/main"); got != second {
		t.Errorf("fetched origin/main = %s, want %s", got, second)
	}

	holder.mu.Lock()
	seen := slices.Clone(holder.seen)
	holder.mu.Unlock()
	for _, secret := range seen {
		for _, o := range outputs {
			if strings.Contains(o, secret) {
				t.Fatal("git printed a grant or a proof")
			}
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatal("ghapp logged a grant or a proof")
		}
	}
	if !strings.Contains(logs.String(), "via=grant") {
		t.Errorf("ghapp's log:\n%s", logs.String())
	}
}
