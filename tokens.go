package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// Token reuse: one installation token per caller, installation, repo set and
// permission set, reused until shortly before it expires, never shared
// between callers, and logged with its hash when it is minted.
//
// Why reuse keeps every check:
//   - The key holds everything that scopes the token: the repo ids and the
//     permissions are exactly those of the mint, so a reused token reaches
//     no more than a fresh one would.
//   - The key holds the caller, so two agents never hold the same token and
//     each event in GitHub's audit log, which records the token's hash, maps
//     back to one agent. Behind the static gateway secret ghapp cannot tell
//     agents apart, so every gateway call is the one caller "gateway".
//   - Every check that decides whether a call may use the token runs on
//     every call, before the token is taken from the cache.
const (
	// tokenRenewBefore is how long before GitHub's expiry a token stops being
	// reused. GitHub's installation tokens last an hour.
	tokenRenewBefore = 5 * time.Minute
	// maxCachedTokens bounds the token cache. A token is a few hundred bytes
	// with its key; one evicted early only costs a new mint.
	maxCachedTokens = 50_000
)

// callerLocal is the caller key without caller authentication, as over stdio
// or on a loopback bind without it.
const callerLocal = "local"

// callerOf is the caller of ctx, or the local caller when caller
// authentication is off.
func callerOf(ctx context.Context) Caller {
	if c, ok := callerFromContext(ctx); ok {
		return c
	}
	return Caller{Kind: callerLocal}
}

// key is the caller part of a token cache key: the kind and, for an agent,
// its client and subject, quoted, as callerUserID writes them, and for a
// grant its act and sub the same way. It is "gateway", "listing" or "local"
// for the other kinds.
func (c Caller) key() string {
	switch c.Kind {
	case callerAgent:
		return callerUserID(c)
	case callerGrant:
		return fmt.Sprintf("%s %q %q", c.Kind, c.Client, c.Subject)
	}
	return c.Kind
}

// logValue is the caller in a log line, without empty fields.
func (c Caller) logValue() slog.Value {
	attrs := []slog.Attr{slog.String("kind", c.Kind)}
	if c.Client != "" {
		attrs = append(attrs, slog.String("client", c.Client))
	}
	if c.Subject != "" {
		attrs = append(attrs, slog.String("subject", c.Subject))
	}
	return slog.GroupValue(attrs...)
}

// TokenHash is the form GitHub's audit log gives a token in hashed_token:
// standard base64, with padding, of the SHA-256 of the token's value. It is
// what GitHub's own command makes, echo -n TOKEN | openssl dgst -sha256
// -binary | base64. The hash names the token in logs; the value never does.
func TokenHash(tok github.Token) string {
	sum := sha256.Sum256([]byte(tok.Value))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// tokenKey is what one cached token is for.
type tokenKey struct {
	caller       string
	installation int64
	repos        string // the repo ids, sorted, comma separated; "" for the whole installation
	name         string // the owner/name a token minted by name was asked for, as the caller spelled it
	perms        string // the permissions, sorted by name, each quoted
}

func newTokenKey(caller Caller, installation int64, repoIDs []int64, perms github.Permissions) tokenKey {
	ids := make([]string, len(repoIDs))
	for i, id := range repoIDs {
		ids[i] = strconv.FormatInt(id, 10)
	}
	ps := make([]string, 0, len(perms))
	for _, name := range slices.Sorted(maps.Keys(perms)) {
		ps = append(ps, strconv.Quote(name)+":"+strconv.Quote(perms[name]))
	}
	return tokenKey{caller: caller.key(), installation: installation, repos: strings.Join(ids, ","), perms: strings.Join(ps, ",")}
}

// errNoCaller refuses a token without a caller, so a caller that forgot to
// say who it mints for never shares the local caller's tokens.
var errNoCaller = errors.New("a token needs a caller: pass the Caller the token is for")

// TokenCache mints installation tokens and reuses each one for the same
// caller, installation, repo ids and permissions until tokenRenewBefore
// before it expires. Concurrent misses for one key mint once. Every mint is
// logged as "token minted" with the token's hash. It is safe for concurrent
// use.
type TokenCache struct {
	gh      github.API
	log     *slog.Logger
	now     func() time.Time
	tokens  *lru[tokenKey, github.Token]
	flights flight[tokenKey, mintResult]
}

type mintResult struct {
	tok    github.Token
	reused bool // found in the cache by the run itself, so nothing was minted
}

func newTokenCache(gh github.API, log *slog.Logger) *TokenCache {
	return &TokenCache{gh: gh, log: log, now: time.Now, tokens: newLRU[tokenKey, github.Token](maxCachedTokens)}
}

// TokenFor returns an installation token for caller, limited to repoIDs when
// there are any, with exactly perms. It is every mint's way to GitHub: tool
// calls, name resolution, repo_list, and the git endpoint. reused reports
// that the token was not minted for this call: it came from the cache, or
// from a concurrent mint for the same key.
//
// caller must name who the token is for, as callerOf(ctx) does for a tool
// call. A git push for an agent passes that agent's Caller, so its tokens
// are never another agent's. A Caller without a kind is refused.
func (c *TokenCache) TokenFor(ctx context.Context, caller Caller, installation int64, repoIDs []int64, perms github.Permissions) (tok github.Token, reused bool, err error) {
	if caller.Kind == "" {
		return github.Token{}, false, errNoCaller
	}
	ids := slices.Clone(repoIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	key := newTokenKey(caller, installation, ids, perms)
	return c.get(ctx, key, func(ctx context.Context) (github.Token, error) {
		tok, err := c.gh.MintToken(ctx, installation, ids, maps.Clone(perms))
		if err != nil {
			return github.Token{}, err
		}
		c.logMint(ctx, caller, installation, ids, "", perms, tok)
		return tok, nil
	})
}

// TokenForRepo is TokenFor for the one repo fullName names, minted by that
// name, so resolving a name never takes a token that reaches every repo of
// the installation. GitHub matches the name without regard to case, and the
// token's Repos says which repo it reaches. A cached token keeps reaching
// the repo the name named when it was minted, so a caller that finds the
// name gone with it asks again under withFreshTokens.
func (c *TokenCache) TokenForRepo(ctx context.Context, caller Caller, installation int64, fullName string, perms github.Permissions) (tok github.Token, reused bool, err error) {
	if caller.Kind == "" {
		return github.Token{}, false, errNoCaller
	}
	key := newTokenKey(caller, installation, nil, perms)
	key.name = fullName
	return c.get(ctx, key, func(ctx context.Context) (github.Token, error) {
		tok, err := c.gh.MintTokenForRepo(ctx, installation, fullName, maps.Clone(perms))
		if err != nil {
			return github.Token{}, err
		}
		c.logMint(ctx, caller, installation, nil, fullName, perms, tok)
		return tok, nil
	})
}

// get returns the token cached under key, or mints one with mint and caches
// it. Concurrent misses for one key mint once.
func (c *TokenCache) get(ctx context.Context, key tokenKey, mintOnly func(context.Context) (github.Token, error)) (tok github.Token, reused bool, err error) {
	mint := func(ctx context.Context) (github.Token, error) {
		tok, err := mintOnly(ctx)
		if err != nil {
			return github.Token{}, err
		}
		c.tokens.put(key, tok, tok.ExpiresAt.Add(-tokenRenewBefore), c.now())
		return tok, nil
	}
	if freshTokens(ctx) {
		// Outside the flight, so it never shares a run that found the token
		// GitHub just refused. The new token replaces it in the cache.
		tok, err := mint(ctx)
		noteTokenUse(ctx, false)
		return tok, false, err
	}
	if tok, ok := c.tokens.get(key, c.now()); ok {
		noteTokenUse(ctx, true)
		return tok, true, nil
	}
	res, shared, err := c.flights.do(ctx, key, func(ctx context.Context) (mintResult, error) {
		// A run that ended just before this one began may have stored it.
		if tok, ok := c.tokens.get(key, c.now()); ok {
			return mintResult{tok: tok, reused: true}, nil
		}
		tok, err := mint(ctx)
		return mintResult{tok: tok}, err
	})
	if err != nil {
		return github.Token{}, false, err
	}
	reused = shared || res.reused
	noteTokenUse(ctx, reused)
	return res.tok, reused, nil
}

// logMint writes the "token minted" line: who the token is for, what it
// reaches, when it expires and its hash, never its value. A token minted by
// name logs the name it was asked for and the repo ids GitHub's answer
// named.
func (c *TokenCache) logMint(ctx context.Context, caller Caller, installation int64, ids []int64, name string, perms github.Permissions, tok github.Token) {
	repoIDs := slices.Clone(ids)
	if name != "" {
		for _, r := range tok.Repos {
			repoIDs = append(repoIDs, r.ID)
		}
	}
	if repoIDs == nil {
		repoIDs = []int64{}
	}
	attrs := []slog.Attr{
		slog.Any("caller", caller.logValue()),
		slog.Int64("installation", installation),
		slog.Any("repo_ids", repoIDs),
		slog.Bool("all_repos", len(ids) == 0 && name == ""),
	}
	if name != "" {
		attrs = append(attrs, slog.String("repo_name", name))
	}
	attrs = append(attrs,
		slog.Any("perms", maps.Clone(perms)),
		slog.Time("expires", tok.ExpiresAt),
		slog.String("token_hash", TokenHash(tok)),
	)
	c.log.LogAttrs(ctx, slog.LevelInfo, "token minted", attrs...)
}

type freshTokensKey struct{}

// withFreshTokens marks ctx so TokenFor mints anew, for a retry after GitHub
// refused a reused token with 401. The new token replaces the cached one.
func withFreshTokens(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshTokensKey{}, true)
}

func freshTokens(ctx context.Context) bool {
	fresh, _ := ctx.Value(freshTokensKey{}).(bool)
	return fresh
}
