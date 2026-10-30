package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// `fakestraza grants ghapp` stands in for Straza's grant route, POST
// /v1/grants. Straza's holder on the agent's machine asks it for a git grant,
// naming the URL git was given, the operation and, for a push that names
// updates, the exact ref updates. The stand-in decides it like a tool call,
// against the agent's roles in roles.json: git_push for a push, git_fetch for
// a fetch. It signs an allowed grant in the form of Straza's spec/grant with
// its own Ed25519 key, and publishes that key at
// /.well-known/straza/jwks.json, where ghapp reads it. The answer names the
// grant's aud and the repo, which the holder sends git to.
//
// Unlike Straza it does not authenticate the holder: it listens on loopback
// for this demo only, and takes the thumbprint of the holder's key from the
// request's jkt, where Straza takes it from the session token. It holds no
// approval, finds a repo byte for byte where Straza ignores case, and asks
// for no fresh listing. Settings: FAKESTRAZA_GRANTS_LISTEN (host:port) and
// FAKESTRAZA_GIT_AUDIENCE (ghapp's public URL, the grants' aud), with the
// ones of `fakestraza mcp ghapp` for the listing and the roles.

const (
	grantType = "straza-grant+jwt"
	grantKID  = "fakestraza-demo"
	grantTTL  = 120 * time.Second // as long as Straza issues grants for
	jwksPath  = "/.well-known/straza/jwks.json"
	// gitUpstream is the git host the demo's repos live on, as the app's
	// straza.git.upstream names it.
	gitUpstream = "https://github.com"
)

// grantRequest is what the holder asks for: the body of Straza's POST
// /v1/grants, and jkt for this stand-in.
type grantRequest struct {
	Upstream string     `json:"upstream"` // the URL git was given
	Op       string     `json:"op"`       // fetch or push
	Refs     []grantRef `json:"refs,omitempty"`
	JKT      string     `json:"jkt"` // the thumbprint of the holder's key
}

type grantRef struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// grantAnswer is Straza's answer: the grant with its audience, repo, app and
// expiry, or why there is none.
type grantAnswer struct {
	Grant string `json:"grant,omitempty"`
	Aud   string `json:"aud,omitempty"`
	Repo  string `json:"repo,omitempty"`
	Srv   string `json:"srv,omitempty"`
	Exp   string `json:"exp,omitempty"`
	Error string `json:"error,omitempty"`
}

// toolFor is the tool a Straza role binds for a git operation, as the app's
// straza.git.ops maps it.
func toolFor(op string) string {
	if op == "push" {
		return "git_push"
	}
	return "git_fetch"
}

// repoOf reads owner/name from a repo URL under gitUpstream, with .git
// dropped, or returns "".
func repoOf(upstream string) string {
	rest, ok := strings.CutPrefix(upstream, gitUpstream+"/")
	segs := strings.Split(rest, "/")
	if !ok || len(segs) < 2 || segs[0] == "" || strings.TrimSuffix(segs[1], ".git") == "" {
		return ""
	}
	return segs[0] + "/" + strings.TrimSuffix(segs[1], ".git")
}

type grantServer struct {
	g        *gate
	key      ed25519.PrivateKey
	issuer   string
	audience string
	n        atomic.Int64
}

func serveGrants(ctx context.Context, cfg config) error {
	listen := os.Getenv("FAKESTRAZA_GRANTS_LISTEN")
	audience := os.Getenv("FAKESTRAZA_GIT_AUDIENCE")
	if listen == "" || audience == "" {
		return errors.New("set FAKESTRAZA_GRANTS_LISTEN and FAKESTRAZA_GIT_AUDIENCE")
	}
	objs, _, err := listing(ctx, cfg)
	if err != nil {
		return err
	}
	g := &gate{cfg: cfg, objects: map[string]listed{}}
	for _, o := range objs {
		g.objects[o.Value] = o
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	s := &grantServer{g: g, key: key, issuer: "http://" + listen, audience: audience}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+jwksPath, s.jwks)
	mux.HandleFunc("POST /v1/grants", s.grant)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	log.Printf("issuing git grants at %s for %s, with %d listed repos", s.issuer, cfg.agent, len(objs))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *grantServer) jwks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: s.key.Public(), KeyID: grantKID, Algorithm: string(jose.EdDSA), Use: "sig",
	}}})
}

func (s *grantServer) grant(w http.ResponseWriter, r *http.Request) {
	var req grantRequest
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	repo := repoOf(req.Upstream)
	switch {
	case err != nil, req.JKT == "", req.Op != "fetch" && req.Op != "push", req.Op == "fetch" && len(req.Refs) > 0:
		answer(w, http.StatusBadRequest, grantAnswer{Error: "a grant request names the URL git was given, the op fetch or push, " +
			"ref updates on a push only, and the holder's key"})
		return
	case repo == "":
		answer(w, http.StatusNotFound, grantAnswer{Error: fmt.Sprintf("%s is not a repo URL under %s, so this stand-in for Straza gives no git grant for it.",
			req.Upstream, gitUpstream)})
		return
	}
	tool := toolFor(req.Op)
	refs := make([]string, len(req.Refs))
	for i, ref := range req.Refs {
		refs[i] = ref.Ref + " " + short(ref.Old) + ".." + short(ref.New)
	}
	d := s.g.decide(tool, repo, false)
	if !d.allowed {
		s.g.audit(auditLine{Kind: "grant", Tool: tool, Repo: repo, Op: req.Op, Refs: strings.Join(refs, ", "), Decision: "refused"})
		answer(w, http.StatusForbidden, grantAnswer{Error: fmt.Sprintf("%s on %s was refused: none of your roles on %s covers that repo.", tool, repo, s.g.cfg.roles.Server)})
		return
	}
	n := s.n.Add(1)
	id, dec := fmt.Sprintf("grant-%d", n), fmt.Sprintf("dec-%d", n)
	now := time.Now()
	exp := now.Add(grantTTL)
	claims := map[string]any{
		"iss": s.issuer, "aud": s.audience, "sub": s.g.cfg.agent,
		"act": map[string]any{"ses": "demo-session", "kind": "agent"},
		"iat": now.Unix(), "exp": exp.Unix(), "jti": id,
		"cnf": map[string]any{"jkt": req.JKT}, "srv": s.g.cfg.roles.Server,
		"obj": map[string]any{"type": "repo", "value": repo, "id": s.g.objects[repo].ID},
		"op":  req.Op, "dec": dec,
	}
	if len(req.Refs) > 0 {
		claims["refs"] = req.Refs
	}
	grant, err := s.sign(claims)
	if err != nil {
		answer(w, http.StatusInternalServerError, grantAnswer{Error: err.Error()})
		return
	}
	s.g.audit(auditLine{Kind: "grant", Tool: tool, Repo: repo, Op: req.Op, Refs: strings.Join(refs, ", "), Decision: "allowed",
		Role: d.role, ObjectID: s.g.objects[repo].ID, GrantID: id, DecisionID: dec})
	answer(w, http.StatusOK, grantAnswer{Grant: grant, Aud: s.audience, Repo: repo, Srv: s.g.cfg.roles.Server, Exp: exp.UTC().Format(time.RFC3339)})
}

func (s *grantServer) sign(claims map[string]any) (string, error) {
	opts := (&jose.SignerOptions{}).WithType(grantType).WithHeader(jose.HeaderKey("kid"), grantKID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: s.key}, opts)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func answer(w http.ResponseWriter, status int, a grantAnswer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(a)
}

// short is a commit id's first seven characters.
func short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}
