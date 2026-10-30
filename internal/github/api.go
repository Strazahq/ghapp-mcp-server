// Package github is the GitHub App's REST client. It signs in as the App,
// mints installation tokens for exact repositories and permissions, and makes
// REST calls with those tokens. It knows nothing about MCP or Straza: the
// tools decide which repository and which permissions a call gets.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Permissions are an installation token's permissions, as GitHub names them:
// {"pull_requests": "write", "contents": "read"}.
type Permissions map[string]string

// Repo is a repository as GitHub answers it.
type Repo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"` // canonical "owner/name", exactly as GitHub spells it
	Name          string `json:"name"`
	Owner         string `json:"owner"`      // the owner's login
	OwnerType     string `json:"owner_type"` // "Organization" or "User"
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
}

// Installation is one installation of the App on an account.
type Installation struct {
	ID          int64  `json:"id"`
	Account     string `json:"account"`      // the account's login
	AccountType string `json:"account_type"` // "Organization" or "User"
}

// Token is an installation access token. Its value is a secret: never log it
// and never return it to a caller.
type Token struct {
	Value     string
	ExpiresAt time.Time
	// InstallationID is the installation the token was minted for.
	InstallationID int64
	// Repos are the repositories GitHub named in its answer to the mint, as
	// it spells them, with their ids. Empty for a token on every repository
	// of the installation.
	Repos []Repo
}

// PropertyDef is one custom property in an organization's schema.
type PropertyDef struct {
	Name string `json:"property_name"`
	// ValuesEditableBy is GitHub's "org_actors" or "org_and_repo_actors".
	// Empty means GitHub's default, "org_actors".
	ValuesEditableBy string `json:"values_editable_by"`
}

// RepoProperties are the custom property values of one repository.
type RepoProperties struct {
	RepoID   int64  `json:"repository_id"`
	FullName string `json:"repository_full_name"`
	// Values maps a property name to its values. A single-value property has
	// one value, a multi-select property several, an unset property none.
	Values map[string][]string `json:"values"`
}

// API is what the tools need from GitHub. *App implements it.
type API interface {
	// InstallationForRepo returns the installation that holds the repository
	// fullName ("owner/name"). An unknown repository, or one the App is not
	// installed on, is ErrNotFound. A rename redirect is ErrRedirect.
	InstallationForRepo(ctx context.Context, fullName string) (int64, error)

	// Installations lists every installation of the App.
	Installations(ctx context.Context) ([]Installation, error)

	// MintToken mints an installation token with exactly perms. repoIDs
	// limits it to those repositories. With no repoIDs the token reaches
	// every repository of the installation. perms must not be empty.
	MintToken(ctx context.Context, installationID int64, repoIDs []int64, perms Permissions) (Token, error)

	// MintTokenForRepo mints an installation token with exactly perms for
	// the one repository fullName names, by its name. GitHub matches the
	// name without regard to case, so the token's Repos says which
	// repository it reaches, as GitHub spells it, with its id. A name the
	// installation cannot see is a StatusError with 422.
	MintTokenForRepo(ctx context.Context, installationID int64, fullName string, perms Permissions) (Token, error)

	// RepositoryByID reads the repository with this id, with tok.
	RepositoryByID(ctx context.Context, tok Token, id int64) (Repo, error)

	// Repository reads the repository fullName, with tok. A rename redirect
	// is ErrRedirect, never followed.
	Repository(ctx context.Context, tok Token, fullName string) (Repo, error)

	// InstallationRepos lists one page of the repositories tok reaches,
	// 100 per page, pages counted from 1. more reports a next page.
	InstallationRepos(ctx context.Context, tok Token, page int) (repos []Repo, more bool, err error)

	// PropertySchema reads the custom property schema of the organization
	// org, with tok.
	PropertySchema(ctx context.Context, tok Token, org string) ([]PropertyDef, error)

	// PropertyValues lists one page of custom property values for the
	// repositories of org, 100 per page, pages counted from 1.
	PropertyValues(ctx context.Context, tok Token, org string, page int) (values []RepoProperties, more bool, err error)

	// Do makes one REST call with tok. path starts with "/" and is relative
	// to the API base URL. query may be nil. body, when not nil, is sent as
	// JSON. out, when not nil, receives the JSON answer. A non-2xx answer is a
	// *StatusError, and a redirect is ErrRedirect, never followed.
	Do(ctx context.Context, tok Token, method, path string, query url.Values, body, out any) error
}

// ErrNotFound is GitHub's 404: the object does not exist, or the token or
// the App cannot see it. GitHub does not say which.
var ErrNotFound = errors.New("not found on GitHub")

// ErrRedirect is a redirect from GitHub, most often for a renamed or
// transferred repository. Redirects are never followed, so a call never
// lands on a repository it did not name.
var ErrRedirect = errors.New("GitHub answered with a redirect")

// StatusError is a non-2xx answer from GitHub other than 404. Its text names
// the method, the path and the status, never a token or the base URL.
type StatusError struct {
	Method     string
	Path       string
	StatusCode int
	Status     string // the status line, e.g. "422 Unprocessable Entity"
	// Message is GitHub's "message" from the error body, or "". Untrusted.
	Message string
	// RateLimited reports a primary or secondary rate limit: a 429, or a 403
	// with x-ratelimit-remaining 0 or a retry-after header.
	RateLimited bool
}

// Error names the request and the status GitHub answered.
func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub %s %s: unexpected status %s", e.Method, e.Path, e.Status)
}

// Config is what the App needs to sign in.
type Config struct {
	// AppID is the GitHub App's id.
	AppID string
	// PrivateKeyPEM is the App's private key, PKCS#1 or PKCS#8 PEM.
	PrivateKeyPEM []byte
	// BaseURL is the REST API base, "https://api.github.com" by default.
	BaseURL string
	// HTTPClient is used for every call. nil means a client with a 30 s
	// timeout. Its CheckRedirect is replaced so redirects are never followed.
	HTTPClient *http.Client
}
