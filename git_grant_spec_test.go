package main

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// specExample is one conformance example of Straza's spec/grant, as
// testdata/spec-grant holds it. SOURCE.txt there says where it came from.
type specExample struct {
	Provenance string    `json:"provenance"`
	At         time.Time `json:"at"`
	Verifier   struct {
		Issuer   string `json:"issuer"`
		Audience string `json:"audience"`
	} `json:"verifier"`
	Key    json.RawMessage `json:"key"`
	Grant  string          `json:"grant"`
	Claims json.RawMessage `json:"claims"`
	Reason string          `json:"reason"`
}

// specMachineSeed is the Ed25519 seed of the machine key behind every
// example's cnf.jkt, as the examples' provenance names it: 32 bytes 0x07.
var specMachineSeed = bytes.Repeat([]byte{0x07}, ed25519.SeedSize)

// The examples of spec/grant, replayed against ghapp's verifier. Each runs
// with its own issuer, audience, clock and key. A valid example passes and
// ghapp reads every claim as the spec names it. An invalid example is
// refused with the error of the rule its reason names. A file this table
// does not know fails the test, so a new example gets a decision here.
func TestGrantSpecExamples(t *testing.T) {
	want := map[string]struct {
		reason string
		err    error // nil for a valid example
	}{
		"valid-fetch.json":                {},
		"valid-push-advertisement.json":   {},
		"valid-push-refs-approved.json":   {},
		"valid-push-sha256.json":          {},
		"invalid-typ-session.json":        {"typ is not straza-grant+jwt", errGrantMalformed},
		"invalid-other-signer.json":       {"signature does not verify", errGrantSignature},
		"invalid-missing-cnf.json":        {"claim cnf is missing", errGrantFields},
		"invalid-refs-on-fetch.json":      {"refs on a fetch", errGrantFields},
		"invalid-aud-trailing-slash.json": {"aud is not this server's git base URL", errGrantNotOurs},
		"invalid-lifetime-over-300s.json": {"exp minus iat is over 300 seconds", errGrantFields},
	}
	files, err := filepath.Glob(filepath.Join("testdata", "spec-grant", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(want) {
		t.Errorf("testdata/spec-grant holds %d examples, this test knows %d", len(files), len(want))
	}
	for _, path := range files {
		name := filepath.Base(path)
		w, known := want[name]
		if !known {
			t.Errorf("%s is not in this test's table. Add it with the error ghapp must give for it", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			ex := readSpecExample(t, path)
			if !strings.HasPrefix(ex.Provenance, "doc ") && !strings.HasPrefix(ex.Provenance, "live ") {
				t.Errorf("provenance %q says neither doc nor live", ex.Provenance)
			}
			if ex.Reason != w.reason {
				t.Fatalf("reason = %q, want %q. The example changed: decide which error ghapp must give for it", ex.Reason, w.reason)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			v := newGrantVerifier(ex.Verifier.Issuer, serveSpecKey(t, ex.Key), ex.Verifier.Audience, log)
			c, err := v.verify(t.Context(), ex.Grant, ex.At)
			if w.err != nil {
				if !errors.Is(err, w.err) {
					t.Fatalf("verify = %v, want %v", err, w.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			checkSpecClaims(t, c, ex.Claims)
		})
	}
}

func readSpecExample(t *testing.T, path string) specExample {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ex specExample
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if ex.Grant == "" || ex.At.IsZero() || ex.Verifier.Issuer == "" || ex.Verifier.Audience == "" || len(ex.Key) == 0 {
		t.Fatalf("%s lacks grant, at, verifier or key", path)
	}
	return ex
}

// serveSpecKey serves key as Straza's key set and returns its URL.
func serveSpecKey(t *testing.T, key json.RawMessage) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != strazaJWKSPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []json.RawMessage{key}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + strazaJWKSPath
}

// checkSpecClaims compares what ghapp read from a valid example with the
// example's decoded claims, and the machine key's thumbprint with cnf.jkt.
func checkSpecClaims(t *testing.T, c grantClaims, raw json.RawMessage) {
	t.Helper()
	var want struct {
		Aud string          `json:"aud"`
		Sub string          `json:"sub"`
		Act json.RawMessage `json:"act"`
		Jti string          `json:"jti"`
		Cnf struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
		Srv string `json:"srv"`
		Obj struct {
			Type  string `json:"type"`
			Value string `json:"value"`
			ID    string `json:"id"`
		} `json:"obj"`
		Op   string     `json:"op"`
		Refs []grantRef `json:"refs"`
		Dec  string     `json:"dec"`
		Apr  string     `json:"apr"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := []string{c.Aud, c.Sub, c.Jti, c.Cnf.JKT, c.Srv, c.Obj.Type, c.Obj.Value, strconv.FormatInt(int64(c.Obj.ID), 10), c.Op, c.Dec, c.Apr}
	exp := []string{want.Aud, want.Sub, want.Jti, want.Cnf.JKT, want.Srv, want.Obj.Type, want.Obj.Value, want.Obj.ID, want.Op, want.Dec, want.Apr}
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("ghapp read %q, want %q", got, exp)
	}
	if !reflect.DeepEqual(c.Refs, want.Refs) {
		t.Errorf("refs = %+v, want %+v", c.Refs, want.Refs)
	}
	var gotAct, wantAct map[string]any
	if json.Unmarshal([]byte(c.actor()), &gotAct) != nil || json.Unmarshal(want.Act, &wantAct) != nil || !reflect.DeepEqual(gotAct, wantAct) {
		t.Errorf("act = %s, want %s", c.actor(), want.Act)
	}
	jwk := jose.JSONWebKey{Key: ed25519.NewKeyFromSeed(specMachineSeed).Public()}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got := b64url.EncodeToString(thumb); got != c.Cnf.JKT {
		t.Errorf("the machine key's thumbprint is %s, the grant's cnf.jkt %s", got, c.Cnf.JKT)
	}
}

// Every claim form spec/grant pins, checked by the verifier with a fixed
// clock: what passes, what is refused as malformed, and what is refused as
// not for this ghapp.
func TestGrantClaimForms(t *testing.T) {
	s := newFakeStraza(t)
	h := newHolderKey(t)
	v := newGrantVerifier(testIssuer, s.jwksURL(), testPublic, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().Truncate(time.Second)
	act := func(c map[string]any) map[string]any { return c["act"].(map[string]any) }
	obj := func(c map[string]any) map[string]any { return c["obj"].(map[string]any) }
	ref40, ref64 := strings.Repeat("a", 40), strings.Repeat("b", 64)

	for _, tc := range []struct {
		name   string
		op     string
		refs   []grantRef
		change func(map[string]any)
		want   error
	}{
		{"a fetch", gitOpFetch, nil, nil, nil},
		{"a push without refs", gitOpPush, nil, nil, nil},
		{"a push with refs", gitOpPush, []grantRef{refMain}, nil, nil},
		{"a push with SHA-256 ids", gitOpPush, []grantRef{{Ref: "refs/heads/main", Old: ref64, New: strings.Repeat("c", 64)}}, nil, nil},
		{"a claim ghapp does not know", gitOpFetch, nil, func(c map[string]any) { c["later"] = map[string]any{"x": 1} }, nil},
		{"act without hrn", gitOpFetch, nil, func(c map[string]any) { delete(act(c), "hrn") }, nil},
		{"act with a member ghapp does not know", gitOpFetch, nil, func(c map[string]any) { act(c)["team"] = "payments" }, nil},
		{"obj.id as a number", gitOpFetch, nil, func(c map[string]any) { obj(c)["id"] = 812345678 }, nil},
		{"a lifetime of 300 seconds", gitOpFetch, nil, func(c map[string]any) { c["exp"] = now.Add(300 * time.Second).Unix() }, nil},
		{"59 seconds after exp", gitOpFetch, nil, func(c map[string]any) {
			c["iat"], c["exp"] = now.Add(-179*time.Second).Unix(), now.Add(-59*time.Second).Unix()
		}, nil},

		{"op advertise", "advertise", nil, nil, errGrantFields},
		{"op missing", gitOpFetch, nil, func(c map[string]any) { delete(c, "op") }, errGrantFields},
		{"aud as an array", gitOpFetch, nil, func(c map[string]any) { c["aud"] = []string{testPublic} }, errGrantFields},
		{"act without ses", gitOpFetch, nil, func(c map[string]any) { delete(act(c), "ses") }, errGrantFields},
		{"act with a number for ses", gitOpFetch, nil, func(c map[string]any) { act(c)["ses"] = 7 }, errGrantFields},
		{"act without kind", gitOpFetch, nil, func(c map[string]any) { delete(act(c), "kind") }, errGrantFields},
		{"act with another kind", gitOpFetch, nil, func(c map[string]any) { act(c)["kind"] = "claude-code" }, errGrantFields},
		{"act with an empty hrn", gitOpFetch, nil, func(c map[string]any) { act(c)["hrn"] = "" }, errGrantFields},
		{"act as a string", gitOpFetch, nil, func(c map[string]any) { c["act"] = "sess-7" }, errGrantFields},
		{"obj.id zero", gitOpFetch, nil, func(c map[string]any) { obj(c)["id"] = "0" }, errGrantFields},
		{"obj.id not a number", gitOpFetch, nil, func(c map[string]any) { obj(c)["id"] = "R_kgDO" }, errGrantFields},
		{"no srv", gitOpFetch, nil, func(c map[string]any) { delete(c, "srv") }, errGrantFields},
		{"no dec", gitOpFetch, nil, func(c map[string]any) { delete(c, "dec") }, errGrantFields},
		{"refs on a fetch", gitOpFetch, []grantRef{refMain}, nil, errGrantFields},
		{"an empty refs array", gitOpPush, nil, func(c map[string]any) { c["refs"] = []grantRef{} }, errGrantFields},
		{"a ref outside refs/", gitOpPush, []grantRef{{Ref: "heads/main", Old: oidA, New: oidB}}, nil, errGrantFields},
		{"a ref of refs/ alone", gitOpPush, []grantRef{{Ref: "refs/", Old: oidA, New: oidB}}, nil, errGrantFields},
		{"old and new of two lengths", gitOpPush, []grantRef{{Ref: "refs/heads/main", Old: ref40, New: ref64}}, nil, errGrantFields},
		{"upper-case hex", gitOpPush, []grantRef{{Ref: "refs/heads/main", Old: strings.ToUpper(oidA), New: oidB}}, nil, errGrantFields},
		{"iat with a fraction", gitOpFetch, nil, func(c map[string]any) { c["iat"] = float64(now.Unix()) + 0.5 }, errGrantFields},
		{"iat as a string", gitOpFetch, nil, func(c map[string]any) { c["iat"] = strconv.FormatInt(now.Unix(), 10) }, errGrantFields},
		{"exp missing", gitOpFetch, nil, func(c map[string]any) { delete(c, "exp") }, errGrantFields},
		{"a lifetime of 301 seconds", gitOpFetch, nil, func(c map[string]any) { c["exp"] = now.Add(301 * time.Second).Unix() }, errGrantFields},
		{"iat 61 seconds ahead", gitOpFetch, nil, func(c map[string]any) {
			c["iat"], c["exp"] = now.Add(61*time.Second).Unix(), now.Add(181*time.Second).Unix()
		}, errGrantFields},

		{"60 seconds after exp", gitOpFetch, nil, func(c map[string]any) {
			c["iat"], c["exp"] = now.Add(-180*time.Second).Unix(), now.Add(-60*time.Second).Unix()
		}, errGrantExpired},
		{"another audience", gitOpFetch, nil, func(c map[string]any) { c["aud"] = "https://other-ghapp.test" }, errGrantNotOurs},
		{"the audience with a trailing slash", gitOpFetch, nil, func(c map[string]any) { c["aud"] = testPublic + "/" }, errGrantNotOurs},
		{"another issuer", gitOpFetch, nil, func(c map[string]any) { c["iss"] = testIssuer + "/" }, errGrantNotOurs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := grantClaimsFor(h, tc.op, tc.refs...)
			c["iat"], c["exp"] = now.Unix(), now.Add(2*time.Minute).Unix()
			if tc.change != nil {
				tc.change(c)
			}
			_, err := v.verify(t.Context(), s.sign(t, c), now)
			if !errors.Is(err, tc.want) {
				t.Errorf("verify = %v, want %v", err, tc.want)
			}
		})
	}
}

// spec/grant caps a compact grant at 8192 bytes, and a resource server must
// take one up to that length. The longest grant at or under the cap passes,
// and the shortest over it is refused before it is read.
func TestGrantSizeCap(t *testing.T) {
	s := newFakeStraza(t)
	h := newHolderKey(t)
	v := newGrantVerifier(testIssuer, s.jwksURL(), testPublic, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var under, over string
	for n := 5500; over == ""; n++ {
		c := grantClaimsFor(h, gitOpFetch)
		c["pad"] = strings.Repeat("p", n) // a claim ghapp does not know
		switch g := s.sign(t, c); {
		case len(g) <= grantMaxLen:
			under = g
		default:
			over = g
		}
	}
	if len(under) < grantMaxLen-3 {
		t.Fatalf("the longest grant under the cap is %d bytes, want close to %d", len(under), grantMaxLen)
	}
	if _, err := v.verify(t.Context(), under, time.Now()); err != nil {
		t.Errorf("a grant of %d bytes = %v, want it taken", len(under), err)
	}
	if _, err := v.verify(t.Context(), over, time.Now()); !errors.Is(err, errGrantMalformed) {
		t.Errorf("a grant of %d bytes = %v, want %v", len(over), err, errGrantMalformed)
	}
	if grantMaxLen != 8192 {
		t.Errorf("grantMaxLen = %d, spec/grant's cap is 8192", grantMaxLen)
	}
}

// Each grant opens the routes spec/grant's table gives it, and no other: a
// fetch grant the upload-pack side, a push grant without refs the
// receive-pack info/refs and a POST without commands, and a push grant with
// refs only the POST of those updates.
func TestGrantAllows(t *testing.T) {
	infoUpload := gitRoute{Repo: pay, Service: svcUploadPack, InfoRefs: true}
	upload := gitRoute{Repo: pay, Service: svcUploadPack}
	infoReceive := gitRoute{Repo: pay, Service: svcReceivePack, InfoRefs: true}
	receive := gitRoute{Repo: pay, Service: svcReceivePack}
	fetch := grantClaims{Op: gitOpFetch}
	push := grantClaims{Op: gitOpPush}
	pushRefs := grantClaims{Op: gitOpPush, Refs: []grantRef{refMain}}
	for _, tc := range []struct {
		name  string
		grant grantClaims
		route gitRoute
		want  string
	}{
		{"fetch at upload-pack info/refs", fetch, infoUpload, ""},
		{"fetch at upload-pack", fetch, upload, ""},
		{"fetch at receive-pack info/refs", fetch, infoReceive, "the grant is for fetch, not for info/refs"},
		{"fetch at receive-pack", fetch, receive, "the grant is for fetch, not for git-receive-pack"},
		{"push without refs at receive-pack info/refs", push, infoReceive, ""},
		{"push without refs at receive-pack", push, receive, ""},
		{"push without refs at upload-pack info/refs", push, infoUpload, "the grant is for push, not for info/refs"},
		{"push without refs at upload-pack", push, upload, "the grant is for push, not for git-upload-pack"},
		{"push with refs at receive-pack", pushRefs, receive, ""},
		{"push with refs at receive-pack info/refs", pushRefs, infoReceive, "the grant is for push, not for info/refs"},
	} {
		if got := tc.grant.allows(tc.route); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A full set of spent ids refuses with 503 and one sentence that names
// what it holds.
func TestGitSpentStoreFullNamesWhatItHolds(t *testing.T) {
	g, s := newGrantGitTest(t)
	g.endpoint.used = newUsedIDs(1)
	g.endpoint.used.use("someone else's", time.Now().Add(time.Hour), time.Now())
	h := newHolderKey(t)
	grant := s.sign(t, grantClaimsFor(h, gitOpFetch))
	resp, body := g.doGrant(t, http.MethodGet, infoRefsUpload, grant, []string{h.proof(t, http.MethodGet, htu(infoRefsUpload), grant)}, nil)
	want := "ghapp did not run this git request, because it already remembers as many used grants as it can until some expire. Try again in a few minutes.\n"
	if resp.StatusCode != http.StatusServiceUnavailable || body != want {
		t.Errorf("= %d %q, want 503 %q", resp.StatusCode, body, want)
	}
	if !strings.Contains(g.log.String(), "the set of used grants is full") {
		t.Errorf("log lacks the reason:\n%s", g.log.String())
	}
}

// A push grant with refs is spent at the push whose commands match, before
// ghapp mints a token, so a push refused by the repo checks or a mint that
// fails leaves no grant to send again (spec/grant, "refused or not").
func TestGitGrantPushSpentBeforeToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spoil  func(g *gitTest, c map[string]any) // makes the token step fail
		status int
	}{
		{"the repo id is another repo's", func(_ *gitTest, c map[string]any) {
			c["obj"].(map[string]any)["id"] = "812345679"
		}, http.StatusForbidden},
		{"the token mint fails", func(g *gitTest, _ map[string]any) {
			g.f.mu.Lock()
			g.f.permMintErrs["contents"] = statusErr(http.StatusBadGateway, "GitHub is down", false)
			g.f.mu.Unlock()
		}, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, s := newGrantGitTest(t)
			h := newHolderKey(t)
			c := grantClaimsFor(h, gitOpPush, refMain)
			tc.spoil(g, c)
			grant := s.sign(t, c)
			resp, body := g.doGrant(t, http.MethodPost, receivePath, grant, []string{h.proof(t, http.MethodPost, htu(receivePath), grant)}, bytes.NewReader(pushBody(cmdMain)))
			if resp.StatusCode != tc.status {
				t.Fatalf("push = %d %q, want %d", resp.StatusCode, body, tc.status)
			}
			if !g.endpoint.used.isUsed("grant " + jtiOf(t, grant)) {
				t.Error("the push left its grant unspent")
			}
			resp, body = g.doGrant(t, http.MethodPost, receivePath, grant, []string{h.proof(t, http.MethodPost, htu(receivePath), grant)}, bytes.NewReader(pushBody(cmdMain)))
			if resp.StatusCode != http.StatusUnauthorized || body != upperFirst(errGrantUsed.Error())+".\n" {
				t.Errorf("the same grant again = %d %q, want 401 used", resp.StatusCode, body)
			}
			if n := len(g.up.requests()); n != 0 {
				t.Errorf("upstream got %d requests", n)
			}
		})
	}
}

// GHAPP_PUBLIC_URL is kept in the canonical form Straza's manifest parser
// stores straza.git.url in, the aud of every grant: scheme and host in lower
// case, no default port, the path as written without a trailing slash.
func TestGitBaseURLCanonical(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://ghapp.example", "https://ghapp.example"},
		{"https://ghapp.example/", "https://ghapp.example"},
		{"https://ghapp.example//", "https://ghapp.example"},
		{"HTTPS://ghapp.example", "https://ghapp.example"},
		{"https://GHAPP.Example", "https://ghapp.example"},
		{"https://ghapp.example:443", "https://ghapp.example"},
		{"https://GHAPP.example:443/", "https://ghapp.example"},
		{"https://ghapp.example:8443", "https://ghapp.example:8443"},
		{"https://ghapp.example:80", "https://ghapp.example:80"},
		{"https://ghapp.example/Base/Git/", "https://ghapp.example/Base/Git"},
		{"https://ghapp.example/a%2Fb", "https://ghapp.example/a%2Fb"},
		{"http://127.0.0.1:8490", "http://127.0.0.1:8490"},
		{"http://127.0.0.1:80/", "http://127.0.0.1"},
		{"http://LOCALHOST:8490/", "http://localhost:8490"},
		{"http://[::1]:8490", "http://[::1]:8490"},
		{"http://[::1]:80", "http://[::1]"},
	} {
		got, err := gitBaseURL(envPublicURL, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("gitBaseURL(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}

	// From the environment: the public URL is canonical, and the issuer is
	// kept as written, because a grant's iss must equal Straza's public URL
	// byte for byte and Straza does not canonicalize that.
	setGitEnv(t, map[string]string{envPublicURL: "https://GHAPP.example:443/"})
	t.Setenv(envGitGrantIssuer, "https://Straza.example/")
	t.Setenv(envGitGrantJWKSURL, "")
	t.Setenv(envGitTickets, "")
	g, err := gitConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if g.PublicURL != "https://ghapp.example" || g.GrantIssuer != "https://Straza.example/" || g.GrantJWKSURL != "https://Straza.example"+strazaJWKSPath {
		t.Errorf("public URL %q, issuer %q, key set %q", g.PublicURL, g.GrantIssuer, g.GrantJWKSURL)
	}
}
