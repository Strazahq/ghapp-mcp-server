package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// Search within one repo. The tools add repo:<canonical> to the query
// themselves and refuse a query with a qualifier that names where to search.
// GitHub joins such qualifiers with OR, so "repo:a/other" next to the added
// one would search both repos, and search reaches every public repo whatever
// the token. The token is minted for the one repo as well, and a result from
// any other repo is dropped, so each lock holds on its own.

// scopeQualifiers are the qualifiers that choose where GitHub searches.
var scopeQualifiers = []string{"repo:", "org:", "user:", "owner:"}

// maxQueryBytes bounds a search query. GitHub allows 256 characters without
// qualifiers.
const maxQueryBytes = 512

func (t *tools) registerSearchTools(server *mcp.Server) {
	t.registerSearchCode(server)
	t.registerSearchIssues(server)
}

// searchQuery checks the caller's query. A false positive, such as a code
// search for the text "user:", is refused on purpose: the check reads the
// query as GitHub would, without guessing what the caller meant.
func searchQuery(tool, repo, query string, required bool) error {
	if required && strings.TrimSpace(query) == "" {
		return invalidInputf("query", "query must not be empty.")
	}
	if len(query) > maxQueryBytes {
		return invalidInputf("query", "query must be at most %d bytes, got %d.", maxQueryBytes, len(query))
	}
	folded := foldString(query)
	for _, q := range scopeQualifiers {
		if strings.Contains(folded, foldString(q)) {
			return invalidInputf("query", "Leave repo:, org:, user: and owner: out of query. %s adds repo:%s itself and searches only that repo.", tool, repo)
		}
	}
	return nil
}

// withRepo is the query GitHub gets: the caller's, then repo:<canonical>.
func withRepo(query, canonical string) string {
	return strings.TrimSpace(strings.TrimSpace(query) + " repo:" + canonical)
}

// foldString maps every character to one representative of its Unicode case
// fold orbit, so "REPO:" and "repo:" match, and so do letters such as the
// long s that fold to an ASCII one.
func foldString(s string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			lowest = min(lowest, f)
		}
		return lowest
	}, s)
}

type searchInput struct {
	repoArg
	Query string `json:"query" jsonschema:"what to search for, in GitHub's search syntax, without repo:, org:, user: or owner:. The tool searches only the named repository"`
	pageArgs
}

// --- search_code ---

type codeHit struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	SHA     string `json:"sha"`
	HTMLURL string `json:"htmlUrl"`
}

type searchCodeOutput struct {
	Repo       string    `json:"repo"`
	Query      string    `json:"query" jsonschema:"the query as sent to GitHub, repo qualifier included"`
	TotalCount int       `json:"totalCount"`
	Incomplete bool      `json:"incomplete" jsonschema:"GitHub stopped the search early, so hits may be missing"`
	Items      []codeHit `json:"items"`
	Page       int       `json:"page"`
	NextPage   int       `json:"nextPage,omitempty" jsonschema:"set when another page may follow"`
}

func (t *tools) registerSearchCode(server *mcp.Server) {
	addRepoTool(server, &mcp.Tool{
		Name:  "search_code",
		Title: "Search code",
		Description: "Search the code of one repository, on its default branch. The tool adds repo:<repo> to the query itself, " +
			"so leave repo:, org:, user: and owner: out: a query with them is refused." + untrustedNote,
		Annotations: readHints("Search code"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchCodeOutput, error) {
		if err := searchQuery("search_code", in.Repo, in.Query, true); err != nil {
			return nil, searchCodeOutput{}, err
		}
		q, perPage, page, err := in.query()
		if err != nil {
			return nil, searchCodeOutput{}, err
		}
		acc, err := t.access(ctx, req, in.Repo, permContentsRead)
		if err != nil {
			return nil, searchCodeOutput{}, err
		}
		q.Set("q", withRepo(in.Query, acc.Repo.FullName))
		var ans struct {
			TotalCount int  `json:"total_count"`
			Incomplete bool `json:"incomplete_results"`
			Items      []struct {
				Name       string `json:"name"`
				Path       string `json:"path"`
				SHA        string `json:"sha"`
				HTMLURL    string `json:"html_url"`
				Repository struct {
					ID int64 `json:"id"`
				} `json:"repository"`
			} `json:"items"`
		}
		if err := t.call(ctx, acc, http.MethodGet, "code search", "/search/code", q, nil, &ans); err != nil {
			return nil, searchCodeOutput{}, err
		}
		out := searchCodeOutput{Repo: acc.Repo.FullName, Query: q.Get("q"), TotalCount: ans.TotalCount, Incomplete: ans.Incomplete,
			Items: []codeHit{}, Page: page, NextPage: nextPage(len(ans.Items), perPage, page)}
		var items []string
		for _, h := range ans.Items {
			if h.Repository.ID != acc.Repo.ID {
				continue
			}
			out.Items = append(out.Items, codeHit{Path: h.Path, Name: h.Name, SHA: h.SHA, HTMLURL: h.HTMLURL})
			items = append(items, "- "+quotedTitle(h.Path))
		}
		return textResult(lines(fmt.Sprintf("%s: %d of %d code hit(s) on page %d.", out.Repo, len(out.Items), out.TotalCount, page), items)), out, nil
	})
}

// --- search_issues ---

type issueHit struct {
	Number        int    `json:"number"`
	Title         string `json:"title"`
	State         string `json:"state"`
	IsPullRequest bool   `json:"isPullRequest"`
	Author        string `json:"author"`
	HTMLURL       string `json:"htmlUrl"`
}

type searchIssuesOutput struct {
	Repo       string     `json:"repo"`
	Query      string     `json:"query" jsonschema:"the query as sent to GitHub, repo qualifier included"`
	TotalCount int        `json:"totalCount"`
	Incomplete bool       `json:"incomplete" jsonschema:"GitHub stopped the search early, so hits may be missing"`
	Items      []issueHit `json:"items"`
	Page       int        `json:"page"`
	NextPage   int        `json:"nextPage,omitempty" jsonschema:"set when another page may follow"`
}

func (t *tools) registerSearchIssues(server *mcp.Server) {
	addRepoTool(server, &mcp.Tool{
		Name:  "search_issues",
		Title: "Search issues and pull requests",
		Description: "Search the issues and pull requests of one repository. Add is:issue or is:pr to the query to choose one kind. " +
			"The tool adds repo:<repo> to the query itself, so leave repo:, org:, user: and owner: out: a query with them is refused." + untrustedNote,
		Annotations: readHints("Search issues and pull requests"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchIssuesOutput, error) {
		if err := searchQuery("search_issues", in.Repo, in.Query, false); err != nil {
			return nil, searchIssuesOutput{}, err
		}
		q, perPage, page, err := in.query()
		if err != nil {
			return nil, searchIssuesOutput{}, err
		}
		acc, err := t.access(ctx, req, in.Repo, permSearchIssues)
		if err != nil {
			return nil, searchIssuesOutput{}, err
		}
		q.Set("q", withRepo(in.Query, acc.Repo.FullName))
		var ans struct {
			TotalCount int           `json:"total_count"`
			Incomplete bool          `json:"incomplete_results"`
			Items      []issueSearch `json:"items"`
		}
		if err := t.call(ctx, acc, http.MethodGet, "issue search", "/search/issues", q, nil, &ans); err != nil {
			return nil, searchIssuesOutput{}, err
		}
		out := searchIssuesOutput{Repo: acc.Repo.FullName, Query: q.Get("q"), TotalCount: ans.TotalCount, Incomplete: ans.Incomplete,
			Items: []issueHit{}, Page: page, NextPage: nextPage(len(ans.Items), perPage, page)}
		var items []string
		for _, h := range ans.Items {
			if !inRepo(h.RepositoryURL, acc.Repo) {
				continue
			}
			hit := issueHit{Number: h.Number, Title: h.Title, State: h.State, IsPullRequest: h.isPull(), Author: h.User.Login, HTMLURL: h.HTMLURL}
			out.Items = append(out.Items, hit)
			kind := "issue"
			if hit.IsPullRequest {
				kind = "pull request"
			}
			items = append(items, fmt.Sprintf("- #%d %s %s (%s)", hit.Number, kind, quotedTitle(hit.Title), hit.State))
		}
		return textResult(lines(fmt.Sprintf("%s: %d of %d hit(s) on page %d.", out.Repo, len(out.Items), out.TotalCount, page), items)), out, nil
	})
}

// issueSearch is one hit of GitHub's issue search.
type issueSearch struct {
	issueAnswer
	RepositoryURL string `json:"repository_url"`
}

// inRepo reports whether an API URL such as
// https://api.github.com/repos/Strazahq/pay-api is the repo's.
func inRepo(apiURL string, repo github.Repo) bool {
	return strings.HasSuffix(apiURL, "/repos/"+repo.FullName)
}
