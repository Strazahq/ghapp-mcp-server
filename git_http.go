package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// ghapp's git endpoint. Straza's holder on the agent's machine talks git's
// smart HTTP to /git/{owner}/{name}.git/ with a Straza grant and a DPoP proof
// for each request (git_grant.go), and ghapp passes each request on to
// GitHub's git server with an installation token for exactly the repo id
// Straza approved. The token never leaves ghapp, and neither the grant nor
// the proof reaches GitHub. Bodies stream in both directions and are never
// held whole.
//
//	GET  /git/{owner}/{name}.git/info/refs?service=git-upload-pack|git-receive-pack
//	POST /git/{owner}/{name}.git/git-upload-pack
//	POST /git/{owner}/{name}.git/git-receive-pack
//
// The endpoint does not use the MCP bearer: the holder brings a grant and
// nothing else. A grant names the repo by id as well, so ghapp checks the
// name and the id as for a tool call through Straza, and mints the token for
// that id.
//
// Each proof opens one request. A push grant is spent by the POST that
// carries its ref updates, before the token, refused or not. A fetch grant
// is spent when a pack starts in the answer: a clone with protocol v2 POSTs
// ls-refs first, and a fetch may POST several negotiation rounds, so neither
// spends it. A fetch that finds nothing new gets no pack and leaves its
// grant unspent until it expires. Spent ids are kept per instance
// (usedIDs).

const (
	svcUploadPack  = "git-upload-pack"
	svcReceivePack = "git-receive-pack"
	gitCopyBuffer  = 32 << 10
	// gitTokenUser is the user name GitHub takes an installation token
	// with over git's HTTP.
	gitTokenUser = "x-access-token"
)

// gitRequestHeaders pass from git to GitHub. Nothing else does: not the
// grant in Authorization, not the DPoP proof, not cookies, not forwarding
// headers.
var gitRequestHeaders = []string{"Git-Protocol", "Content-Type", "Content-Encoding", "Accept", "User-Agent"}

// gitResponseHeaders pass from GitHub to git. Not WWW-Authenticate, not
// Location, not Set-Cookie.
var gitResponseHeaders = []string{"Content-Type", "Content-Encoding", "Cache-Control", "Expires", "Pragma"}

// gitEndpoint serves git's smart HTTP at /git/.
type gitEndpoint struct {
	gh       github.API
	tokens   *TokenCache // one token per agent, repo and permission set, as the tools
	grants   *grantVerifier
	resolver *tools // the repo checks of the tools
	// used remembers each spent grant and proof, per instance.
	used      spentStore
	readOnly  bool   // refuse every push, whatever opens it
	publicURL string // the URL agents reach ghapp at, a proof's htu base
	upstream  string // GitHub's git server, without a trailing slash
	maxBody   int64
	timeout   time.Duration
	rt        http.RoundTripper // never follows a redirect, as no RoundTripper does
	log       *slog.Logger
	now       func() time.Time // the grant clock, replaced in tests
}

// newGitEndpoint returns the endpoint for cfg. Its transport asks GitHub
// for no compression, so bytes pass through as GitHub sends them.
func newGitEndpoint(gh github.API, cfg GitConfig) *gitEndpoint {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	// The audit log the tools write: a git line and its mints are JSON
	// lines on stderr beside "tool call" and "token minted".
	audit := newAuditLogger(os.Stderr)
	resolver := newTools(gh, Config{}, audit)
	g := &gitEndpoint{
		gh:        gh,
		tokens:    resolver.tokens,
		grants:    newGrantVerifier(cfg.GrantIssuer, cfg.GrantJWKSURL, cfg.PublicURL, audit),
		resolver:  resolver,
		used:      newUsedIDs(usedIDsMax),
		publicURL: cfg.PublicURL,
		upstream:  strings.TrimRight(cfg.Upstream, "/"),
		maxBody:   cfg.MaxPushBytes,
		timeout:   cfg.Timeout,
		rt:        tr,
		log:       audit,
		now:       time.Now,
	}
	return g
}

// withGitEndpoint serves git at /git/ and everything else with next.
func withGitEndpoint(next, git http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/git/", git)
	mux.Handle("/", next)
	return mux
}

// logGitState describes git transport for the startup log.
func logGitState(g GitConfig) {
	log.Printf("git transport at %s/git/ to %s, pushes up to %d bytes, %s per request",
		g.PublicURL, g.Upstream, g.MaxPushBytes, g.Timeout)
	log.Printf("git transport takes Straza grants from %s, with keys from %s", g.GrantIssuer, g.GrantJWKSURL)
}

// gitRoute is what a request's path and query name.
type gitRoute struct {
	Repo     string // owner/name as the path spells it
	Service  string // svcUploadPack or svcReceivePack
	InfoRefs bool   // the GET of info/refs, rather than the service's POST
}

// op is the grant operation the service needs.
func (rt gitRoute) op() string {
	if rt.Service == svcReceivePack {
		return gitOpPush
	}
	return gitOpFetch
}

func (rt gitRoute) step() string {
	if rt.InfoRefs {
		return "info/refs"
	}
	return rt.Service
}

// upstreamPath is the path at GitHub's git server, with the query.
func (rt gitRoute) upstreamPath() string {
	if rt.InfoRefs {
		return "/" + rt.Repo + ".git/info/refs?service=" + rt.Service
	}
	return "/" + rt.Repo + ".git/" + rt.Service
}

// parseGitRoute reads the route of r, or returns the status and the line
// to answer with. The repo is taken from the path as it is, with or
// without ".git": no case folding and no escapes.
func parseGitRoute(r *http.Request) (gitRoute, int, string) {
	const usage = "ghapp's git endpoint serves /git/{owner}/{name}.git/ for git's smart HTTP protocol."
	rest, ok := strings.CutPrefix(r.URL.Path, "/git/")
	segs := strings.Split(rest, "/")
	if !ok || r.URL.RawPath != "" || len(segs) < 3 {
		return gitRoute{}, http.StatusNotFound, usage
	}
	rt := gitRoute{Repo: segs[0] + "/" + strings.TrimSuffix(segs[1], ".git")}
	if checkRepoName(rt.Repo) != nil {
		return gitRoute{}, http.StatusNotFound, usage
	}
	switch tail := strings.Join(segs[2:], "/"); tail {
	case "info/refs":
		q := r.URL.Query()
		rt.InfoRefs, rt.Service = true, q.Get("service")
		if r.Method != http.MethodGet {
			return gitRoute{}, http.StatusMethodNotAllowed, "info/refs takes GET."
		}
		if len(q) != 1 || len(q["service"]) != 1 || (rt.Service != svcUploadPack && rt.Service != svcReceivePack) {
			return gitRoute{}, http.StatusForbidden, "ghapp serves git's smart HTTP protocol only: info/refs needs ?service=git-upload-pack or ?service=git-receive-pack."
		}
	case svcUploadPack, svcReceivePack:
		rt.Service = tail
		if r.Method != http.MethodPost {
			return gitRoute{}, http.StatusMethodNotAllowed, tail + " takes POST."
		}
	default:
		return gitRoute{}, http.StatusNotFound, usage
	}
	return rt, 0, ""
}

// gitEvent is what one request's log line says.
type gitEvent struct {
	route     gitRoute
	grant     *grantClaims // a grant was verified, with its proof
	proof     checkedProof
	spent     bool // this request used the grant up
	refs      []pushCommand
	in        *countingReader
	tokenHash string
	outcome   string
	reason    string
}

func (g *gitEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rt, status, msg := parseGitRoute(r)
	if status != 0 {
		if status == http.StatusMethodNotAllowed {
			allow := http.MethodPost
			if strings.HasSuffix(r.URL.Path, "/info/refs") {
				allow = http.MethodGet
			}
			w.Header().Set("Allow", allow)
		}
		gitPlain(w, status, msg)
		return
	}

	// A generous bound on the whole request, the body read and the answer
	// written included, so a stalled client or GitHub does not hold the
	// request open for ever.
	ctx, cancel := context.WithTimeout(r.Context(), g.timeout)
	defer cancel()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(start.Add(g.timeout))
	_ = rc.SetWriteDeadline(start.Add(g.timeout))
	// The server sets no write deadline of its own between requests on one
	// connection, so this one must not outlive the request.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	ev := &gitEvent{route: rt}
	gw := &gitResponse{ResponseWriter: w}
	defer func() { g.logEvent(ev, gw, time.Since(start)) }()
	if g.readOnly && rt.Service == svcReceivePack {
		// git_push is not offered in read-only mode, but a grant from
		// elsewhere could still ask for a push here.
		ev.outcome, ev.reason = "refused", "read-only"
		gitPlain(gw, http.StatusForbidden, "This ghapp is read-only: it takes no pushes.")
		return
	}
	g.serve(ctx, gw, r, ev)
}

func (g *gitEndpoint) serve(ctx context.Context, w *gitResponse, r *http.Request, ev *gitEvent) {
	rt := ev.route
	scheme, cred, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, authSchemeDPoP) {
		ev.outcome = "no-credentials"
		if scheme != "" {
			ev.outcome, ev.reason = "unauthorized", "not a Straza grant"
		}
		g.unauthorized(w, "ghapp takes Straza grants only: run git through Straza's holder on this machine.")
		return
	}
	if !g.checkGrant(ctx, w, r, ev, strings.TrimSpace(cred)) {
		return
	}
	if key, _ := ev.once(); g.used.isUsed(key) {
		g.refuseUsed(w, ev)
		return
	}
	if g.used.full(g.now()) {
		ev.outcome, ev.reason = "busy", "the set of used grants is full"
		gitPlain(w, http.StatusServiceUnavailable, "ghapp did not run this git request, because it already remembers as many used "+
			"grants as it can until some expire. Try again in a few minutes.")
		return
	}

	header := http.Header{}
	for _, name := range gitRequestHeaders {
		if v := r.Header.Values(name); len(v) > 0 {
			header[http.CanonicalHeaderKey(name)] = slices.Clone(v)
		}
	}
	var body io.Reader
	length := int64(0)
	if !rt.InfoRefs {
		// The server's own writer, so that a body over the cap also closes
		// the connection.
		ev.in = &countingReader{r: http.MaxBytesReader(w.ResponseWriter, r.Body, g.maxBody)}
		body, length = ev.in, r.ContentLength
		if rt.Service == svcReceivePack {
			// The ref updates are read and checked before any token exists.
			var ok bool
			if body, length, ok = g.checkPush(w, ev, header, length); !ok {
				return
			}
		}
	}

	// A push with ref updates spends its grant here, before the token, so a
	// push refused by the repo checks or a failed mint uses it up, as
	// spec/grant says ("refused or not"). A push without them, such as the
	// probe git sends before a large push, does not, and neither does
	// info/refs.
	push := rt.Service == svcReceivePack && !rt.InfoRefs && len(ev.refs) > 0
	if push && !g.spend(ev) {
		g.refuseUsed(w, ev)
		return
	}
	tok, ok := g.token(ctx, w, ev)
	if !ok {
		return
	}
	ev.tokenHash = hashedToken(tok.Value)
	g.forward(ctx, w, r.Method, ev, header, body, length, tok)
}

// checkGrant verifies a Straza grant and the request's DPoP proof, and that
// the grant opens this route on this repo. The proof is spent here: each
// one opens one request. A refusal is answered here, and it returns false.
func (g *gitEndpoint) checkGrant(ctx context.Context, w http.ResponseWriter, r *http.Request, ev *gitEvent, raw string) bool {
	rt := ev.route
	now := g.now()
	c, err := g.grants.verify(ctx, raw, now)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, errGrantKeys) {
			status = http.StatusServiceUnavailable
		}
		g.refuseGrant(w, ev, status, err)
		return false
	}
	ev.grant = &c
	proofs := r.Header.Values("DPoP")
	if len(proofs) != 1 {
		g.refuseGrant(w, ev, http.StatusUnauthorized, errProofMissing)
		return false
	}
	p, err := verifyProof(proofs[0], r.Method, g.publicURL+r.URL.Path, raw, c.Cnf.JKT, now)
	if err != nil {
		g.refuseGrant(w, ev, http.StatusUnauthorized, err)
		return false
	}
	if !g.used.use("proof "+p.jkt+" "+p.jti, p.spentUntil(), now) {
		g.refuseGrant(w, ev, http.StatusUnauthorized, errProofUsed)
		return false
	}
	ev.proof = p
	if why := c.allows(rt); why != "" {
		g.refuseGrant(w, ev, http.StatusForbidden, errors.New(why))
		return false
	}
	if c.Obj.Value != rt.Repo {
		g.refuseGrant(w, ev, http.StatusForbidden, fmt.Errorf("the grant is for %s, not %s", c.Obj.Value, rt.Repo))
		return false
	}
	return true
}

// refuseGrant answers a request whose grant or proof ghapp refuses. The log
// keeps a proof's own reason; git hears the sentence.
func (g *gitEndpoint) refuseGrant(w http.ResponseWriter, ev *gitEvent, status int, err error) {
	ev.outcome, ev.reason = "unauthorized", err.Error()
	if status == http.StatusForbidden {
		ev.outcome = "refused"
	}
	var pe *proofError
	if errors.As(err, &pe) {
		ev.reason = "the DPoP proof is not valid: " + pe.reason
	}
	if status == http.StatusUnauthorized {
		g.unauthorized(w, upperFirst(err.Error())+".")
		return
	}
	gitPlain(w, status, upperFirst(err.Error())+".")
}

// token mints the request's GitHub token through the repo checks of the
// tools, for the id Straza approved, with the grant's user and session as
// the caller. A failure is answered here, and ok is false.
func (g *gitEndpoint) token(ctx context.Context, w http.ResponseWriter, ev *gitEvent) (tok github.Token, ok bool) {
	perms := gitOpPerms(ev.route.op())
	c := ev.grant
	ctx = withCaller(ctx, Caller{Kind: callerGrant, Client: c.actor(), Subject: c.Sub})
	repo, id := ev.route.Repo, int64(c.Obj.ID)
	acc, err := g.resolver.resolve(ctx, repo, id, true, perms, false)
	if errors.Is(err, errStale) {
		g.resolver.repos.forget(repo, id)
		acc, err = g.resolver.resolve(ctx, repo, id, true, perms, true)
	}
	var ce *CodedError
	switch {
	case errors.As(err, &ce):
		ev.outcome, ev.reason = "refused", err.Error()
		gitPlain(w, http.StatusForbidden, err.Error())
		return github.Token{}, false
	case err != nil:
		ev.outcome, ev.reason = "no-token", err.Error()
		gitPlain(w, http.StatusBadGateway, fmt.Sprintf("ghapp could not get a GitHub token for %s. Try again.", repo))
		return github.Token{}, false
	}
	return acc.Token, true
}

// once is what the request spends, the grant's jti, and until when it is
// remembered.
func (ev *gitEvent) once() (string, time.Time) {
	return "grant " + ev.grant.Jti, ev.grant.expires().Add(grantClockSkew)
}

// unauthorized answers 401 with the DPoP challenge. There is no Basic
// challenge, so git asks no credential helper.
func (g *gitEndpoint) unauthorized(w http.ResponseWriter, line string) {
	w.Header().Set("WWW-Authenticate", authSchemeDPoP+` algs="EdDSA"`)
	gitPlain(w, http.StatusUnauthorized, line)
}

// spend marks the request's grant used. It reports false when another
// request used it first.
func (g *gitEndpoint) spend(ev *gitEvent) bool {
	key, until := ev.once()
	if !g.used.use(key, until, g.now()) {
		return false
	}
	ev.spent = true
	return true
}

// refuseUsed answers a request whose grant was used.
func (g *gitEndpoint) refuseUsed(w http.ResponseWriter, ev *gitEvent) {
	ev.outcome, ev.reason = "unauthorized", "the grant was used"
	g.unauthorized(w, upperFirst(errGrantUsed.Error())+".")
}

// checkPush reads the ref updates of a push and refuses what v1 does not
// allow. It returns the body to send upstream: the bytes it read, then the
// rest, as one stream. A refusal is answered here, and ok is false.
func (g *gitEndpoint) checkPush(w *gitResponse, ev *gitEvent, header http.Header, length int64) (body io.Reader, n int64, ok bool) {
	src := io.Reader(ev.in)
	switch enc := header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		// git does not compress a push, but a client may. ghapp reads the
		// commands from the plain stream, and sends that on plain.
		zr, err := gzip.NewReader(src)
		if err != nil {
			g.pushFailed(w, ev, malformed("the body is not valid gzip"))
			return nil, 0, false
		}
		src = http.MaxBytesReader(w.ResponseWriter, zr, g.maxBody)
		header.Del("Content-Encoding")
		length = -1
	default:
		ev.outcome, ev.reason = "refused", "Content-Encoding "+enc
		gitPlain(w, http.StatusUnsupportedMediaType, "ghapp reads a push sent plain or in gzip only.")
		return nil, 0, false
	}

	push, err := readPushRequest(src)
	if err != nil {
		g.pushFailed(w, ev, err)
		return nil, 0, false
	}
	ev.refs = push.Commands
	if ev.grant != nil && len(ev.grant.Refs) > 0 && len(push.Commands) == 0 {
		// A grant with refs opens only the push of those updates, and a push
		// without updates takes a push grant without refs (spec/grant, The
		// git binding). The grant is not spent: its own push may follow.
		ev.outcome, ev.reason = "refused", reasonRefsNoCommands
		if _, err := io.Copy(io.Discard, src); err != nil {
			g.pushFailed(w, ev, err)
			return nil, 0, false
		}
		gitPlain(w, http.StatusForbidden, upperFirst(reasonRefsNoCommands)+".")
		return nil, 0, false
	}
	reasons := push.refusals()
	if reasons == nil && ev.grant != nil && len(push.Commands) > 0 {
		// A grant opens exactly the updates it names. Its old commits make
		// each update a compare-and-swap at GitHub.
		reasons = grantedRefusals(push.Commands, ev.grant.Refs)
	}
	if reasons == nil {
		return io.MultiReader(bytes.NewReader(push.Head), src), length, true
	}

	// A refused push spends its grant too: the operation is over.
	if !g.spend(ev) {
		g.refuseUsed(w, ev)
		return nil, 0, false
	}
	ev.outcome = "refused"
	for _, r := range reasons {
		if r != reasonOtherRefused {
			ev.reason = r
			break
		}
	}
	// Read the rest of the push, the pack, and drop it. git sends the
	// whole request before it reads the answer, so an answer to a request
	// cut short would reach git as a broken connection, not as a reason.
	if _, err := io.Copy(io.Discard, src); err != nil {
		g.pushFailed(w, ev, err)
		return nil, 0, false
	}
	if !push.wantsReport() {
		gitPlain(w, http.StatusForbidden, "ghapp refused this push: "+ev.reason+".")
		return nil, 0, false
	}
	// git prints each ref with its reason, as for a declined hook.
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(push.reportStatus(reasons))
	return nil, 0, false
}

// pushFailed answers a push ghapp cannot read or that is too large.
func (g *gitEndpoint) pushFailed(w http.ResponseWriter, ev *gitEvent, err error) {
	status, line := http.StatusBadRequest, upperFirst(err.Error())+"."
	var mbe *http.MaxBytesError
	var bad *pushMalformed
	switch {
	case errors.As(err, &mbe):
		status, line = http.StatusRequestEntityTooLarge, fmt.Sprintf("This push is larger than %d bytes, ghapp's limit (%s).", g.maxBody, envGitMaxPushBytes)
	case errors.Is(err, errPushHeadTooLarge), errors.Is(err, errPushTooManyRefs):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, errPushSigned):
		status = http.StatusForbidden
	case errors.As(err, &bad):
	default:
		// The body broke off: the client went away or the deadline passed.
		line = "ghapp could not read the push."
	}
	if ev.outcome == "" {
		ev.outcome = "refused"
	}
	ev.reason = err.Error()
	gitPlain(w, status, line)
}

// forward sends the request to GitHub with tok and streams the answer
// back. A redirect is refused, never followed.
func (g *gitEndpoint) forward(ctx context.Context, w *gitResponse, method string, ev *gitEvent, header http.Header, body io.Reader, length int64, tok github.Token) {
	rt := ev.route
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, method, g.upstream+rt.upstreamPath(), nil)
	if err != nil {
		ev.outcome, ev.reason = "error", err.Error()
		gitPlain(w, http.StatusInternalServerError, "ghapp could not build the request to GitHub.")
		return
	}
	out.Header = header
	if body != nil {
		closed := make(chan struct{})
		out.Body = &closeSignal{r: body, closed: closed}
		out.ContentLength = length
		if length <= 0 {
			out.ContentLength = -1
		}
		// The transport may still read the body after RoundTrip returns.
		// It must stop before this handler returns, since the body is the
		// client's request: cancel, then wait for the transport to close it.
		defer func() { cancel(); <-closed }()
	}
	out.SetBasicAuth(gitTokenUser, tok.Value)

	resp, err := g.rt.RoundTrip(out)
	if err != nil {
		var mbe *http.MaxBytesError
		switch {
		case ev.in != nil && errors.As(ev.in.firstErr(), &mbe):
			ev.outcome, ev.reason = "too-large", fmt.Sprintf("the request is larger than %d bytes", g.maxBody)
			gitPlain(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("This request is larger than %d bytes, ghapp's limit (%s).", g.maxBody, envGitMaxPushBytes))
		case ctx.Err() != nil:
			ev.outcome, ev.reason = "timeout", err.Error()
			gitPlain(w, http.StatusGatewayTimeout, "GitHub's git server did not answer in time.")
		default:
			ev.outcome, ev.reason = "upstream-unreachable", err.Error()
			gitPlain(w, http.StatusBadGateway, "ghapp could not reach GitHub's git server. Try again.")
		}
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		ev.outcome, ev.reason = "refused", "GitHub answered with a redirect"
		gitPlain(w, http.StatusForbidden, fmt.Sprintf("GitHub answered %s with a redirect, and ghapp does not follow it: the repo may have been renamed or moved. Use its new name.", rt.Repo))
		return
	case resp.StatusCode == http.StatusUnauthorized:
		// The token is ghapp's, so this is no reason for git to ask for
		// credentials: GitHub's challenge does not reach git.
		ev.outcome, ev.reason = "upstream-401", "GitHub refused the App's token"
		gitPlain(w, http.StatusBadGateway, fmt.Sprintf("GitHub refused the App's token for %s. Try again.", rt.Repo))
		return
	}

	// An upload-pack answer spends the grant when a pack starts in it.
	src := io.Reader(resp.Body)
	if rt.Service == svcUploadPack && !rt.InfoRefs && resp.StatusCode == http.StatusOK {
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
			// ghapp asks for no compression and cannot look into a
			// compressed answer, so it takes one to hold a pack.
			if !g.spend(ev) {
				g.refuseUsed(w, ev)
				return
			}
		} else {
			src = &packSpotter{r: resp.Body, onPack: func() error {
				if !g.spend(ev) {
					return errGrantUsed
				}
				return nil
			}}
		}
	}

	for _, name := range gitResponseHeaders {
		if v := resp.Header.Values(name); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(name)] = slices.Clone(v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = copyFlush(w, src)
	switch {
	case errors.Is(err, errGrantUsed):
		// Another request with this grant got its pack first. The answer is
		// cut before any of this pack reaches git.
		ev.outcome, ev.reason = "refused", "the grant was used by another request"
	case err != nil:
		ev.outcome, ev.reason = "broken", err.Error()
	case resp.StatusCode >= 400:
		ev.outcome = fmt.Sprintf("upstream-%d", resp.StatusCode)
	default:
		ev.outcome = "ok"
	}
}

// copyFlush copies r to w and flushes after every read, so git sees
// GitHub's progress as it comes, and nothing is held whole.
func copyFlush(w http.ResponseWriter, r io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, gitCopyBuffer)
	var total int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// gitPlain answers with one plain line, which git prints as "remote: ...".
func gitPlain(w http.ResponseWriter, status int, line string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, line+"\n")
}

// hashedToken is base64(sha256(token)), the form of hashed_token in
// GitHub's audit log, so a log line here finds the event there.
func hashedToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// logEvent writes the one line of a git request. It never holds the grant,
// the proof or the token, only the token's hash. A request without
// credentials, which git sends first every time, is logged at debug level
// only.
func (g *gitEndpoint) logEvent(ev *gitEvent, w *gitResponse, took time.Duration) {
	level := slog.LevelInfo
	if ev.outcome == "no-credentials" {
		level = slog.LevelDebug
	}
	attrs := []slog.Attr{
		slog.String("op", ev.route.op()),
		slog.String("step", ev.route.step()),
		slog.String("repo", ev.route.Repo),
	}
	if c := ev.grant; c != nil {
		// The grant's ids, so a line here finds Straza's decision and
		// approval, and never the grant itself.
		attrs = append(attrs, slog.String("via", "grant"), slog.String("grant_id", c.Jti), slog.String("grant_op", c.Op),
			slog.String("srv", c.Srv), slog.Int64("rid", int64(c.Obj.ID)), slog.String("sub", c.Sub), slog.String("act", c.actor()))
		if c.Dec != "" {
			attrs = append(attrs, slog.String("decision", c.Dec))
		}
		if c.Apr != "" {
			attrs = append(attrs, slog.String("approval", c.Apr))
		}
		if ev.proof.jkt != "" {
			attrs = append(attrs, slog.String("jkt", ev.proof.jkt))
		}
	}
	if ev.spent {
		attrs = append(attrs, slog.Bool("spent", true))
	}
	if len(ev.refs) > 0 {
		refs := make([]string, len(ev.refs))
		for i, c := range ev.refs {
			refs[i] = c.String()
		}
		attrs = append(attrs, slog.String("refs", strings.Join(refs, ", ")))
	}
	var in int64
	if ev.in != nil {
		in = ev.in.count()
	}
	attrs = append(attrs,
		slog.Int64("bytes_in", in),
		slog.Int64("bytes_out", w.n),
		slog.Duration("duration", took),
		slog.Int("status", w.status),
		slog.String("outcome", ev.outcome),
	)
	if ev.reason != "" {
		attrs = append(attrs, slog.String("reason", ev.reason))
	}
	if ev.tokenHash != "" {
		attrs = append(attrs, slog.String("hashed_token", ev.tokenHash))
	}
	g.log.LogAttrs(context.Background(), level, "git", attrs...)
}

// gitResponse counts what goes to git and keeps the status.
type gitResponse struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *gitResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gitResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the connection.
func (w *gitResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// countingReader counts what it reads and keeps the first error other than
// EOF. The transport may read it from another goroutine.
type countingReader struct {
	r   io.Reader
	mu  sync.Mutex
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	return n, err
}

func (c *countingReader) count() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingReader) firstErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// packSpotter passes an upload-pack answer through and calls onPack once,
// when a pack starts: the "packfile" section of protocol v2, a side-band
// line of protocol v0, or a bare "PACK" after the acknowledgments. The
// answers to ls-refs and to a negotiation round that is not done hold none.
// If onPack fails, Read returns its error in place of the bytes that start
// the pack. It reads pkt-lines only up to the pack, or to anything it cannot
// read, and keeps no more than one unfinished pkt-line.
type packSpotter struct {
	r      io.Reader
	onPack func() error
	buf    []byte
	done   bool
}

func (s *packSpotter) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && !s.done && s.scan(p[:n]) {
		s.done, s.buf = true, nil
		if perr := s.onPack(); perr != nil {
			return 0, perr
		}
	}
	return n, err
}

// scan reads the pkt-lines that chunk completes and reports whether a pack
// starts. On anything that is not a pkt-line it stops for good.
func (s *packSpotter) scan(chunk []byte) bool {
	s.buf = append(s.buf, chunk...)
	i := 0
	defer func() {
		if !s.done {
			s.buf = append(s.buf[:0], s.buf[i:]...)
		}
	}()
	for len(s.buf)-i >= 4 {
		head := string(s.buf[i : i+4])
		if head == "PACK" {
			return true
		}
		n, err := strconv.ParseUint(head, 16, 16)
		switch {
		case err != nil, n == 3:
			s.done, s.buf = true, nil
			return false
		case n < 3:
			// flush, delim or response-end: no payload.
			i += 4
			continue
		case len(s.buf)-i < int(n):
			return false // the rest of the line is still to come
		}
		payload := s.buf[i+4 : i+int(n)]
		i += int(n)
		switch {
		case len(payload) > 0 && payload[0] >= 1 && payload[0] <= 3, // side-band: pack data, progress or an error
			string(payload) == "packfile\n", string(payload) == "packfile":
			return true
		case bytes.HasPrefix(payload, []byte("ERR ")):
			s.done, s.buf = true, nil
			return false
		}
	}
	return false
}

// closeSignal is a request body that closes a channel once closed.
type closeSignal struct {
	r      io.Reader
	once   sync.Once
	closed chan struct{}
}

func (c *closeSignal) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
