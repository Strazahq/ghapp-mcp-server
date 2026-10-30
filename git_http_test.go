package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// upstreamReq is one request the fake upstream git server received.
type upstreamReq struct {
	Method string
	URI    string
	Header http.Header
	Body   []byte
}

// fakeUpstream is GitHub's git server: it records each request and answers
// with canned info/refs and receive-pack answers, or with answer when set.
type fakeUpstream struct {
	mu     sync.Mutex
	reqs   []upstreamReq
	answer func(w http.ResponseWriter, r *http.Request, body []byte) // nil: the canned answers
}

const (
	advertReceive = "001f# service=git-receive-pack\n0000"
	advertUpload  = "001e# service=git-upload-pack\n0000"
	receiveOK     = "000eunpack ok\n0019ok refs/heads/main\n0000"
	uploadAnswer  = "0008NAK\nPACK-DATA"
)

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.reqs = append(u.reqs, upstreamReq{Method: r.Method, URI: r.URL.RequestURI(), Header: r.Header.Clone(), Body: body})
	answer := u.answer
	u.mu.Unlock()
	if answer != nil {
		answer(w, r, body)
		return
	}
	// Headers no client of ghapp may see.
	w.Header().Set("X-Seen-Authorization", r.Header.Get("Authorization"))
	w.Header().Set("Set-Cookie", "session="+r.Header.Get("Authorization"))
	w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
	w.Header().Set("Cache-Control", "no-cache")
	switch {
	case strings.HasSuffix(r.URL.Path, "/info/refs"):
		svc := r.URL.Query().Get("service")
		w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
		if svc == svcReceivePack {
			io.WriteString(w, advertReceive)
		} else {
			io.WriteString(w, advertUpload)
		}
	case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		io.WriteString(w, receiveOK)
	case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		io.WriteString(w, uploadAnswer)
	default:
		http.NotFound(w, r)
	}
}

func (u *fakeUpstream) requests() []upstreamReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamReq(nil), u.reqs...)
}

// gitTest is the git endpoint in front of a fake upstream, on a fake
// GitHub, with a fake Straza that signs grants and the key of Straza's
// holder that proves them.
type gitTest struct {
	f        *fakeGitHub
	up       *fakeUpstream
	endpoint *gitEndpoint
	srv      *httptest.Server
	log      *syncBuffer
	straza   *fakeStraza
	holder   holderKey
}

func testGitConfig(public, upstream, jwks string) GitConfig {
	return GitConfig{PublicURL: public, Upstream: upstream, MaxPushBytes: 1 << 30, Timeout: time.Minute,
		GrantIssuer: testIssuer, GrantJWKSURL: jwks}
}

func newGitTest(t *testing.T) *gitTest {
	t.Helper()
	g := &gitTest{f: newFakeGitHub(), up: &fakeUpstream{}, log: &syncBuffer{}, straza: newFakeStraza(t), holder: newHolderKey(t)}
	upSrv := httptest.NewServer(g.up)
	t.Cleanup(upSrv.Close)
	g.endpoint = newGitEndpoint(g.f, testGitConfig(testPublic, upSrv.URL, g.straza.jwksURL()))
	g.endpoint.log = slog.New(slog.NewTextHandler(g.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g.endpoint.grants = newGrantVerifier(testIssuer, g.straza.jwksURL(), testPublic, g.endpoint.log)
	mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	g.srv = httptest.NewServer(withGitEndpoint(mcp, g.endpoint))
	t.Cleanup(g.srv.Close)
	return g
}

// grant is a new grant from the fake Straza for op on pay-api, bound to the
// holder's key, naming refs.
func (g *gitTest) grant(t *testing.T, op string, refs ...grantRef) string {
	t.Helper()
	return g.straza.sign(t, grantClaimsFor(g.holder, op, refs...))
}

// send sends one request as Straza's holder does: with grant, when not
// empty, and a new proof for this request. It returns the answer with its
// body read.
func (g *gitTest) send(t *testing.T, method, path, grant string, body io.Reader, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, g.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if grant != "" {
		req.Header.Set("Authorization", "DPoP "+grant)
		req.Header.Set("DPoP", g.holder.proof(t, method, htu(path), grant))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// do sends one request with a new grant for op, or with no credentials when
// op is empty. A push grant names the ref updates at the start of a
// receive-pack body, as the holder reads them before it asks Straza.
func (g *gitTest) do(t *testing.T, method, path, op string, body io.Reader, header map[string]string) (*http.Response, string) {
	t.Helper()
	if op == "" {
		return g.send(t, method, path, "", body, header)
	}
	var refs []grantRef
	if body != nil && strings.HasSuffix(path, "/"+svcReceivePack) {
		refs, body = pushRefs(t, body, header["Content-Encoding"])
	}
	return g.send(t, method, path, g.grant(t, op, refs...), body, header)
}

// pushRefs reads the ref updates at the start of a push body, through gzip
// when encoding says so, and returns them with a body that still holds
// every byte. A body of known length stays one, so the request keeps its
// Content-Length. A body ghapp cannot read gives no refs.
func pushRefs(t *testing.T, body io.Reader, encoding string) ([]grantRef, io.Reader) {
	t.Helper()
	var src io.Reader
	switch body.(type) {
	case *strings.Reader, *bytes.Reader, *bytes.Buffer:
		all, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		body, src = bytes.NewReader(all), bytes.NewReader(all)
	default:
		var read bytes.Buffer
		src = io.TeeReader(body, &read)
		body = io.MultiReader(&read, body)
	}
	if encoding == "gzip" {
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, body
		}
		src = zr
	}
	push, err := readPushRequest(src)
	if err != nil {
		return nil, body
	}
	var refs []grantRef
	for _, c := range push.Commands {
		refs = append(refs, grantRef{Ref: c.Ref, Old: c.Old, New: c.New})
	}
	return refs, body
}

const (
	payGit          = "/git/Strazahq/pay-api.git"
	infoRefsReceive = payGit + "/info/refs?service=git-receive-pack"
	infoRefsUpload  = payGit + "/info/refs?service=git-upload-pack"
	receivePath     = payGit + "/git-receive-pack"
	uploadPath      = payGit + "/git-upload-pack"
)

// mintedToken is the mint behind a token value.
func (f *fakeGitHub) mintedToken(value string) (mintRec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.tokens[value]
	return m, ok
}

// Without a grant every route answers 401 with the DPoP challenge only, no
// Basic challenge, so git asks no credential helper and the agent learns to
// run git through Straza's holder. A Basic password, such as a GitHub
// token, is no grant either.
func TestGitNoCredentials(t *testing.T) {
	g := newGitTest(t)
	for _, tc := range []struct {
		method, path string
		header       map[string]string
	}{
		{http.MethodGet, infoRefsReceive, nil},
		{http.MethodPost, receivePath, nil},
		{http.MethodGet, infoRefsUpload, nil},
		{http.MethodPost, uploadPath, nil},
		{http.MethodGet, "/git/Strazahq/pay-api/info/refs?service=git-upload-pack", nil},
		{http.MethodGet, infoRefsUpload, map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_notAGrant"))}},
		{http.MethodGet, infoRefsUpload, map[string]string{"Authorization": "Bearer ghs_notAGrant"}},
	} {
		resp, body := g.send(t, tc.method, tc.path, "", strings.NewReader("0000"), tc.header)
		if got := resp.Header.Values("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized || len(got) != 1 || got[0] != `DPoP algs="EdDSA"` {
			t.Errorf("%s %s = %d %q, want 401 with the DPoP challenge only", tc.method, tc.path, resp.StatusCode, got)
		}
		if body != "ghapp takes Straza grants only: run git through Straza's holder on this machine.\n" {
			t.Errorf("%s %s body = %q", tc.method, tc.path, body)
		}
	}
	if mints, _ := g.f.snapshot(); len(mints) != 0 {
		t.Errorf("minted without a grant: %+v", mints)
	}
	if reqs := g.up.requests(); len(reqs) != 0 {
		t.Errorf("upstream reached without a grant: %+v", reqs)
	}
	log := g.log.String()
	if !strings.Contains(log, "level=DEBUG msg=git") || !strings.Contains(log, "outcome=no-credentials") ||
		!strings.Contains(log, `reason="not a Straza grant"`) || strings.Contains(log, "ghs_notAGrant") {
		t.Errorf("log = %q, want debug lines for no credentials and the reason for a Basic password, without it", log)
	}
}

// Answers of upload-pack, as GitHub sends them.
var (
	lsRefsAnswer   = pkt(oidA+" HEAD symref-target:refs/heads/main\n") + pkt(oidA+" refs/heads/main\n") + "0000"
	v2CloneAnswer  = pkt("packfile\n") + pkt("\x02Enumerating objects: 3, done.\n") + pkt("\x01PACK\x00\x00\x00\x02pack") + "0000"
	v2RoundAnswer  = pkt("acknowledgments\n") + pkt("NAK\n") + "0000"
	v2ReadyAnswer  = pkt("acknowledgments\n") + pkt("ACK "+oidA+"\n") + pkt("ready\n") + "0001" + v2CloneAnswer
	v0RoundAnswer  = pkt("NAK\n")
	v0BandAnswer   = pkt("ACK "+oidA+"\n") + pkt("\x01PACK\x00\x00\x00\x02pack") + "0000"
	v0BareAnswer   = pkt("NAK\n") + "PACK\x00\x00\x00\x02pack"
	errAnswer      = pkt("ERR upload-pack: not our ref " + oidB + "\n")
	shallowAnswer  = pkt("shallow-info\n") + pkt("shallow "+oidA+"\n") + "0001" + v2CloneAnswer
	emptyPktAnswer = "0004" + v0BareAnswer
)

// A fetch grant is spent by the upload-pack POST whose answer starts a
// pack: not by ls-refs, not by a negotiation round without a pack, and not
// by an error. Then every request with it gets the 401.
func TestGitFetchGrantSpentByThePack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
		spentAt int // the answer that spends the grant, -1 for none
	}{
		{"v2 clone: ls-refs, then fetch", []string{lsRefsAnswer, v2CloneAnswer}, 1},
		{"v2 fetch: ls-refs, two rounds, then ready", []string{lsRefsAnswer, v2RoundAnswer, v2RoundAnswer, v2ReadyAnswer}, 3},
		{"v2 shallow", []string{shallowAnswer}, 0},
		{"v0: a round, then the pack in side-band", []string{v0RoundAnswer, v0BandAnswer}, 1},
		{"v0: the pack without side-band", []string{v0BareAnswer}, 0},
		{"v0: an empty pkt-line first", []string{emptyPktAnswer}, 0},
		{"an error", []string{errAnswer, errAnswer}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGitTest(t)
			var mu sync.Mutex
			next := 0
			g.up.answer = func(w http.ResponseWriter, r *http.Request, body []byte) {
				mu.Lock()
				answer := tc.answers[next]
				next++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				io.WriteString(w, answer)
			}
			grant := g.grant(t, gitOpFetch)
			key := "grant " + jtiOf(t, grant)
			for i, answer := range tc.answers {
				resp, body := g.send(t, http.MethodPost, uploadPath, grant, strings.NewReader("0014command=fetch\n0000"), map[string]string{"Git-Protocol": "version=2"})
				if resp.StatusCode != http.StatusOK || body != answer {
					t.Fatalf("POST %d = %d %q, want 200 with the answer passed through", i, resp.StatusCode, body)
				}
				if spent := g.endpoint.used.isUsed(key); spent != (tc.spentAt >= 0 && i >= tc.spentAt) {
					t.Fatalf("after POST %d spent = %v, want it spent at %d", i, spent, tc.spentAt)
				}
			}
			if tc.spentAt < 0 {
				return
			}
			for _, step := range []struct{ method, path string }{{http.MethodGet, infoRefsUpload}, {http.MethodPost, uploadPath}} {
				resp, body := g.send(t, step.method, step.path, grant, strings.NewReader("0000"), nil)
				if resp.StatusCode != http.StatusUnauthorized || body != upperFirst(errGrantUsed.Error())+".\n" {
					t.Errorf("replay %s %s = %d %q, want 401", step.method, step.path, resp.StatusCode, body)
				}
			}
			if reqs := g.up.requests(); len(reqs) != len(tc.answers) {
				t.Errorf("upstream got %d requests, want %d", len(reqs), len(tc.answers))
			}
		})
	}
}

// When another request spent the grant while this one negotiated, the
// answer is cut before its pack reaches git.
func TestGitFetchPackOnceUnderARace(t *testing.T) {
	g := newGitTest(t)
	grant := g.grant(t, gitOpFetch)
	key := "grant " + jtiOf(t, grant)
	g.up.answer = func(w http.ResponseWriter, r *http.Request, body []byte) {
		// Another request with the grant got its pack first.
		g.endpoint.used.use(key, time.Now().Add(time.Minute), time.Now())
		io.WriteString(w, v2ReadyAnswer)
	}
	_, body := g.send(t, http.MethodPost, uploadPath, grant, strings.NewReader("0000"), nil)
	if strings.Contains(body, "PACK") {
		t.Errorf("git got a second pack for one grant: %q", body)
	}
	if !strings.Contains(g.log.String(), "the grant was used by another request") {
		t.Errorf("log = %q", g.log.String())
	}
}

// The spotter finds the pack however the answer is cut into reads, passes
// every byte through, and when onPack fails returns no byte of the pack.
func TestPackSpotter(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		pack   bool
	}{
		"ls-refs": {lsRefsAnswer, false}, "v2 clone": {v2CloneAnswer, true}, "v2 round": {v2RoundAnswer, false},
		"v2 ready": {v2ReadyAnswer, true}, "v0 round": {v0RoundAnswer, false}, "v0 band": {v0BandAnswer, true},
		"v0 bare": {v0BareAnswer, true}, "error": {errAnswer, false}, "not pkt-lines": {"Repository not found.\n", false},
		"empty": {"", false}, "a long line first": {pkt(strings.Repeat("x", 65000)) + v0BareAnswer, true},
		"a cut line": {"0100abc", false},
	} {
		for _, reads := range []string{"whole", "one byte at a time"} {
			var src io.Reader = strings.NewReader(tc.answer)
			if reads != "whole" {
				src = iotest.OneByteReader(src)
			}
			calls := 0
			s := &packSpotter{r: src, onPack: func() error { calls++; return nil }}
			out, err := io.ReadAll(s)
			if err != nil || string(out) != tc.answer || calls != map[bool]int{true: 1, false: 0}[tc.pack] {
				t.Errorf("%s, %s: %d calls, %v, passed through %v", name, reads, calls, err, string(out) == tc.answer)
			}
			if len(s.buf) > 70000 {
				t.Errorf("%s, %s: holds %d bytes", name, reads, len(s.buf))
			}
		}
	}
	s := &packSpotter{r: strings.NewReader(v2ReadyAnswer), onPack: func() error { return errGrantUsed }}
	out, err := io.ReadAll(s)
	if !errors.Is(err, errGrantUsed) || strings.Contains(string(out), "PACK") {
		t.Errorf("a failed onPack: %v, read %q", err, out)
	}
}

// A full set of spent ids refuses new grants with 503 until some expire,
// so no grant is let through unremembered. A spent one still gets its 401.
// Each proof takes a place in the set too.
func TestGitUsedGrantsFull(t *testing.T) {
	g := newGitTest(t)
	g.endpoint.used = newUsedIDs(3)
	used := g.grant(t, gitOpPush, refMain)
	// The proof and the grant: two of three.
	if resp, body := g.send(t, http.MethodPost, receivePath, used, bytes.NewReader(pushBody(cmdMain)), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("push = %d %q", resp.StatusCode, body)
	}
	resp, body := g.send(t, http.MethodGet, infoRefsReceive, g.grant(t, gitOpPush), nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "used grants as it can") || !strings.Contains(body, "Try again in a few minutes") {
		t.Errorf("a new grant with the set full = %d %q, want 503", resp.StatusCode, body)
	}
	if resp, _ := g.send(t, http.MethodPost, receivePath, used, bytes.NewReader(pushBody(cmdMain)), nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the spent grant with the set full = %d, want 401", resp.StatusCode)
	}
	// Once the spent grant and the proofs expire, there is room again: a
	// grant and a proof made at that later time open info/refs.
	later := time.Now().Add(4 * time.Minute)
	g.endpoint.now = func() time.Time { return later }
	c := grantClaimsFor(g.holder, gitOpPush)
	c["iat"], c["exp"] = later.Unix(), later.Add(2*time.Minute).Unix()
	fresh := g.straza.sign(t, c)
	req, _ := http.NewRequest(http.MethodGet, g.srv.URL+infoRefsReceive, nil)
	req.Header.Set("Authorization", "DPoP "+fresh)
	req.Header.Set("DPoP", g.holder.proofWith(t, map[string]any{"iat": later.Unix()}, http.MethodGet, htu(infoRefsReceive), fresh))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("a new grant after the spent one expired = %d, want 200", r.StatusCode)
	}
}

// A push end to end against the fake upstream: the installation token
// reaches upstream only, for exactly the repo id Straza approved with
// contents write, and never the client; neither the grant nor the proof
// reaches upstream; only the allowed headers pass; and the log line names
// the push without a secret.
func TestGitPushTokenReachesUpstreamOnly(t *testing.T) {
	g := newGitTest(t)
	header := map[string]string{
		"Git-Protocol": "version=0", "Accept": "application/x-git-receive-pack-result",
		"Content-Type": "application/x-git-receive-pack-request", "User-Agent": "git/2.47.3",
		"Cookie": "a=b", "X-Forwarded-For": "10.0.0.9", "X-Custom": "nope",
	}

	adv := g.grant(t, gitOpPush)
	resp, body := g.send(t, http.MethodGet, infoRefsReceive, adv, nil, header)
	if resp.StatusCode != http.StatusOK || body != advertReceive || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-advertisement" {
		t.Fatalf("info/refs = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	reqBody := pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main"), cmd(zeroID, oidC, "refs/heads/new")) + "PACK\x00\x00\x00\x02pack bytes"
	push := g.grant(t, gitOpPush, refMain, grantRef{Ref: "refs/heads/new", Old: zeroID, New: oidC})
	resp2, body2 := g.send(t, http.MethodPost, receivePath, push, strings.NewReader(reqBody), header)
	if resp2.StatusCode != http.StatusOK || body2 != receiveOK {
		t.Fatalf("receive-pack = %d %q", resp2.StatusCode, body2)
	}

	mints, _ := g.f.snapshot()
	// One mint for both requests of the push: the second reuses the token,
	// which is the same agent, repo and permission set.
	want := []mintRec{mintOf(orgInst, []int64{payID}, permContentsWrite)}
	if got := mintsWithout(mints); !reflect.DeepEqual(got, want) {
		t.Fatalf("mints = %+v, want %+v", got, want)
	}
	reqs := g.up.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %+v", reqs)
	}
	if reqs[0].Method != http.MethodGet || reqs[0].URI != "/Strazahq/pay-api.git/info/refs?service=git-receive-pack" ||
		reqs[1].Method != http.MethodPost || reqs[1].URI != "/Strazahq/pay-api.git/git-receive-pack" {
		t.Errorf("upstream saw %s %s and %s %s", reqs[0].Method, reqs[0].URI, reqs[1].Method, reqs[1].URI)
	}
	if string(reqs[1].Body) != reqBody {
		t.Errorf("upstream body = %q, want the push byte for byte", reqs[1].Body)
	}
	for i, r := range reqs {
		user, pass, ok := (&http.Request{Header: r.Header}).BasicAuth()
		if !ok || user != "x-access-token" || pass != mints[0].Value {
			t.Errorf("upstream request %d authenticated as %q, want x-access-token and the one token minted for the push", i, user)
		}
		if r.Header.Get("DPoP") != "" {
			t.Errorf("upstream request %d carries the DPoP proof", i)
		}
		for name, vs := range r.Header {
			for _, v := range vs {
				for _, grant := range []string{adv, push} {
					if strings.Contains(v, grant) || strings.Contains(v, grant[len(grant)-20:]) {
						t.Errorf("upstream header %s carries a grant", name)
					}
				}
			}
		}
		for _, name := range []string{"Git-Protocol", "Accept", "Content-Type", "User-Agent"} {
			if r.Header.Get(name) != header[name] && !(i == 0 && name == "Content-Type") {
				t.Errorf("request %d: %s = %q, want %q passed through", i, name, r.Header.Get(name), header[name])
			}
		}
		for _, name := range []string{"Cookie", "X-Forwarded-For", "X-Custom"} {
			if r.Header.Get(name) != "" {
				t.Errorf("request %d: %s passed through", i, name)
			}
		}
	}

	for _, r := range []*http.Response{resp, resp2} {
		for name, vs := range r.Header {
			for _, v := range vs {
				for _, m := range mints {
					if strings.Contains(v, m.Value) {
						t.Errorf("the client got the token in %s", name)
					}
				}
			}
		}
		for _, name := range []string{"X-Seen-Authorization", "Set-Cookie", "WWW-Authenticate"} {
			if r.Header.Get(name) != "" {
				t.Errorf("the client got upstream's %s", name)
			}
		}
		if r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("Cache-Control = %q, want upstream's passed through", r.Header.Get("Cache-Control"))
		}
	}

	logs := g.log.String()
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != 2 {
		t.Fatalf("log = %q, want one line per request", logs)
	}
	for i, want := range []string{
		"op=push step=info/refs repo=Strazahq/pay-api via=grant",
		"op=push step=git-receive-pack repo=Strazahq/pay-api via=grant",
	} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "rid=812345678") || !strings.Contains(lines[i], "outcome=ok") ||
			!strings.Contains(lines[i], "status=200") || !strings.Contains(lines[i], "hashed_token="+strconv.Quote(hashedToken(mints[0].Value))) ||
			!strings.Contains(lines[i], "sub=user-42") {
			t.Errorf("log line %d = %q", i, lines[i])
		}
	}
	if !strings.Contains(lines[1], fmt.Sprintf("refs=\"refs/heads/main %s %s, refs/heads/new %s %s\"", oidA, oidB, zeroID, oidC)) ||
		!strings.Contains(lines[1], fmt.Sprintf("bytes_in=%d bytes_out=%d", len(reqBody), len(receiveOK))) {
		t.Errorf("push log line = %q, want refs and bytes", lines[1])
	}
	for _, m := range mints {
		if strings.Contains(logs, m.Value) {
			t.Error("the log holds a token")
		}
	}
	for _, grant := range []string{adv, push} {
		if strings.Contains(logs, grant) {
			t.Error("the log holds a grant")
		}
	}
}

// hashed_token has GitHub's form: base64 of the SHA-256 of the token.
func TestHashedToken(t *testing.T) {
	// printf ghs_example | openssl dgst -sha256 -binary | base64
	if got := hashedToken("ghs_example"); got != "UMaRjVoedd29MJgtcZ6b6jIFPLlG7OzD84ZhFi+79Cc=" {
		t.Errorf("hashedToken = %q", got)
	}
}

// A fetch mints contents read, and passes upload-pack's body as it is,
// compressed or not.
func TestGitFetch(t *testing.T) {
	g := newGitTest(t)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, "0032want "+oidA+"\n00000009done\n")
	zw.Close()

	resp, body := g.do(t, http.MethodPost, uploadPath, gitOpFetch, bytes.NewReader(gz.Bytes()),
		map[string]string{"Content-Encoding": "gzip", "Content-Type": "application/x-git-upload-pack-request", "Git-Protocol": "version=2"})
	if resp.StatusCode != http.StatusOK || body != uploadAnswer {
		t.Fatalf("upload-pack = %d %q", resp.StatusCode, body)
	}
	mints, _ := g.f.snapshot()
	if got := mintsWithout(mints); !reflect.DeepEqual(got, []mintRec{mintOf(orgInst, []int64{payID}, permContentsRead)}) {
		t.Errorf("mints = %+v, want contents read for the one repo", got)
	}
	reqs := g.up.requests()
	if len(reqs) != 1 || !bytes.Equal(reqs[0].Body, gz.Bytes()) || reqs[0].Header.Get("Content-Encoding") != "gzip" || reqs[0].Header.Get("Git-Protocol") != "version=2" {
		t.Errorf("upstream got %+v", reqs)
	}
}

// A push in gzip is read plain and sent on plain.
func TestGitPushGzip(t *testing.T) {
	g := newGitTest(t)
	plain := pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main")) + "PACK-DATA"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, plain)
	zw.Close()
	resp, _ := g.do(t, http.MethodPost, receivePath, gitOpPush, &gz, map[string]string{"Content-Encoding": "gzip"})
	reqs := g.up.requests()
	if resp.StatusCode != http.StatusOK || len(reqs) != 1 || string(reqs[0].Body) != plain || reqs[0].Header.Get("Content-Encoding") != "" {
		t.Errorf("status %d, upstream %+v", resp.StatusCode, reqs)
	}

	resp, _ = g.do(t, http.MethodPost, receivePath, gitOpPush, strings.NewReader(plain), map[string]string{"Content-Encoding": "br"})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("brotli push: status %d, want 415", resp.StatusCode)
	}
}

// parseReport reads a report-status answer, side-band or not.
func parseReport(t *testing.T, body string) []string {
	t.Helper()
	pkts := readPkts(t, []byte(body))
	if len(pkts) > 0 && len(pkts[0]) > 0 && pkts[0][0] == 1 {
		var band1 bytes.Buffer
		for _, p := range pkts[:len(pkts)-1] {
			band1.WriteString(p[1:])
		}
		pkts = readPkts(t, band1.Bytes())
	}
	return pkts
}

// A refused push is answered with report-status, one ng per ref, after
// the whole body was read; nothing goes upstream and no token is minted.
func TestGitPushRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps string
		cmds []string
		want []string
	}{
		{"a delete", testCaps, []string{cmd(oidA, zeroID, "refs/heads/main")},
			[]string{"unpack ok\n", "ng refs/heads/main " + reasonDelete + "\n", ""}},
		{"a tag, no side-band", "report-status", []string{cmd(zeroID, oidA, "refs/tags/v1")},
			[]string{"unpack ok\n", "ng refs/tags/v1 " + reasonTag + "\n", ""}},
		{"a tag among branches", "report-status-v2 side-band-64k", []string{cmd(oidA, oidB, "refs/heads/main"), cmd(zeroID, oidA, "refs/tags/v1")},
			[]string{"unpack ok\n", "ng refs/heads/main " + reasonOtherRefused + "\n", "ng refs/tags/v1 " + reasonTag + "\n", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGitTest(t)
			pack := strings.Repeat("P", 300<<10) // more than the server would drain on its own
			resp, body := g.do(t, http.MethodPost, receivePath, gitOpPush,
				strings.NewReader(pushHead(tc.caps, tc.cmds...)+pack), nil)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-result" {
				t.Fatalf("status %d, type %q: %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
			if got := parseReport(t, body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("report = %q, want %q", got, tc.want)
			}
			if mints, _ := g.f.snapshot(); len(mints) != 0 {
				t.Errorf("a refused push minted %+v", mints)
			}
			if reqs := g.up.requests(); len(reqs) != 0 {
				t.Errorf("a refused push went upstream")
			}
			if log := g.log.String(); !strings.Contains(log, "outcome=refused") || !strings.Contains(log, "refs=") {
				t.Errorf("log = %q", log)
			}
		})
	}
}

// Without report-status, or with a request ghapp cannot read, the answer
// is an HTTP error with one plain line.
func TestGitPushErrors(t *testing.T) {
	refs := make([]string, 1001)
	for i := range refs {
		refs[i] = cmd(oidA, oidB, fmt.Sprintf("refs/heads/b%d", i))
	}
	for _, tc := range []struct {
		name   string
		body   string
		status int
		text   string
	}{
		{"no report-status", pushHead("side-band-64k", cmd(zeroID, oidA, "refs/tags/v1")) + "PACK", http.StatusForbidden, "ghapp refused this push: " + reasonTag},
		{"malformed", pkt("not a command\n") + "0000", http.StatusBadRequest, "not valid git protocol"},
		{"cut short", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main"))[:20], http.StatusBadRequest, "ends inside the ref updates"},
		{"1001 refs", pushHead(testCaps, refs...), http.StatusRequestEntityTooLarge, "more than 1000 refs"},
		{"signed", pkt("push-cert\x00report-status") + "0000", http.StatusForbidden, "Signed pushes are not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGitTest(t)
			resp, body := g.do(t, http.MethodPost, receivePath, gitOpPush, strings.NewReader(tc.body), nil)
			if resp.StatusCode != tc.status || !strings.Contains(body, tc.text) || strings.Count(body, "\n") != 1 ||
				!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
				t.Errorf("status %d %q, want %d with %q", resp.StatusCode, body, tc.status, tc.text)
			}
			if mints, _ := g.f.snapshot(); len(mints) != 0 {
				t.Errorf("minted %+v", mints)
			}
			if reqs := g.up.requests(); len(reqs) != 0 {
				t.Errorf("went upstream")
			}
		})
	}
}

// A body over GHAPP_GIT_MAX_PUSH_BYTES is cut and answered 413.
func TestGitPushTooLarge(t *testing.T) {
	g := newGitTest(t)
	g.endpoint.maxBody = 64 << 10
	g.up.answer = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusOK)
	}
	head := pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main"))
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{"known length", strings.NewReader(head + strings.Repeat("P", 256<<10))},
		{"chunked", io.MultiReader(strings.NewReader(head), strings.NewReader(strings.Repeat("P", 256<<10)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := g.do(t, http.MethodPost, receivePath, gitOpPush, tc.body, nil)
			if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(body, envGitMaxPushBytes) {
				t.Errorf("status %d %q, want 413 naming %s", resp.StatusCode, body, envGitMaxPushBytes)
			}
		})
	}
	for _, r := range g.up.requests() {
		if len(r.Body) > 64<<10 {
			t.Errorf("upstream got %d bytes, past the cap", len(r.Body))
		}
	}
}

// A redirect from upstream is refused, and its target never requested.
func TestGitNoRedirect(t *testing.T) {
	g := newGitTest(t)
	g.up.answer = func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.HasPrefix(r.URL.Path, "/Strazahq/pay-api.git/") {
			http.Redirect(w, r, "/Strazahq/pay-new.git"+strings.TrimPrefix(r.URL.RequestURI(), "/Strazahq/pay-api.git"), http.StatusMovedPermanently)
			return
		}
		io.WriteString(w, advertUpload)
	}
	resp, body := g.do(t, http.MethodGet, infoRefsUpload, gitOpFetch, nil, nil)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" || !strings.Contains(body, "redirect") {
		t.Errorf("status %d, Location %q, body %q", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	reqs := g.up.requests()
	if len(reqs) != 1 || strings.Contains(reqs[0].URI, "pay-new") {
		t.Errorf("upstream requests %+v, want the one request and no follow", reqs)
	}
}

// GitHub refusing ghapp's own token does not ask git for credentials, and
// GitHub's other answers pass through.
func TestGitUpstreamAnswers(t *testing.T) {
	g := newGitTest(t)
	status := http.StatusUnauthorized
	g.up.answer = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		io.WriteString(w, "Repository not found.\n")
	}
	resp, _ := g.do(t, http.MethodGet, infoRefsUpload, gitOpFetch, nil, nil)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("upstream 401: status %d, challenge %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	status = http.StatusNotFound
	resp, body := g.do(t, http.MethodGet, infoRefsUpload, gitOpFetch, nil, nil)
	if resp.StatusCode != http.StatusNotFound || body != "Repository not found.\n" || resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("upstream 404: status %d %q", resp.StatusCode, body)
	}
	if !strings.Contains(g.log.String(), "outcome=upstream-404") {
		t.Errorf("log = %q", g.log.String())
	}
}

// A token GitHub will not mint ends the request before upstream.
func TestGitMintRefused(t *testing.T) {
	g := newGitTest(t)
	g.f.idMintErrs[payID] = &github.StatusError{Method: "POST", Path: "/app/installations/7/access_tokens", StatusCode: 422, Status: "422 Unprocessable Entity"}
	resp, body := g.do(t, http.MethodGet, infoRefsUpload, gitOpFetch, nil, nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Strazahq/pay-api") {
		t.Errorf("status %d %q", resp.StatusCode, body)
	}
	g.f.idMintErrs[payID] = errors.New("connection reset")
	resp, _ = g.do(t, http.MethodGet, infoRefsUpload, gitOpFetch, nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
	if reqs := g.up.requests(); len(reqs) != 0 {
		t.Errorf("went upstream without a token")
	}
}

// Paths and methods outside smart HTTP are refused before any grant check.
func TestGitRoutes(t *testing.T) {
	g := newGitTest(t)
	push := gitOpPush
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, payGit + "/info/refs", http.StatusForbidden},
		{http.MethodGet, payGit + "/info/refs?service=git-upload-archive", http.StatusForbidden},
		{http.MethodGet, payGit + "/info/refs?service=git-upload-pack&service=git-receive-pack", http.StatusForbidden},
		{http.MethodGet, payGit + "/info/refs?service=git-upload-pack&x=1", http.StatusForbidden},
		{http.MethodPost, payGit + "/info/refs?service=git-upload-pack", http.StatusMethodNotAllowed},
		{http.MethodGet, receivePath, http.StatusMethodNotAllowed},
		{http.MethodGet, payGit + "/HEAD", http.StatusNotFound},
		{http.MethodGet, payGit + "/objects/info/packs", http.StatusNotFound},
		{http.MethodGet, "/git/Strazahq/info/refs?service=git-upload-pack", http.StatusNotFound},
		{http.MethodGet, "/git/Straza_hq/pay-api.git/info/refs?service=git-upload-pack", http.StatusNotFound},
		{http.MethodGet, "/git/Strazahq/pay%2Dapi.git/info/refs?service=git-upload-pack", http.StatusNotFound},
		{http.MethodGet, "/git/Strazahq/pay-api.git/x/info/refs?service=git-upload-pack", http.StatusNotFound},
	} {
		resp, body := g.do(t, tc.method, tc.path, push, nil, nil)
		if resp.StatusCode != tc.status || strings.Count(body, "\n") != 1 {
			t.Errorf("%s %s = %d %q, want %d", tc.method, tc.path, resp.StatusCode, body, tc.status)
		}
	}
	if mints, _ := g.f.snapshot(); len(mints) != 0 {
		t.Errorf("minted %+v", mints)
	}
	// Everything outside /git/ goes to the MCP handler.
	for _, path := range []string{"/mcp", "/", "/gitx"} {
		if resp, _ := g.do(t, http.MethodGet, path, "", nil, nil); resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s went to the git endpoint: %d", path, resp.StatusCode)
		}
	}
}

// zeros is n zero bytes, made as they are read.
type zeros struct{ n int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	clear(p)
	z.n -= int64(len(p))
	return len(p), nil
}

// allocatedDuring is how many bytes the Go heap allocated while f ran.
func allocatedDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A push and a fetch of 64 MiB each stream through: the whole process
// allocates a small part of that, so neither body is ever held whole.
func TestGitStreamsLargeBodies(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 128 MiB")
	}
	const size = 64 << 20
	g := newGitTest(t)
	var upstreamGot atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			n, _ := io.Copy(io.Discard, r.Body)
			upstreamGot.Store(n)
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			io.WriteString(w, receiveOK)
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			io.Copy(w, &zeros{n: size})
		}
	}))
	t.Cleanup(up.Close)
	g.endpoint.upstream = up.URL
	push, fetch := g.grant(t, gitOpPush, refMain), g.grant(t, gitOpFetch)
	head := pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main"))
	// The grant and a proof as Straza's holder sends them.
	authorize := func(req *http.Request, grant string) {
		req.Header.Set("Authorization", "DPoP "+grant)
		req.Header.Set("DPoP", g.holder.proof(t, req.Method, htu(req.URL.Path), grant))
	}

	pushAlloc := allocatedDuring(func() {
		req, _ := http.NewRequest(http.MethodPost, g.srv.URL+receivePath, io.MultiReader(strings.NewReader(head), &zeros{n: size}))
		authorize(req, push)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("push status %d", resp.StatusCode)
		}
	})
	if got := upstreamGot.Load(); got != int64(len(head))+size {
		t.Errorf("upstream got %d bytes, want %d", got, len(head)+size)
	}

	var fetched int64
	fetchAlloc := allocatedDuring(func() {
		req, _ := http.NewRequest(http.MethodPost, g.srv.URL+uploadPath, strings.NewReader("0000"))
		authorize(req, fetch)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		fetched, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	})
	if fetched != size {
		t.Errorf("fetched %d bytes, want %d", fetched, size)
	}
	for name, alloc := range map[string]uint64{"push": pushAlloc, "fetch": fetchAlloc} {
		t.Logf("%s of %d MiB allocated %d KiB", name, size>>20, alloc>>10)
		if alloc > size/8 {
			t.Errorf("%s of %d MiB allocated %d MiB: the body is held, not streamed", name, size>>20, alloc>>20)
		}
	}
}

// With caller authentication on, /mcp needs a bearer and /git does not: the
// git endpoint's own auth is the Straza grant, which is all Straza's holder
// brings. It never holds the gateway secret or an OIDC token for ghapp, so
// the bearer check must not cover /git, as serveHTTP wires the two.
func TestGitEndpointBesideCallerAuth(t *testing.T) {
	g := newGitTest(t)
	cfg := Config{GatewaySecret: "gateway-secret-for-tests", Git: testGitConfig(testPublic, g.endpoint.upstream, g.straza.jwksURL())}
	srv := httptest.NewServer(withGitEndpoint(mcpHTTPHandler(newMCPServer(g.f, cfg), cfg, nil), g.endpoint))
	t.Cleanup(srv.Close)

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(init))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/mcp without a bearer = %d, want 401", resp.StatusCode)
	}

	grant := g.grant(t, gitOpFetch)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+infoRefsUpload, nil)
	req.Header.Set("Authorization", "DPoP "+grant)
	req.Header.Set("DPoP", g.holder.proof(t, http.MethodGet, htu(infoRefsUpload), grant))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != advertUpload {
		t.Errorf("/git with a grant and no bearer = %d %q, want the advertisement", resp.StatusCode, body)
	}
	// A bearer that /mcp would take opens nothing at /git.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+infoRefsUpload, nil)
	req.Header.Set("Authorization", "Bearer gateway-secret-for-tests")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/git with the gateway secret = %d, want 401", resp.StatusCode)
	}
}
