package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// sdkValidationPrefix starts the text of the error the SDK returns, before any
// handler runs, for arguments that fail the tool's input schema.
const sdkValidationPrefix = `validating "arguments"`

// gitHubSaidLimit is how much of GitHub's own message a refusal carries, in
// characters.
const gitHubSaidLimit = 600

// CodedError gives an error a stable code from errmeta.go. Its text is the
// wrapped error's, unchanged: the one plain sentence the caller reads.
type CodedError struct {
	Code  string
	Field string // the argument at fault, mostly for invalid-input
	Err   error
}

// Error returns the refusal's sentence.
func (e *CodedError) Error() string { return e.Err.Error() }

// Unwrap returns the error the refusal wraps.
func (e *CodedError) Unwrap() error { return e.Err }

// refuse returns a coded error whose text is sentence.
func refuse(code, field, sentence string) error {
	return &CodedError{Code: code, Field: field, Err: errors.New(sentence)}
}

// invalidInputf returns an invalid-input error about field.
func invalidInputf(field, format string, args ...any) error {
	return &CodedError{Code: CodeInvalidInput, Field: field, Err: fmt.Errorf(format, args...)}
}

// errorCodes is the receiving middleware that codes every tool error result.
//
// Tool handlers return typed errors (CodedError, github.StatusError and the
// github sentinels) and the SDK turns a returned error into the result, so the
// text is exactly the error's. Building the result in the handler instead
// would not keep the shape: with a typed output the SDK adds the zero output
// as structuredContent to any result a handler returns without an error. The
// SDK keeps the error on the result (GetError), and the code is read from it
// here. An error the SDK raised itself is invalid-input when the arguments
// failed validation, and any other error without a code is internal. JSON-RPC
// errors, such as an unknown tool, are not tool results and pass unchanged.
func errorCodes(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		r, ok := res.(*mcp.CallToolResult)
		if err != nil || !ok || r == nil || !r.IsError {
			return res, err
		}
		if _, coded := r.Meta[errorMetaKey]; coded {
			return res, nil
		}
		out := *r
		out.Meta = maps.Clone(r.Meta)
		if out.Meta == nil {
			out.Meta = mcp.Meta{}
		}
		out.Meta[errorMetaKey] = classifyError(r.GetError())
		out.Content = withGitHubSaid(r.Content, r.GetError())
		return &out, nil
	}
}

// classifyError returns the error payload for a tool error.
func classifyError(err error) errorPayload {
	code, field := errorCode(err)
	return errorPayload{V: 1, Code: code, Field: field, Reason: gitHubSaid(err)}
}

// errorCode returns the code and field of err. The outermost CodedError wins,
// so a tool that wraps a GitHub answer with a code of its own overrides the
// code of the cause.
func errorCode(err error) (code, field string) {
	if err == nil {
		return CodeInternal, ""
	}
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code, ce.Field
	}
	switch {
	case errors.Is(err, github.ErrRedirect):
		return CodeRepoRenamed, ""
	case errors.Is(err, github.ErrNotFound):
		return CodeNotFound, ""
	case errors.Is(err, github.ErrInvalidInput):
		return CodeInvalidInput, ""
	}
	var se *github.StatusError
	if errors.As(err, &se) {
		switch {
		case se.RateLimited:
			return CodeRateLimited, ""
		case se.StatusCode == http.StatusUnauthorized, se.StatusCode == http.StatusForbidden:
			return CodeNotAuthorized, ""
		case se.StatusCode == http.StatusConflict, se.StatusCode == http.StatusUnprocessableEntity:
			return CodeRefused, ""
		case se.StatusCode >= 500:
			return CodeGitHubUnavailable, ""
		}
		return CodeInternal, ""
	}
	// GitHub not reached: a dial, TLS or timeout failure of the HTTP client,
	// which errmeta.go counts as github-unavailable.
	var ne net.Error
	if errors.As(err, &ne) {
		return CodeGitHubUnavailable, ""
	}
	if strings.HasPrefix(err.Error(), sdkValidationPrefix) {
		return CodeInvalidInput, ""
	}
	return CodeInternal, ""
}

// gitHubSaid returns GitHub's own message about the first status error in
// err's chain, on one line and cut to gitHubSaidLimit, or "".
func gitHubSaid(err error) string {
	var se *github.StatusError
	if !errors.As(err, &se) {
		return ""
	}
	return truncate(strings.TrimSpace(plainText(se.Message)), gitHubSaidLimit)
}

// withGitHubSaid adds GitHub's own message under the error's text, as one line
// marked as GitHub's words and untrusted. The content is copied, not changed
// in place.
func withGitHubSaid(content []mcp.Content, err error) []mcp.Content {
	said := gitHubSaid(err)
	if said == "" || len(content) == 0 {
		return content
	}
	tc, ok := content[0].(*mcp.TextContent)
	if !ok {
		return content
	}
	out := slices.Clone(content)
	text := *tc
	text.Text = tc.Text + "\n" + untrustedLine("message in GitHub's own words", said)
	out[0] = &text
	return out
}
