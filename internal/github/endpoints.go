package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// perPage is the page size of every listing, GitHub's largest.
const perPage = 100

// maxPage bounds a page number, so page*perPage cannot overflow.
const maxPage = 1 << 20

// maxInstallationPages stops Installations from paging forever on a broken
// answer: 100,000 installations.
const maxInstallationPages = 1000

// installationsPath lists the App's installations.
const installationsPath = "/app/installations"

// repoJSON is a repository as GitHub's REST API answers it.
type repoJSON struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
}

// repo maps GitHub's answer into Repo.
func (r repoJSON) repo() Repo {
	return Repo{
		ID:            r.ID,
		FullName:      r.FullName,
		Name:          r.Name,
		Owner:         r.Owner.Login,
		OwnerType:     r.Owner.Type,
		Private:       r.Private,
		Archived:      r.Archived,
		DefaultBranch: r.DefaultBranch,
		HTMLURL:       r.HTMLURL,
	}
}

// installationJSON is an installation as GitHub's REST API answers it.
type installationJSON struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Slug  string `json:"slug"` // an enterprise account has a slug and no login
		Type  string `json:"type"`
	} `json:"account"`
	TargetType string `json:"target_type"`
}

// installation maps GitHub's answer into Installation. An enterprise
// installation has no login and no account type, so its slug and target
// type stand in.
func (in installationJSON) installation() Installation {
	out := Installation{ID: in.ID, Account: in.Account.Login, AccountType: in.Account.Type}
	if out.Account == "" {
		out.Account = in.Account.Slug
	}
	if out.AccountType == "" {
		out.AccountType = in.TargetType
	}
	return out
}

// tokenRequest is the body of POST /app/installations/{id}/access_tokens.
// With no repository ids the field is left out, and the token reaches every
// repository of the installation.
type tokenRequest struct {
	RepositoryIDs []int64     `json:"repository_ids,omitempty"`
	Repositories  []string    `json:"repositories,omitempty"`
	Permissions   Permissions `json:"permissions"`
}

// tokenAnswer is the part of GitHub's token answer the client keeps.
type tokenAnswer struct {
	Token        string     `json:"token"`
	ExpiresAt    time.Time  `json:"expires_at"`
	Repositories []repoJSON `json:"repositories"`
}

// propertyValuesJSON is one repository in GET /orgs/{org}/properties/values.
type propertyValuesJSON struct {
	RepoID     int64  `json:"repository_id"`
	FullName   string `json:"repository_full_name"`
	Properties []struct {
		Name  string          `json:"property_name"`
		Value json.RawMessage `json:"value"`
	} `json:"properties"`
}

// InstallationForRepo returns the installation that holds the repository
// fullName, with the App JWT.
func (a *App) InstallationForRepo(ctx context.Context, fullName string) (int64, error) {
	path, err := RepoPath(fullName, "installation")
	if err != nil {
		return 0, err
	}
	var in installationJSON
	if _, err := a.callAsApp(ctx, http.MethodGet, path, nil, nil, &in); err != nil {
		return 0, err
	}
	if in.ID <= 0 {
		return 0, fmt.Errorf("GET %s: GitHub's answer has no installation id", path)
	}
	return in.ID, nil
}

// Installations lists every installation of the App, page by page, with the
// App JWT.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	out := []Installation{}
	for page := 1; ; page++ {
		if page > maxInstallationPages {
			return nil, fmt.Errorf("GET %s: still more after %d pages of %d. Stopped, so a broken answer cannot page forever", installationsPath, maxInstallationPages, perPage)
		}
		var batch []installationJSON
		h, err := a.callAsApp(ctx, http.MethodGet, installationsPath, pageQuery(page), nil, &batch)
		if err != nil {
			return nil, err
		}
		for _, in := range batch {
			out = append(out, in.installation())
		}
		if len(batch) == 0 || !hasNext(h) {
			return out, nil
		}
	}
}

// MintToken mints an installation token with exactly perms, limited to
// repoIDs when there are any, with the App JWT. It refuses an installation
// id, a repository id or a permission GitHub cannot know, and empty perms,
// before any call: GitHub would answer empty permissions with every
// permission of the installation.
func (a *App) MintToken(ctx context.Context, installationID int64, repoIDs []int64, perms Permissions) (Token, error) {
	if err := checkMint(installationID, perms); err != nil {
		return Token{}, err
	}
	for _, id := range repoIDs {
		if id <= 0 {
			return Token{}, invalidf("the repository id %d is not a GitHub repository id, which is a positive number", id)
		}
	}
	return a.mint(ctx, installationID, tokenRequest{RepositoryIDs: repoIDs, Permissions: perms})
}

// MintTokenForRepo mints an installation token with exactly perms for the
// one repository fullName names, by its name, with the App JWT. The
// installation is the one that holds the repository, so GitHub takes the
// name without its owner.
func (a *App) MintTokenForRepo(ctx context.Context, installationID int64, fullName string, perms Permissions) (Token, error) {
	_, name, err := splitFullName(fullName)
	if err != nil {
		return Token{}, err
	}
	if err := checkMint(installationID, perms); err != nil {
		return Token{}, err
	}
	return a.mint(ctx, installationID, tokenRequest{Repositories: []string{name}, Permissions: perms})
}

// checkMint refuses an installation id or a permission GitHub cannot know,
// and empty perms.
func checkMint(installationID int64, perms Permissions) error {
	if installationID <= 0 {
		return invalidf("the installation id %d is not a GitHub installation id, which is a positive number", installationID)
	}
	if len(perms) == 0 {
		return invalidf("no permissions to mint a token with. Name what the call needs, such as contents: read; GitHub would answer an empty set with every permission of the installation")
	}
	for name, level := range perms {
		if !validPermission(name) || (level != "read" && level != "write" && level != "admin") {
			return invalidf("the permission %s: %s is not one GitHub knows. Name it as GitHub does, such as pull_requests, with read, write or admin", quote(name), quote(level))
		}
	}
	return nil
}

// mint posts req and reads the token from GitHub's answer.
func (a *App) mint(ctx context.Context, installationID int64, req tokenRequest) (Token, error) {
	path := installationsPath + "/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	var raw json.RawMessage
	if _, err := a.callAsApp(ctx, http.MethodPost, path, nil, req, &raw); err != nil {
		return Token{}, err
	}
	// The answer holds the token, so a decode error is not passed on: its text
	// could quote part of the answer.
	var ans tokenAnswer
	if err := json.Unmarshal(raw, &ans); err != nil || ans.Token == "" || ans.ExpiresAt.IsZero() {
		return Token{}, fmt.Errorf("POST %s: GitHub's answer is not a token with token and expires_at", path)
	}
	tok := Token{Value: ans.Token, ExpiresAt: ans.ExpiresAt, InstallationID: installationID}
	for _, r := range ans.Repositories {
		tok.Repos = append(tok.Repos, r.repo())
	}
	return tok, nil
}

// RepositoryByID reads the repository with this id, with tok.
func (a *App) RepositoryByID(ctx context.Context, tok Token, id int64) (Repo, error) {
	if err := checkToken(tok); err != nil {
		return Repo{}, err
	}
	if id <= 0 {
		return Repo{}, invalidf("the repository id %d is not a GitHub repository id, which is a positive number", id)
	}
	return a.repository(ctx, tok, "/repositories/"+strconv.FormatInt(id, 10))
}

// Repository reads the repository fullName, with tok. A redirect is
// ErrRedirect and is not followed.
func (a *App) Repository(ctx context.Context, tok Token, fullName string) (Repo, error) {
	if err := checkToken(tok); err != nil {
		return Repo{}, err
	}
	path, err := RepoPath(fullName)
	if err != nil {
		return Repo{}, err
	}
	return a.repository(ctx, tok, path)
}

// repository reads one repository at path and checks GitHub named it.
func (a *App) repository(ctx context.Context, tok Token, path string) (Repo, error) {
	var r repoJSON
	if _, err := a.call(ctx, tok.Value, http.MethodGet, path, nil, nil, &r); err != nil {
		return Repo{}, err
	}
	if r.ID <= 0 || r.FullName == "" {
		return Repo{}, fmt.Errorf("GET %s: GitHub's answer has no repository id or full_name", path)
	}
	return r.repo(), nil
}

// InstallationRepos lists one page of the repositories tok reaches. more is
// page*100 < total_count, and false on an empty page.
func (a *App) InstallationRepos(ctx context.Context, tok Token, page int) ([]Repo, bool, error) {
	if err := checkToken(tok); err != nil {
		return nil, false, err
	}
	if err := checkPage(page); err != nil {
		return nil, false, err
	}
	var ans struct {
		TotalCount   int        `json:"total_count"`
		Repositories []repoJSON `json:"repositories"`
	}
	if _, err := a.call(ctx, tok.Value, http.MethodGet, "/installation/repositories", pageQuery(page), nil, &ans); err != nil {
		return nil, false, err
	}
	repos := make([]Repo, 0, len(ans.Repositories))
	for _, r := range ans.Repositories {
		repos = append(repos, r.repo())
	}
	return repos, len(repos) > 0 && page*perPage < ans.TotalCount, nil
}

// PropertySchema reads the custom property schema of the organization org,
// with tok.
func (a *App) PropertySchema(ctx context.Context, tok Token, org string) ([]PropertyDef, error) {
	if err := checkToken(tok); err != nil {
		return nil, err
	}
	if err := checkOwner("organization", org); err != nil {
		return nil, err
	}
	path := "/orgs/" + url.PathEscape(org) + "/properties/schema"
	defs := []PropertyDef{}
	if _, err := a.call(ctx, tok.Value, http.MethodGet, path, nil, nil, &defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// PropertyValues lists one page of custom property values for the
// repositories of org, with tok. A string value gives one value, a list its
// strings, and null none, as an empty list. more comes from the Link header,
// and is false on an empty page.
func (a *App) PropertyValues(ctx context.Context, tok Token, org string, page int) ([]RepoProperties, bool, error) {
	if err := checkToken(tok); err != nil {
		return nil, false, err
	}
	if err := checkOwner("organization", org); err != nil {
		return nil, false, err
	}
	if err := checkPage(page); err != nil {
		return nil, false, err
	}
	path := "/orgs/" + url.PathEscape(org) + "/properties/values"
	var batch []propertyValuesJSON
	h, err := a.call(ctx, tok.Value, http.MethodGet, path, pageQuery(page), nil, &batch)
	if err != nil {
		return nil, false, err
	}
	out := make([]RepoProperties, 0, len(batch))
	for _, rp := range batch {
		values := make(map[string][]string, len(rp.Properties))
		for _, p := range rp.Properties {
			v, ok := propertyValue(p.Value)
			if !ok {
				return nil, false, fmt.Errorf("GET %s: GitHub's value for the property %s of repository %d is not a string, a list of strings or null", path, quote(p.Name), rp.RepoID)
			}
			values[p.Name] = v
		}
		out = append(out, RepoProperties{RepoID: rp.RepoID, FullName: rp.FullName, Values: values})
	}
	return out, len(out) > 0 && hasNext(h), nil
}

// Do makes one REST call with tok. It refuses an unsafe path or a method
// other than GET, POST, PUT, PATCH or DELETE before any call.
func (a *App) Do(ctx context.Context, tok Token, method, path string, query url.Values, body, out any) error {
	if err := checkToken(tok); err != nil {
		return err
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return invalidf("the method %s is not one the client sends. Use GET, POST, PUT, PATCH or DELETE", quote(method))
	}
	_, err := a.call(ctx, tok.Value, method, path, query, body, out)
	return err
}

// propertyValue normalises a custom property value: a string gives one
// value, a list of strings its strings, and null, or no value, none. Any
// other value is not ok.
func propertyValue(raw json.RawMessage) ([]string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return []string{}, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, true
	}
	var list []*string // a pointer, so a null inside the list is seen
	if json.Unmarshal(raw, &list) != nil {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v == nil {
			return nil, false
		}
		out = append(out, *v)
	}
	return out, true
}

// checkToken refuses an installation token that is empty or could not go in
// a header. Its errors never quote the token.
func checkToken(tok Token) error {
	if tok.Value == "" {
		return invalidf("the installation token is empty. Mint one with MintToken first")
	}
	for i := 0; i < len(tok.Value); i++ {
		if c := tok.Value[i]; c <= ' ' || c >= 0x7f {
			return invalidf("the installation token holds a character a token may not. Mint a new one with MintToken")
		}
	}
	return nil
}

// checkPage refuses a page number below 1 or too large to count.
func checkPage(page int) error {
	if page < 1 || page > maxPage {
		return invalidf("the page %d is not a page number. Pages count from 1", page)
	}
	return nil
}

// validPermission reports whether name could be a GitHub App permission:
// 1 to 64 lower case letters and '_'.
func validPermission(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

// pageQuery asks for one page of perPage.
func pageQuery(page int) url.Values {
	return url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
}
