package main

import "github.com/modelcontextprotocol/go-sdk/mcp"

// Stable error codes on tool results. Every error result carries one in
// _meta, so Straza and views tell failures apart without matching their
// text. The codes are part of ghapp's interface with Straza: renaming or
// removing one breaks callers.
const (
	CodeInvalidInput      = "invalid-input"      // arguments the tool cannot use
	CodeRepoNotCanonical  = "repo-not-canonical" // the name differs from GitHub's spelling
	CodeRepoRenamed       = "repo-renamed"       // GitHub answered with a redirect
	CodeRepoIDMismatch    = "repo-id-mismatch"   // the name is not the repository Straza approved
	CodeRepoPathOutside   = "repo-path-outside"  // a path outside the named repository
	CodeListingTokenOnly  = "listing-token-only" // the listing token on a tool other than repo_list
	CodeNotFound          = "not-found"          // GitHub's 404
	CodeNotAuthorized     = "not-authorized"     // GitHub's 401 or 403
	CodeRefused           = "refused"            // GitHub's 409 or 422
	CodeRateLimited       = "rate-limited"       // a GitHub rate limit
	CodeGitHubUnavailable = "github-unavailable" // GitHub's 5xx, or GitHub not reached
	CodeInternal          = "internal"           // anything else
)

// errorMetaKey names the error payload in an error result's _meta.
const errorMetaKey = "ghapp-mcp-server/error"

// errorPayload is the value under errorMetaKey.
type errorPayload struct {
	V     int    `json:"v"`
	Code  string `json:"code"`
	Field string `json:"field,omitempty"`
	// Reason is GitHub's own message about a refusal, when it gave one.
	// Untrusted text from GitHub.
	Reason string `json:"reason,omitempty"`
}

// refusal is a coded error result with one plain sentence, for code that
// answers before a tool handler runs.
func refusal(code, text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		Meta:    mcp.Meta{errorMetaKey: errorPayload{V: 1, Code: code}},
	}
}
