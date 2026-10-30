package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// The token permissions of the tools, as GitHub names them. The token cache
// clones them before a mint, so a map here is never changed.
var (
	permMetadataRead = github.Permissions{"metadata": "read"}
	permContentsRead = github.Permissions{"contents": "read"}
	permPullsRead    = github.Permissions{"pull_requests": "read"}
	permPullsWrite   = github.Permissions{"pull_requests": "write"}
	// permPRCreate: on a private repo GitHub refuses a pull request whose
	// token cannot read the branches, with 422 "not all refs are readable".
	permPRCreate     = github.Permissions{"pull_requests": "write", "contents": "read"}
	permMerge        = github.Permissions{"contents": "write", "pull_requests": "write"}
	permIssuesRead   = github.Permissions{"issues": "read"}
	permIssuesWrite  = github.Permissions{"issues": "write"}
	permSearchIssues = github.Permissions{"issues": "read", "pull_requests": "read"}
)

// repoAccess is a resolved repo and the one token of the call.
type repoAccess struct {
	// Repo holds the repo's ID and FullName as GitHub answered them, fresh
	// or from the lookup caches. FullName is byte for byte the caller's repo
	// argument. No other field is set.
	Repo github.Repo
	// Token reaches exactly Repo.ID with the tool's permissions. It is a
	// secret: never log it and never return it.
	Token github.Token

	reused bool // Token came from the token cache
	warm   bool // Repo came from a lookup cache, without the read that confirms it
}

// path returns the REST path of the repo followed by rest, every segment
// escaped, built from GitHub's canonical name.
func (a repoAccess) path(rest ...string) string {
	return repoPath(a.Repo.FullName, rest...)
}

// id is the repo id as a decimal string, as repo_list reports it.
func (a repoAccess) id() string { return strconv.FormatInt(a.Repo.ID, 10) }

// repoPath is "/repos/{owner}/{name}" and then rest, each segment escaped.
func repoPath(fullName string, rest ...string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	var b strings.Builder
	b.WriteString("/repos/")
	b.WriteString(url.PathEscape(owner))
	b.WriteString("/")
	b.WriteString(url.PathEscape(name))
	for _, s := range rest {
		b.WriteString("/")
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// access resolves the repo a call names and gets the call's one token, for
// that repo and perms only. Every refusal is a CodedError, and every refusal
// that does not need GitHub comes before any token is minted.
//
// With an id from Straza in params._meta, the token is for exactly that id,
// so it reaches only the repo Straza approved even when the name points
// elsewhere by now. The name is checked against it as well.
//
// Without an id, a metadata:read token for that one repo, minted by its name,
// resolves the name to its id, and the tool's own token is then for exactly
// that id. The metadata token is never used for anything else.
//
// Tokens come from the token cache, one per caller, repo and permission set.
// The installation, the id's canonical name and the name's id come from the
// lookup caches when they hold them (repo_cache.go says why that is safe).
// When GitHub answers a step that used a cached entry in a way the entry
// could explain, the entries are dropped and access runs once more from cold.
func (t *tools) access(ctx context.Context, req *mcp.CallToolRequest, name string, perms github.Permissions) (repoAccess, error) {
	id, hasID, err := approvedID(req)
	if err != nil {
		return repoAccess{}, err
	}
	a := auditFrom(ctx)
	if a != nil {
		a.repo = name
	}
	cold := a != nil && a.cold
	acc, err := t.resolve(ctx, name, id, hasID, perms, cold)
	if errors.Is(err, errStale) {
		t.repos.forget(name, id)
		acc, err = t.resolve(ctx, name, id, hasID, perms, true)
	}
	if err != nil {
		return repoAccess{}, err
	}
	if a != nil {
		a.repoID, a.warm = acc.Repo.ID, acc.warm
		a.repoIDBy = repoIDByLookup
		if hasID {
			a.repoIDBy = repoIDByStraza
		}
	}
	noteCallToken(ctx, acc.Token, acc.reused)
	return acc, nil
}

// errStale is a failure of a step that used a cached entry, which the entry
// being out of date could explain. access then runs again from cold, where
// no step uses a cached entry, so errStale never leaves access.
var errStale = errors.New("a cached repo lookup is out of date")

// resolve is one run of access. cold skips the lookup caches, and still
// fills them.
func (t *tools) resolve(ctx context.Context, name string, id int64, hasID bool, perms github.Permissions, cold bool) (repoAccess, error) {
	inst, instHit, err := t.installation(ctx, name, cold)
	switch {
	case errors.Is(err, github.ErrRedirect):
		return repoAccess{}, repoRenamed(name)
	case errors.Is(err, github.ErrNotFound):
		return repoAccess{}, repoNotFound(name)
	case err != nil:
		return repoAccess{}, fmt.Errorf("finding the App's installation on %s: %w", name, err)
	}
	if hasID {
		return t.accessByID(ctx, inst, instHit, name, id, perms, cold)
	}
	return t.accessByName(ctx, inst, instHit, name, perms, cold)
}

// installation returns the installation that holds the repo name, from the
// cache unless cold. hit reports a cached answer. A redirect or an error is
// never cached.
func (t *tools) installation(ctx context.Context, name string, cold bool) (inst int64, hit bool, err error) {
	if !cold {
		if inst, ok := t.repos.installs.get(name, t.now()); ok {
			return inst, true, nil
		}
	}
	inst, err = t.gh.InstallationForRepo(ctx, name)
	if err != nil {
		return 0, false, err
	}
	now := t.now()
	t.repos.installs.put(name, inst, now.Add(installTTL), now)
	return inst, false, nil
}

// accessByID is access with the id Straza approved. instHit reports that
// inst came from the cache.
func (t *tools) accessByID(ctx context.Context, inst int64, instHit bool, name string, id int64, perms github.Permissions, cold bool) (repoAccess, error) {
	tok, reused, err := t.tokens.TokenFor(ctx, callerOf(ctx), inst, []int64{id}, perms)
	var se *github.StatusError
	switch {
	case err != nil && instHit && staleAnswer(err):
		// The installation may have lost the repo since it was cached.
		return repoAccess{}, errStale
	case errors.Is(err, github.ErrNotFound):
		// The installation that holds the name does not hold the id.
		return repoAccess{}, repoIDMismatch(name)
	case errors.As(err, &se) && se.StatusCode == http.StatusUnprocessableEntity:
		// GitHub answers 422 both for a repository id the installation
		// cannot reach and for permissions the App was not granted. Ask
		// which, rather than read GitHub's message.
		if t.installationLacksID(ctx, inst, id) {
			return repoAccess{}, repoIDMismatch(name)
		}
		return repoAccess{}, fmt.Errorf("minting a token for %s: %w", name, err)
	case err != nil:
		return repoAccess{}, fmt.Errorf("minting a token for %s: %w", name, err)
	}
	acc := repoAccess{Repo: github.Repo{ID: id, FullName: name}, Token: tok, reused: reused}
	if !cold {
		// Only a hit that matches lets the call go ahead. Any other answer
		// is read fresh below, so the cache never refuses.
		if canonical, ok := t.repos.canonical.get(id, t.now()); ok && canonical == name {
			acc.warm = true
			return acc, nil
		}
	}
	repo, err := t.gh.RepositoryByID(ctx, tok, id)
	switch {
	case errors.Is(err, github.ErrNotFound) && instHit:
		// A token minted for a cached installation the repo has left.
		return repoAccess{}, errStale
	case errors.Is(err, github.ErrNotFound):
		return repoAccess{}, repoIDMismatch(name)
	case err != nil:
		return repoAccess{}, fmt.Errorf("reading the approved repo of %s: %w", name, err)
	}
	switch {
	case repo.ID != id:
		return repoAccess{}, repoIDMismatch(name)
	case repo.FullName == name:
		now := t.now()
		t.repos.canonical.put(id, repo.FullName, now.Add(canonicalTTL), now)
		return acc, nil
	case strings.EqualFold(repo.FullName, name):
		return repoAccess{}, repoNotCanonical(repo.FullName)
	}
	return repoAccess{}, repoIDMismatch(name)
}

// installationLacksID reports whether the installation inst cannot see the
// repository id: GitHub refuses a metadata:read token for that one id with
// 422, and every installation token has metadata:read. The mint is fresh, so
// a token cached while the id was still visible doesn't answer. Any other
// failure reports false, so the caller keeps GitHub's own answer.
func (t *tools) installationLacksID(ctx context.Context, inst, id int64) bool {
	_, _, err := t.tokens.TokenFor(withFreshTokens(ctx), callerOf(ctx), inst, []int64{id}, permMetadataRead)
	return isStatus(err, http.StatusUnprocessableEntity)
}

// isStatus reports whether err is GitHub's answer with that status.
func isStatus(err error, status int) bool {
	var se *github.StatusError
	return errors.As(err, &se) && se.StatusCode == status
}

// resolveName reads the repo name names, with a metadata:read token for
// that one repo, so resolving a name never takes a token that reaches the
// whole installation. A cached token reaches the repo the name named when it
// was minted, so when it no longer finds the name, a fresh one asks again:
// the name may now be another repo, deleted and created again.
func (t *tools) resolveName(ctx context.Context, caller Caller, inst int64, name string) (github.Repo, error) {
	meta, reused, err := t.tokens.TokenForRepo(ctx, caller, inst, name, permMetadataRead)
	if err != nil {
		return github.Repo{}, err
	}
	repo, err := t.gh.Repository(ctx, meta, name)
	if errors.Is(err, github.ErrNotFound) && reused {
		if meta, _, err = t.tokens.TokenForRepo(withFreshTokens(ctx), caller, inst, name, permMetadataRead); err != nil {
			return github.Repo{}, err
		}
		repo, err = t.gh.Repository(ctx, meta, name)
	}
	return repo, err
}

// accessByName is access without an id: a call through a role on every repo.
// instHit reports that inst came from the cache.
func (t *tools) accessByName(ctx context.Context, inst int64, instHit bool, name string, perms github.Permissions, cold bool) (repoAccess, error) {
	caller := callerOf(ctx)
	if !cold {
		if id, ok := t.repos.names.get(name, t.now()); ok {
			tok, reused, err := t.tokens.TokenFor(ctx, caller, inst, []int64{id}, perms)
			switch {
			case err != nil && staleAnswer(err):
				t.repos.canonical.remove(id)
				return repoAccess{}, errStale
			case err != nil:
				return repoAccess{}, fmt.Errorf("minting a token for %s: %w", name, err)
			}
			return repoAccess{Repo: github.Repo{ID: id, FullName: name}, Token: tok, reused: reused, warm: true}, nil
		}
	}
	repo, err := t.resolveName(ctx, caller, inst, name)
	switch {
	case errors.Is(err, github.ErrRedirect):
		return repoAccess{}, repoRenamed(name)
	case instHit && (errors.Is(err, github.ErrNotFound) || staleAnswer(err)):
		// The cached installation no longer holds the name.
		return repoAccess{}, errStale
	case errors.Is(err, github.ErrNotFound), isStatus(err, http.StatusUnprocessableEntity):
		// GitHub refuses a token for a name the installation can't see
		// with 422.
		return repoAccess{}, repoNotFound(name)
	case err != nil:
		return repoAccess{}, fmt.Errorf("resolving %s: %w", name, err)
	}
	switch {
	case repo.FullName == name:
	case strings.EqualFold(repo.FullName, name):
		return repoAccess{}, repoNotCanonical(repo.FullName)
	default:
		// GitHub answered with another repo without a redirect. Its name is
		// not the caller's to learn here.
		return repoAccess{}, refuse(CodeRepoNotCanonical, repoArgument,
			fmt.Sprintf("GitHub names the repo behind %s differently. Send the repo exactly as GitHub names it.", name))
	}
	if repo.ID <= 0 {
		return repoAccess{}, fmt.Errorf("GitHub answered %s without a repository id", name)
	}
	tok, reused, err := t.tokens.TokenFor(ctx, caller, inst, []int64{repo.ID}, perms)
	switch {
	case err != nil && instHit && staleAnswer(err):
		return repoAccess{}, errStale
	case err != nil:
		return repoAccess{}, fmt.Errorf("minting a token for %s: %w", name, err)
	}
	now := t.now()
	t.repos.names.put(name, repo.ID, now.Add(nameTTL), now)
	t.repos.canonical.put(repo.ID, name, now.Add(canonicalTTL), now)
	return repoAccess{Repo: github.Repo{ID: repo.ID, FullName: name}, Token: tok, reused: reused}, nil
}

// approvedID reads the repo id Straza approved from params._meta. It must be
// GitHub's id as a decimal string, as repo_list reports it.
func approvedID(req *mcp.CallToolRequest) (id int64, ok bool, err error) {
	if req == nil || req.Params == nil {
		return 0, false, nil
	}
	v, present := req.Params.Meta[metaObjectID]
	if !present {
		return 0, false, nil
	}
	s, isString := v.(string)
	if isString {
		id, err = strconv.ParseInt(s, 10, 64)
	}
	// FormatInt round trip: digits only, no sign, no leading zero, no space.
	if !isString || err != nil || id <= 0 || strconv.FormatInt(id, 10) != s {
		return 0, false, refuse(CodeInvalidInput, metaObjectID,
			"The "+metaObjectID+" in _meta must be a repository id as a decimal string, such as \"812345678\".")
	}
	return id, true, nil
}

// The refusals of a repo that is not what the call named, each one plain
// sentence.

func repoNotCanonical(canonical string) error {
	return refuse(CodeRepoNotCanonical, repoArgument,
		fmt.Sprintf("Send the repo as %s, exactly as GitHub names it.", canonical))
}

func repoRenamed(name string) error {
	return refuse(CodeRepoRenamed, repoArgument,
		fmt.Sprintf("%s was renamed on GitHub. Call it by its new name.", name))
}

func repoIDMismatch(name string) error {
	return refuse(CodeRepoIDMismatch, repoArgument,
		fmt.Sprintf("%s is not the repo Straza approved: it was probably deleted and created again. A person has to add the new repo to the role.", name))
}

func repoNotFound(name string) error {
	return refuse(CodeNotFound, repoArgument,
		fmt.Sprintf("%s was not found on GitHub, or the App is not installed on it.", name))
}
