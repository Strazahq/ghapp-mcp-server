package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testIssuer   = "https://straza.test"
	testGrantKID = "straza-2026-10"
	testPublic   = "http://ghapp.test"
)

// fakeStraza signs grants and serves its key set, as Straza does at
// /.well-known/straza/jwks.json.
type fakeStraza struct {
	key     ed25519.PrivateKey
	kid     string
	extra   []map[string]any // more JWKS entries, served before Straza's key
	fetches atomic.Int32
	down    atomic.Bool
	slow    atomic.Bool   // hold each read of the key set until release
	release chan struct{} // closed by let
	let     func()
	srv     *httptest.Server
}

func newFakeStraza(t *testing.T) *fakeStraza {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeStraza{key: key, kid: testGrantKID, release: make(chan struct{})}
	s.let = sync.OnceFunc(func() { close(s.release) })
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fetches.Add(1)
		if s.slow.Load() {
			<-s.release
		}
		if s.down.Load() || r.URL.Path != strazaJWKSPath {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		keys := append([]map[string]any(nil), s.extra...)
		keys = append(keys, map[string]any{
			"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": s.kid,
			"x": b64url.EncodeToString(s.key.Public().(ed25519.PublicKey)),
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(s.srv.Close)
	t.Cleanup(s.let) // before Close, which waits for held reads
	return s
}

func (s *fakeStraza) jwksURL() string { return s.srv.URL + strazaJWKSPath }

// holderKey is the key of Straza's holder on the agent's machine.
type holderKey struct {
	priv ed25519.PrivateKey
	jkt  string
}

func newHolderKey(t *testing.T) holderKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: pub}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return holderKey{priv: priv, jkt: b64url.EncodeToString(thumb)}
}

// signJWS signs claims with key, typ and the given headers.
func signJWS(t *testing.T, key ed25519.PrivateKey, typ string, embed bool, headers map[string]any, claims any) string {
	t.Helper()
	opts := (&jose.SignerOptions{EmbedJWK: embed}).WithType(jose.ContentType(typ))
	for k, v := range headers {
		opts = opts.WithHeader(jose.HeaderKey(k), v)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// grantClaimsFor are the claims of a grant for op on pay-api, bound to h, in
// the form spec/grant pins, issued now for 120 seconds as Straza issues
// them. refs are set only when there are some, as Straza sets them. Tests
// change the claims before signing.
func grantClaimsFor(h holderKey, op string, refs ...grantRef) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss": testIssuer, "aud": testPublic, "sub": "user-42",
		"act": map[string]any{"ses": "sess-7", "hrn": "claude-code/2.1.0", "kind": "agent"},
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(),
		"jti": randomID(), "cnf": map[string]any{"jkt": h.jkt},
		"srv": "ghapp", "obj": map[string]any{"type": "repo", "value": "Strazahq/pay-api", "id": "812345678"},
		"op": op, "dec": "dec-1", "apr": "apr-1",
	}
	if len(refs) > 0 {
		c["refs"] = refs
	}
	return c
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return b64url.EncodeToString(b)
}

func (s *fakeStraza) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	return signJWS(t, s.key, grantType, false, map[string]any{"kid": s.kid}, claims)
}

// proof is the holder's DPoP proof for one request with grant.
func (h holderKey) proof(t *testing.T, method, htu, grant string) string {
	t.Helper()
	return h.proofWith(t, map[string]any{}, method, htu, grant)
}

// proofWith is proof with claims changed by over.
func (h holderKey) proofWith(t *testing.T, over map[string]any, method, htu, grant string) string {
	t.Helper()
	ath := sha256.Sum256([]byte(grant))
	c := map[string]any{"jti": randomID(), "htm": method, "htu": htu, "iat": time.Now().Unix(), "ath": b64url.EncodeToString(ath[:])}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return signJWS(t, h.priv, proofType, true, nil, c)
}

// newGrantGitTest is newGitTest with its fake Straza at hand.
func newGrantGitTest(t *testing.T) (*gitTest, *fakeStraza) {
	t.Helper()
	g := newGitTest(t)
	return g, g.straza
}

// doGrant sends one request with the grant and the proof as given, either
// left out when empty.
func (g *gitTest) doGrant(t *testing.T, method, path, grant string, proofs []string, body io.Reader) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, g.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if grant != "" {
		req.Header.Set("Authorization", "DPoP "+grant)
	}
	for _, p := range proofs {
		req.Header.Add("DPoP", p)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// htu is the URL a proof names for path, without the query.
func htu(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return testPublic + p
}

// pushBody is a receive-pack request that updates refs, with report-status.
func pushBody(cmds ...pushCommand) []byte {
	var b bytes.Buffer
	for i, c := range cmds {
		line := c.Old + " " + c.New + " " + c.Ref
		if i == 0 {
			line += "\x00report-status side-band-64k agent=git/2.47.3"
		}
		writePkt(&b, []byte(line+"\n"))
	}
	b.WriteString("0000PACK-DATA")
	return b.Bytes()
}

var (
	cmdMain = pushCommand{Old: oidA, New: oidB, Ref: "refs/heads/main"}
	refMain = grantRef{Ref: "refs/heads/main", Old: oidA, New: oidB}
)

// A push with grants: info/refs with a push grant without refs, then the
// POST with a push grant that names exactly its updates. The grant without
// refs is never spent, a push grant with refs opens one push, and each proof
// opens one request. The token is for the id Straza approved with contents
// write, keyed by the grant's user and session, and the log names the grant,
// Straza's app and decision without holding the grant.
func TestGitGrantPush(t *testing.T) {
	g, s := newGrantGitTest(t)
	h := newHolderKey(t)

	adv := s.sign(t, grantClaimsFor(h, gitOpPush))
	for i := range 2 {
		resp, body := g.doGrant(t, http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsReceive), adv)}, nil)
		if resp.StatusCode != http.StatusOK || body != advertReceive {
			t.Fatalf("info/refs %d with a push grant without refs = %d %q", i+1, resp.StatusCode, body)
		}
	}
	if g.endpoint.used.isUsed("grant " + jtiOf(t, adv)) {
		t.Error("info/refs spent the push grant without refs")
	}

	push := s.sign(t, grantClaimsFor(h, gitOpPush, refMain))
	proof := h.proof(t, http.MethodPost, htu(receivePath), push)
	resp, body := g.doGrant(t, http.MethodPost, receivePath, push, []string{proof}, bytes.NewReader(pushBody(cmdMain)))
	if resp.StatusCode != http.StatusOK || body != receiveOK {
		t.Fatalf("push = %d %q", resp.StatusCode, body)
	}
	// The same proof again: refused before the grant is looked at.
	resp, body = g.doGrant(t, http.MethodPost, receivePath, push, []string{proof}, bytes.NewReader(pushBody(cmdMain)))
	if resp.StatusCode != http.StatusUnauthorized || body != upperFirst(errProofUsed.Error())+".\n" {
		t.Errorf("a replayed proof = %d %q", resp.StatusCode, body)
	}
	// A new proof with the spent grant.
	resp, body = g.doGrant(t, http.MethodPost, receivePath, push, []string{h.proof(t, http.MethodPost, htu(receivePath), push)}, bytes.NewReader(pushBody(cmdMain)))
	if resp.StatusCode != http.StatusUnauthorized || body != upperFirst(errGrantUsed.Error())+".\n" {
		t.Errorf("a spent push grant = %d %q", resp.StatusCode, body)
	}
	if n := len(g.up.requests()); n != 3 {
		t.Errorf("upstream got %d requests, want 3", n)
	}

	mints, _ := g.f.snapshot()
	var gitMints []mintRec
	for _, m := range mints {
		if m.Perms["contents"] != "" {
			gitMints = append(gitMints, m)
		}
	}
	if len(gitMints) != 1 || gitMints[0].Perms["contents"] != "write" || len(gitMints[0].RepoIDs) != 1 || gitMints[0].RepoIDs[0] != payID {
		t.Errorf("mints = %+v, want one contents write token for pay-api, reused", mints)
	}
	for _, r := range g.up.requests() {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") || r.Header.Get("DPoP") != "" {
			t.Errorf("upstream got Authorization %q DPoP %q, want the App's token only", r.Header.Get("Authorization"), r.Header.Get("DPoP"))
		}
	}

	logs := g.log.String()
	for _, want := range []string{
		"via=grant", "grant_op=push", "srv=ghapp", "rid=812345678", "sub=user-42",
		`act="{\"hrn\":\"claude-code/2.1.0\",\"kind\":\"agent\",\"ses\":\"sess-7\"}"`, "decision=dec-1", "approval=apr-1", "jkt=" + h.jkt,
		"refs=\"refs/heads/main " + oidA + " " + oidB + "\"", "spent=true", "outcome=ok",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %s:\n%s", want, logs)
		}
	}
	for _, secret := range []string{adv, push, proof} {
		if strings.Contains(logs, secret) {
			t.Error("the log holds a grant or a proof")
		}
	}
}

// The token cache keys a grant's token by its user and session: one agent
// session reuses its token, another session gets its own.
func TestGitGrantTokenPerSession(t *testing.T) {
	g, s := newGrantGitTest(t)
	h := newHolderKey(t)
	fetch := func(act map[string]any) {
		t.Helper()
		c := grantClaimsFor(h, gitOpFetch)
		c["act"] = act
		grant := s.sign(t, c)
		resp, body := g.doGrant(t, http.MethodGet, infoRefsUpload, grant, []string{h.proof(t, http.MethodGet, htu(infoRefsUpload), grant)}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("info/refs = %d %q", resp.StatusCode, body)
		}
	}
	fetch(map[string]any{"ses": "a", "kind": "agent"})
	fetch(map[string]any{"ses": "a", "kind": "agent"})
	fetch(map[string]any{"ses": "b", "kind": "agent"})
	// Numbers too large for a float64 stay apart, in a member of act that
	// spec/grant does not name.
	fetch(map[string]any{"ses": "c", "kind": "agent", "n": json.Number("9007199254740992")})
	fetch(map[string]any{"ses": "c", "kind": "agent", "n": json.Number("9007199254740993")})
	mints, _ := g.f.snapshot()
	n := 0
	for _, m := range mints {
		if m.Perms["contents"] == "read" {
			n++
		}
	}
	if n != 4 {
		t.Errorf("contents read mints = %d, want 4, one per session: %+v", n, mints)
	}
}

// Every bad grant or proof is refused before any token or upstream request,
// with a sentence git prints.
func TestGitGrantRefusals(t *testing.T) {
	g, s := newGrantGitTest(t)
	h := newHolderKey(t)
	other := newHolderKey(t)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)

	type req struct {
		method, path, grant string
		proofs              []string
		body                []byte
	}
	signed := func(op string, change func(map[string]any), refs ...grantRef) string {
		c := grantClaimsFor(h, op, refs...)
		if change != nil {
			change(c)
		}
		return s.sign(t, c)
	}
	// get is info/refs of receive-pack with grant and a good proof for it.
	get := func(grant string) req {
		return req{http.MethodGet, infoRefsReceive, grant, []string{h.proof(t, http.MethodGet, htu(infoRefsReceive), grant)}, nil}
	}
	adv := signed(gitOpPush, nil) // a push grant without refs opens this info/refs
	withProof := func(over map[string]any) req {
		return req{http.MethodGet, infoRefsReceive, adv, []string{h.proofWith(t, over, http.MethodGet, htu(infoRefsReceive), adv)}, nil}
	}

	for _, tc := range []struct {
		name   string
		req    req
		status int
		body   string
	}{
		{"not a JWS", get("not-a-grant"), 401, errGrantMalformed.Error()},
		{"wrong typ", get(signJWS(t, s.key, "JWT", false, map[string]any{"kid": s.kid}, grantClaimsFor(h, gitOpPush))), 401, errGrantMalformed.Error()},
		{"no kid", get(signJWS(t, s.key, grantType, false, nil, grantClaimsFor(h, gitOpPush))), 401, errGrantMalformed.Error()},
		{"unknown kid", get(signJWS(t, s.key, grantType, false, map[string]any{"kid": "other"}, grantClaimsFor(h, gitOpPush))), 401, errGrantSignature.Error()},
		{"another key", get(signJWS(t, stranger, grantType, false, map[string]any{"kid": s.kid}, grantClaimsFor(h, gitOpPush))), 401, errGrantSignature.Error()},
		{"another issuer", get(signed(gitOpPush, func(c map[string]any) { c["iss"] = "https://evil.test" })), 401, errGrantNotOurs.Error()},
		{"another audience", get(signed(gitOpPush, func(c map[string]any) { c["aud"] = "https://other-ghapp.test" })), 401, errGrantNotOurs.Error()},
		{"aud as an array", get(signed(gitOpPush, func(c map[string]any) { c["aud"] = []string{testPublic} })), 401, errGrantFields.Error()},
		{"expired", get(signed(gitOpPush, func(c map[string]any) {
			c["iat"], c["exp"] = time.Now().Add(-8*time.Minute).Unix(), time.Now().Add(-3*time.Minute).Unix()
		})), 401, errGrantExpired.Error()},
		{"too long", get(signed(gitOpPush, func(c map[string]any) { c["exp"] = time.Now().Add(time.Hour).Unix() })), 401, errGrantFields.Error()},
		{"no jti", get(signed(gitOpPush, func(c map[string]any) { delete(c, "jti") })), 401, errGrantFields.Error()},
		{"no cnf", get(signed(gitOpPush, func(c map[string]any) { delete(c, "cnf") })), 401, errGrantFields.Error()},
		{"no act", get(signed(gitOpPush, func(c map[string]any) { delete(c, "act") })), 401, errGrantFields.Error()},
		{"act not an object", get(signed(gitOpPush, func(c map[string]any) { c["act"] = "sess-7" })), 401, errGrantFields.Error()},
		{"an empty act", get(signed(gitOpPush, func(c map[string]any) { c["act"] = map[string]any{} })), 401, errGrantFields.Error()},
		{"unknown op", get(signed("delete", nil)), 401, errGrantFields.Error()},
		{"op advertise", get(signed("advertise", nil)), 401, errGrantFields.Error()},
		{"not a repo", get(signed(gitOpPush, func(c map[string]any) { c["obj"].(map[string]any)["type"] = "issue" })), 401, errGrantFields.Error()},
		{"refs on a fetch", get(signed(gitOpFetch, nil, refMain)), 401, errGrantFields.Error()},
		{"a bad ref id", get(signed(gitOpPush, nil, grantRef{Ref: "refs/heads/main", Old: "x", New: oidB})), 401, errGrantFields.Error()},

		{"no proof", req{http.MethodGet, infoRefsReceive, adv, nil, nil}, 401, errProofMissing.Error()},
		{"two proofs", req{http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsReceive), adv), h.proof(t, http.MethodGet, htu(infoRefsReceive), adv)}, nil}, 401, errProofMissing.Error()},
		{"proof by another key", req{http.MethodGet, infoRefsReceive, adv, []string{other.proof(t, http.MethodGet, htu(infoRefsReceive), adv)}, nil}, 401, errProofInvalid.Error()},
		{"proof for another method", req{http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodPost, htu(infoRefsReceive), adv)}, nil}, 401, errProofInvalid.Error()},
		{"proof for another URL", req{http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsUpload)+"x", adv)}, nil}, 401, errProofInvalid.Error()},
		{"proof for another host", req{http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, "http://evil.test"+payGit+"/info/refs", adv)}, nil}, 401, errProofInvalid.Error()},
		{"proof for another grant", req{http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsReceive), adv+"x")}, nil}, 401, errProofInvalid.Error()},
		{"proof too old", withProof(map[string]any{"iat": time.Now().Add(-3 * time.Minute).Unix()}), 401, errProofInvalid.Error()},
		{"proof from the future", withProof(map[string]any{"iat": time.Now().Add(3 * time.Minute).Unix()}), 401, errProofInvalid.Error()},
		{"proof without ath", withProof(map[string]any{"ath": nil}), 401, errProofInvalid.Error()},
		{"proof without jti", withProof(map[string]any{"jti": nil}), 401, errProofInvalid.Error()},
		{"proof not dpop+jwt", req{http.MethodGet, infoRefsReceive, adv, []string{signJWS(t, h.priv, "JWT", true, nil, map[string]any{"jti": "1", "htm": "GET", "htu": htu(infoRefsReceive), "iat": time.Now().Unix()})}, nil}, 401, errProofInvalid.Error()},

		{"a push grant with refs at info/refs", get(signed(gitOpPush, nil, refMain)), 403, "the grant is for push, not for info/refs"},
		{"a fetch grant at info/refs of receive-pack", get(signed(gitOpFetch, nil)), 403, "the grant is for fetch, not for info/refs"},
		{"a push grant at info/refs of upload-pack", func() req {
			return req{http.MethodGet, infoRefsUpload, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsUpload), adv)}, nil}
		}(), 403, "the grant is for push, not for info/refs"},
		{"a fetch grant at the push", func() req {
			fetch := signed(gitOpFetch, nil)
			return req{http.MethodPost, receivePath, fetch, []string{h.proof(t, http.MethodPost, htu(receivePath), fetch)}, pushBody(cmdMain)}
		}(), 403, "the grant is for fetch, not for git-receive-pack"},
		{"another repo", get(signed(gitOpPush, func(c map[string]any) { c["obj"].(map[string]any)["value"] = "Strazahq/other" })), 403, "the grant is for Strazahq/other, not Strazahq/pay-api"},
		{"another id", get(signed(gitOpPush, func(c map[string]any) { c["obj"].(map[string]any)["id"] = "812345679" })), 403, "is not the repo Straza approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(g.up.requests())
			resp, body := g.doGrant(t, tc.req.method, tc.req.path, tc.req.grant, tc.req.proofs, bytes.NewReader(tc.req.body))
			if resp.StatusCode != tc.status || !strings.Contains(strings.ToLower(body), strings.ToLower(tc.body)) {
				t.Errorf("= %d %q, want %d with %q", resp.StatusCode, body, tc.status, tc.body)
			}
			if tc.status == 401 && !strings.Contains(strings.Join(resp.Header.Values("WWW-Authenticate"), ", "), `DPoP algs="EdDSA"`) {
				t.Errorf("WWW-Authenticate = %q, want the DPoP challenge", resp.Header.Values("WWW-Authenticate"))
			}
			if len(g.up.requests()) != before {
				t.Error("upstream reached")
			}
		})
	}
	// The id check mints for the id Straza approved, as for a tool call, and
	// then finds another repo behind it. No token reached pay-api.
	mints, _ := g.f.snapshot()
	for _, m := range mints {
		if m.Perms["contents"] != "" && m.RepoIDs[0] == payID {
			t.Errorf("minted a git token for a refused request: %+v", m)
		}
	}
}

// A push must be exactly the updates its grant names. Anything else is
// refused per ref, with the reason git prints, the grant is spent, and
// nothing reaches GitHub. A push with no updates, git's probe before a
// large push, takes a push grant without refs and spends nothing. A push
// grant with refs does not open it and stays for its own push.
func TestGitGrantRefsMustMatch(t *testing.T) {
	g, s := newGrantGitTest(t)
	h := newHolderKey(t)
	cmdOther := pushCommand{Old: zeroID, New: oidC, Ref: "refs/heads/new"}
	for _, tc := range []struct {
		name    string
		granted []grantRef
		cmds    []pushCommand
		lines   []string
	}{
		{"another new commit", []grantRef{refMain}, []pushCommand{{Old: oidA, New: oidC, Ref: "refs/heads/main"}},
			[]string{"ng refs/heads/main " + reasonNotGranted}},
		{"another old commit", []grantRef{refMain}, []pushCommand{{Old: oidC, New: oidB, Ref: "refs/heads/main"}},
			[]string{"ng refs/heads/main " + reasonNotGranted}},
		{"one more ref", []grantRef{refMain}, []pushCommand{cmdMain, cmdOther},
			[]string{"ng refs/heads/main " + reasonGrantMismatch, "ng refs/heads/new " + reasonNotGranted}},
		{"one ref fewer", []grantRef{refMain, {Ref: cmdOther.Ref, Old: cmdOther.Old, New: cmdOther.New}}, []pushCommand{cmdMain},
			[]string{"ng refs/heads/main " + reasonGrantMismatch}},
		{"a grant without refs", []grantRef{}, []pushCommand{cmdMain},
			[]string{"ng refs/heads/main " + reasonNotGranted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := s.sign(t, grantClaimsFor(h, gitOpPush, tc.granted...))
			resp, body := g.doGrant(t, http.MethodPost, receivePath, grant, []string{h.proof(t, http.MethodPost, htu(receivePath), grant)}, bytes.NewReader(pushBody(tc.cmds...)))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d %q, want 200 with a report", resp.StatusCode, body)
			}
			for _, line := range tc.lines {
				if !strings.Contains(body, line) {
					t.Errorf("report %q lacks %q", body, line)
				}
			}
			if g.endpoint.used.isUsed("grant "+jtiOf(t, grant)) != true {
				t.Error("a refused push left its grant unspent")
			}
		})
	}
	if n := len(g.up.requests()); n != 0 {
		t.Errorf("upstream got %d requests", n)
	}

	// The probe: no ref updates. A push grant without refs opens it and is
	// not spent.
	adv := s.sign(t, grantClaimsFor(h, gitOpPush))
	resp, _ := g.doGrant(t, http.MethodPost, receivePath, adv, []string{h.proof(t, http.MethodPost, htu(receivePath), adv)}, strings.NewReader("0000"))
	if resp.StatusCode != http.StatusOK || g.endpoint.used.isUsed("grant "+jtiOf(t, adv)) {
		t.Errorf("probe with a push grant without refs = %d, spent %v", resp.StatusCode, g.endpoint.used.isUsed("grant "+jtiOf(t, adv)))
	}
	// A push grant with refs opens only the push of its updates: the probe
	// is refused before any token or upstream request, and the grant stays.
	grant := s.sign(t, grantClaimsFor(h, gitOpPush, refMain))
	before := len(g.up.requests())
	resp, body := g.doGrant(t, http.MethodPost, receivePath, grant, []string{h.proof(t, http.MethodPost, htu(receivePath), grant)}, strings.NewReader("0000"))
	if want := upperFirst(reasonRefsNoCommands) + ".\n"; resp.StatusCode != http.StatusForbidden || body != want {
		t.Errorf("probe with a push grant with refs = %d %q, want 403 %q", resp.StatusCode, body, want)
	}
	if g.endpoint.used.isUsed("grant "+jtiOf(t, grant)) || len(g.up.requests()) != before {
		t.Errorf("the refused probe spent the grant (%v) or reached upstream", g.endpoint.used.isUsed("grant "+jtiOf(t, grant)))
	}
	resp, body = g.doGrant(t, http.MethodPost, receivePath, grant, []string{h.proof(t, http.MethodPost, htu(receivePath), grant)}, bytes.NewReader(pushBody(cmdMain)))
	if resp.StatusCode != http.StatusOK || body != receiveOK {
		t.Errorf("push after the probe = %d %q", resp.StatusCode, body)
	}
}

// jtiOf reads a grant's jti without checking it.
func jtiOf(t *testing.T, grant string) string {
	t.Helper()
	jws, err := jose.ParseSignedCompact(grant, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &c); err != nil {
		t.Fatal(err)
	}
	return c.Jti
}

// A fetch grant opens the POSTs of one fetch and is spent when GitHub starts
// sending a pack. info/refs of upload-pack takes it as well and leaves it
// unspent.
func TestGitGrantFetch(t *testing.T) {
	g, s := newGrantGitTest(t)
	h := newHolderKey(t)
	grant := s.sign(t, grantClaimsFor(h, gitOpFetch))
	resp, body := g.doGrant(t, http.MethodGet, infoRefsUpload, grant, []string{h.proof(t, http.MethodGet, htu(infoRefsUpload), grant)}, nil)
	if resp.StatusCode != http.StatusOK || body != advertUpload {
		t.Fatalf("info/refs = %d %q", resp.StatusCode, body)
	}
	resp, body = g.doGrant(t, http.MethodPost, uploadPath, grant, []string{h.proof(t, http.MethodPost, htu(uploadPath), grant)}, strings.NewReader("0000"))
	if resp.StatusCode != http.StatusOK || body != uploadAnswer {
		t.Fatalf("upload-pack = %d %q", resp.StatusCode, body)
	}
	resp, body = g.doGrant(t, http.MethodPost, uploadPath, grant, []string{h.proof(t, http.MethodPost, htu(uploadPath), grant)}, strings.NewReader("0000"))
	if resp.StatusCode != http.StatusUnauthorized || body != upperFirst(errGrantUsed.Error())+".\n" {
		t.Errorf("a second pack with the grant = %d %q", resp.StatusCode, body)
	}
	// A push grant does not open a fetch.
	push := s.sign(t, grantClaimsFor(h, gitOpPush, refMain))
	resp, _ = g.doGrant(t, http.MethodPost, uploadPath, push, []string{h.proof(t, http.MethodPost, htu(uploadPath), push)}, strings.NewReader("0000"))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a push grant at upload-pack = %d, want 403", resp.StatusCode)
	}
}

// A read-only ghapp takes no push, whatever grant comes with it, and
// refuses before anything is checked or minted. Fetches still work.
func TestGitEndpointReadOnly(t *testing.T) {
	g, s := newGrantGitTest(t)
	g.endpoint.readOnly = true
	h := newHolderKey(t)
	adv := s.sign(t, grantClaimsFor(h, gitOpPush))
	push := s.sign(t, grantClaimsFor(h, gitOpPush, refMain))
	for _, tc := range []struct {
		name string
		do   func() (*http.Response, string)
	}{
		{"no credentials", func() (*http.Response, string) {
			return g.do(t, http.MethodPost, receivePath, "", bytes.NewReader(pushBody(cmdMain)), nil)
		}},
		{"info/refs with a grant", func() (*http.Response, string) {
			return g.doGrant(t, http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsReceive), adv)}, nil)
		}},
		{"a push with a grant", func() (*http.Response, string) {
			return g.doGrant(t, http.MethodPost, receivePath, push, []string{h.proof(t, http.MethodPost, htu(receivePath), push)}, bytes.NewReader(pushBody(cmdMain)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := tc.do()
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "read-only") {
				t.Errorf("= %d %q, want 403 read-only", resp.StatusCode, body)
			}
		})
	}
	if g.endpoint.used.isUsed("grant "+jtiOf(t, push)) || g.endpoint.used.isUsed("grant "+jtiOf(t, adv)) {
		t.Error("a refused push spent its grant")
	}
	if mints, _ := g.f.snapshot(); len(mints) != 0 || len(g.up.requests()) != 0 {
		t.Errorf("mints %+v, %d upstream requests, want none", mints, len(g.up.requests()))
	}
	if !strings.Contains(g.log.String(), "reason=read-only") {
		t.Errorf("log lacks reason=read-only:\n%s", g.log.String())
	}

	fetch := s.sign(t, grantClaimsFor(h, gitOpFetch))
	if resp, body := g.doGrant(t, http.MethodGet, infoRefsUpload, fetch, []string{h.proof(t, http.MethodGet, htu(infoRefsUpload), fetch)}, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("a fetch in read-only mode = %d %q", resp.StatusCode, body)
	}
}

// The key set is read once, again for an unknown kid at most every
// keySetMinInterval, kept for keySetMaxStale while Straza cannot be read, and
// a grant is refused with 503 when no key set was ever read.
func TestGrantKeySet(t *testing.T) {
	s := newFakeStraza(t)
	// A key type go-jose does not know does not fail the set.
	s.extra = []map[string]any{{"kty": "unknown", "kid": "x"}, {"kty": "OKP", "crv": "Ed25519", "kid": "enc", "use": "enc", "x": b64url.EncodeToString(make([]byte, 32))}}
	ks := newKeySet(s.jwksURL(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	var mu sync.Mutex
	now := time.Now()
	ks.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	ctx := t.Context()

	if _, err := ks.key(ctx, testGrantKID); err != nil {
		t.Fatalf("key: %v", err)
	}
	if _, err := ks.key(ctx, "enc"); err != errGrantSignature {
		t.Errorf("an encryption key = %v, want it skipped", err)
	}
	if _, err := ks.key(ctx, "other"); err != errGrantSignature {
		t.Errorf("unknown kid = %v", err)
	}
	if n := s.fetches.Load(); n != 1 {
		t.Errorf("fetches = %d, want 1: an unknown kid right after a read reads nothing", n)
	}
	advance(keySetMinInterval)
	_, _ = ks.key(ctx, "other")
	if n := s.fetches.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2", n)
	}
	s.down.Store(true)
	advance(keySetMaxAge)
	if _, err := ks.key(ctx, testGrantKID); err != nil {
		t.Errorf("Straza down, keys kept: %v", err)
	}
	ks.settle()
	if n := s.fetches.Load(); n != 3 {
		t.Errorf("fetches = %d, want 3: an old set is read again", n)
	}
	advance(keySetMaxStale)
	if _, err := ks.key(ctx, testGrantKID); err != errGrantKeys {
		t.Errorf("Straza down past keySetMaxStale = %v, want errGrantKeys", err)
	}

	advance(keySetMinInterval / 2)
	before := s.fetches.Load()
	if _, err := ks.key(ctx, testGrantKID); err != errGrantKeys || s.fetches.Load() != before {
		t.Errorf("Straza down, read again within keySetMinInterval: %v, %d reads", err, s.fetches.Load()-before)
	}

	never := newKeySet(s.jwksURL(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := never.key(ctx, testGrantKID); err != errGrantKeys {
		t.Errorf("never read = %v", err)
	}
	g, s2 := newGrantGitTest(t)
	s2.down.Store(true)
	h := newHolderKey(t)
	adv := s2.sign(t, grantClaimsFor(h, gitOpPush))
	resp, body := g.doGrant(t, http.MethodGet, infoRefsReceive, adv, []string{h.proof(t, http.MethodGet, htu(infoRefsReceive), adv)}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Straza's signing keys") {
		t.Errorf("keys unreadable = %d %q", resp.StatusCode, body)
	}
}

// A read of the key set runs on its own. A known kid is served while it
// runs, a request that needs it and hangs up stops waiting, and the read
// goes on to the end.
func TestGrantKeySetSlowRead(t *testing.T) {
	s := newFakeStraza(t)
	ks := newKeySet(s.jwksURL(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	var mu sync.Mutex
	now := time.Now()
	ks.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	ctx := t.Context()
	if _, err := ks.key(ctx, testGrantKID); err != nil {
		t.Fatalf("key: %v", err)
	}

	s.slow.Store(true)
	mu.Lock()
	now = now.Add(keySetMaxAge)
	mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := ks.key(ctx, testGrantKID); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a known kid while the set is read = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a known kid waited for the read")
	}

	gone, hangUp := context.WithCancel(ctx)
	go func() { _, err := ks.key(gone, "next"); done <- err }()
	hangUp()
	if err := <-done; err != errGrantKeys {
		t.Errorf("a request that hung up = %v, want errGrantKeys", err)
	}
	s.let()
	ks.settle()
	ks.mu.Lock()
	read := ks.fetched.Equal(ks.now())
	ks.mu.Unlock()
	if !read || s.fetches.Load() != 2 {
		t.Errorf("after the hang-up: set read %v, fetches %d, want one read that ended", read, s.fetches.Load())
	}
}

func TestSameURL(t *testing.T) {
	for _, tc := range []struct {
		htu, want string
		same      bool
	}{
		{"https://ghapp.example/git/a/b.git/info/refs", "https://ghapp.example/git/a/b.git/info/refs", true},
		{"HTTPS://GHAPP.example:443/git/a/b.git/info/refs", "https://ghapp.example/git/a/b.git/info/refs", true},
		{"https://ghapp.example/git/a/b.git/info/refs?service=git-upload-pack", "https://ghapp.example/git/a/b.git/info/refs", true},
		{"https://ghapp.example/git/a/B.git/info/refs", "https://ghapp.example/git/a/b.git/info/refs", false},
		{"http://ghapp.example/git/a/b.git/info/refs", "https://ghapp.example/git/a/b.git/info/refs", false},
		{"https://ghapp.example:8443/git/a/b.git/info/refs", "https://ghapp.example/git/a/b.git/info/refs", false},
		{"https://user@ghapp.example/git/a/b.git/info/refs", "https://ghapp.example/git/a/b.git/info/refs", false},
	} {
		if got := sameURL(tc.htu, tc.want); got != tc.same {
			t.Errorf("sameURL(%q, %q) = %v", tc.htu, tc.want, got)
		}
	}
}

func TestGitGrantConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want GitConfig
		err  string
	}{
		{"issuer", map[string]string{envPublicURL: "https://ghapp.example", envGitGrantIssuer: "https://straza.example"},
			GitConfig{GrantIssuer: "https://straza.example", GrantJWKSURL: "https://straza.example" + strazaJWKSPath}, ""},
		{"own JWKS URL", map[string]string{envPublicURL: "https://ghapp.example", envGitGrantIssuer: "https://straza.example", envGitGrantJWKSURL: "http://127.0.0.1:8080/keys"},
			GitConfig{GrantIssuer: "https://straza.example", GrantJWKSURL: "http://127.0.0.1:8080/keys"}, ""},
		{"no public URL", map[string]string{envGitGrantIssuer: "https://straza.example"}, GitConfig{}, envGitGrantIssuer + " is set but " + envPublicURL},
		{"JWKS without issuer", map[string]string{envPublicURL: "https://ghapp.example", envGitGrantJWKSURL: "https://straza.example/keys"}, GitConfig{}, "is set but " + envGitGrantIssuer},
		{"http issuer", map[string]string{envPublicURL: "https://ghapp.example", envGitGrantIssuer: "http://straza.example"}, GitConfig{}, "must use https"},
		{"http JWKS", map[string]string{envPublicURL: "https://ghapp.example", envGitGrantIssuer: "https://straza.example", envGitGrantJWKSURL: "http://straza.example/keys"}, GitConfig{}, "https URL"},
		{"public URL without issuer", map[string]string{envPublicURL: "https://ghapp.example"}, GitConfig{}, envPublicURL + " is set but " + envGitGrantIssuer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{envPublicURL, envGitTicketKey, envGitUpstream, envGitMaxPushBytes, envGitTimeout, envGitGrantIssuer, envGitGrantJWKSURL, envGitTickets} {
				t.Setenv(name, tc.env[name])
			}
			got, err := gitConfigFromEnv()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.GrantIssuer != tc.want.GrantIssuer || got.GrantJWKSURL != tc.want.GrantJWKSURL || !got.On() {
				t.Errorf("= %q %q", got.GrantIssuer, got.GrantJWKSURL)
			}
		})
	}
}
