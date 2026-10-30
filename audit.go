package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// ghapp's own audit log: one JSON line per mint, "token minted", and one per
// tool call, "tool call", on stderr. A line names the caller, the repo and
// the token's hash, which GitHub's audit log records as hashed_token, so a
// GitHub event maps back to the agent behind it. A line never holds a
// token, a grant, a secret or an argument other than repo.

// newAuditLogger writes JSON lines to w.
func newAuditLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// callAudit is one attempt of one tool call, carried in its context. access
// fills it in, the audit middleware reads it for the log line and to decide
// on a retry from cold.
type callAudit struct {
	cold    bool // this attempt skips the lookup caches
	retried bool // this attempt is the retry from cold

	repo      string // the repo argument access worked on
	repoID    int64  // the repo id access resolved
	repoIDBy  string // who named that id: repoIDByStraza or repoIDByLookup
	tokenHash string // the hash of the token the tool's call goes out with
	reused    bool   // that token was not minted for this call
	warm      bool   // access took the repo from a lookup cache, without the read that confirms it
	reusedAny bool   // some token this attempt used came from the cache
}

// Who named the repo id of a call, in the "tool call" line's repo_id_from:
// Straza, in params._meta, or ghapp's own lookup of the name.
const (
	repoIDByStraza = "straza"
	repoIDByLookup = "lookup"
)

type auditKey struct{}

func withAudit(ctx context.Context, a *callAudit) context.Context {
	return context.WithValue(ctx, auditKey{}, a)
}

// auditFrom is the attempt ctx belongs to, or nil outside a tool call.
func auditFrom(ctx context.Context) *callAudit {
	a, _ := ctx.Value(auditKey{}).(*callAudit)
	return a
}

// noteTokenUse records that the attempt used a token from the cache.
func noteTokenUse(ctx context.Context, reused bool) {
	if a := auditFrom(ctx); a != nil && reused {
		a.reusedAny = true
	}
}

// noteCallToken records the token the tool's own call goes out with.
func noteCallToken(ctx context.Context, tok github.Token, reused bool) {
	if a := auditFrom(ctx); a != nil {
		a.tokenHash, a.reused = TokenHash(tok), reused
	}
}

// auditCalls is the receiving middleware that writes the "tool call" line,
// and retries a call once from cold when a cached entry may explain its
// failure. It sits inside callerGate, so the caller is known, and inside
// errorCodes, so it reads the error itself.
//
// The retry: when access took the repo from a lookup cache and GitHub then
// answered the tool's call with 403, 404 or 422, or the id check failed, the
// entries for that repo are dropped and the call runs again with every check
// read fresh from GitHub. A 401 on a reused token mints anew. Each tool makes
// one GitHub call after access, so a call that failed this way applied
// nothing and running it again cannot apply anything twice.
func (t *tools) auditCalls(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		start := time.Now()
		a := &callAudit{}
		res, err := next(withAudit(ctx, a), method, req)
		if retry, fresh := retryCold(a, res, err); retry {
			t.repos.forget(a.repo, a.repoID)
			rctx := ctx
			if fresh {
				rctx = withFreshTokens(ctx)
			}
			a = &callAudit{cold: true, retried: true}
			res, err = next(withAudit(rctx, a), method, req)
		}
		t.logCall(ctx, req, a, res, err, time.Since(start))
		return res, err
	}
}

// retryCold reports whether a failed attempt runs again from cold, and
// whether with fresh tokens.
func retryCold(a *callAudit, res mcp.Result, err error) (retry, fresh bool) {
	r, ok := res.(*mcp.CallToolResult)
	if err != nil || !ok || r == nil || !r.IsError {
		return false, false
	}
	cause := r.GetError()
	var ce *CodedError
	if errors.As(cause, &ce) {
		// Refusals are decided on fresh reads, except an id check after a
		// warm access, such as repo_get's.
		return a.warm && ce.Code == CodeRepoIDMismatch, false
	}
	var se *github.StatusError
	if errors.As(cause, &se) && se.StatusCode == http.StatusUnauthorized {
		return a.reusedAny, true
	}
	return a.warm && staleAnswer(cause), false
}

// staleAnswer reports a GitHub answer that a stale installation, id or name
// could explain: 404, 422, or a 403 that is not a rate limit. GitHub answers
// 422 to a mint for a repo id the installation lost, 404 to a token that no
// longer reaches the repo behind a name, and 403 to a write on a public repo
// the token has no access to.
func staleAnswer(err error) bool {
	if errors.Is(err, github.ErrNotFound) {
		return true
	}
	var se *github.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.StatusCode {
	case http.StatusUnprocessableEntity:
		return true
	case http.StatusForbidden:
		return !se.RateLimited
	}
	return false
}

// logCall writes the "tool call" line.
func (t *tools) logCall(ctx context.Context, req mcp.Request, a *callAudit, res mcp.Result, err error, took time.Duration) {
	repo := a.repo
	if repo == "" {
		repo = calledRepo(req)
	}
	attrs := []slog.Attr{
		slog.String("tool", calledToolName(req)),
		slog.Any("caller", callerOf(ctx).logValue()),
	}
	if repo != "" {
		attrs = append(attrs, slog.String("repo", repo))
	}
	if a.repoID > 0 {
		attrs = append(attrs, slog.Int64("repo_id", a.repoID), slog.String("repo_id_from", a.repoIDBy))
	}
	if a.tokenHash != "" {
		attrs = append(attrs, slog.String("token_hash", a.tokenHash))
	}
	attrs = append(attrs,
		slog.Bool("cached", a.reused),
		slog.String("outcome", callOutcome(res, err)),
		slog.Bool("retried", a.retried),
		slog.Float64("duration_ms", float64(took.Microseconds())/1000),
	)
	t.log.LogAttrs(ctx, slog.LevelInfo, "tool call", attrs...)
}

// outcomeOK is the outcome of a call that succeeded. A failed call's outcome
// is its error code, and a JSON-RPC error, such as an unknown tool, is
// outcomeProtocolError.
const (
	outcomeOK            = "ok"
	outcomeProtocolError = "protocol-error"
)

func callOutcome(res mcp.Result, err error) string {
	if err != nil {
		return outcomeProtocolError
	}
	r, ok := res.(*mcp.CallToolResult)
	if !ok || r == nil || !r.IsError {
		return outcomeOK
	}
	if p, ok := r.Meta[errorMetaKey].(errorPayload); ok {
		return p.Code
	}
	code, _ := errorCode(r.GetError())
	return code
}

// calledRepo is the repo argument of a tools/call, on one line and cut, or
// "", for the log line of a call refused before access.
func calledRepo(req mcp.Request) string {
	call, ok := req.(*mcp.CallToolRequest)
	if !ok || call.Params == nil || len(call.Params.Arguments) == 0 {
		return ""
	}
	var args struct {
		Repo string `json:"repo"`
	}
	if json.Unmarshal(call.Params.Arguments, &args) != nil {
		return ""
	}
	return truncate(plainText(args.Repo), 120)
}
