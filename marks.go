package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Straza's object-scope keys: the marks in tools/list and the approved id
// in a call's _meta.
const (
	// metaObject marks a tool with the object it works on, in tools/list.
	metaObject = "ai.straza/object"
	// metaListsObjects marks the tool Straza lists the objects with.
	metaListsObjects = "ai.straza/lists-objects"
	// metaObjectID is the id Straza approved, in a call's params._meta.
	metaObjectID = "ai.straza/object-id"

	objectTypeRepo = "repo"
	// repoArgument is the one argument that names the repo on every repo
	// tool.
	repoArgument = "repo"
)

// objectMark is the value of metaObject. Exactly one of Argument and Reach is
// set.
type objectMark struct {
	Type     string `json:"type"`
	Argument string `json:"argument,omitempty"`
	Reach    string `json:"reach,omitempty"`
}

// repoObjectMark marks a tool that works on the one repo its repo argument
// names. Straza decides on that argument, so changing the mark is a release
// note.
var repoObjectMark = objectMark{Type: objectTypeRepo, Argument: repoArgument}

// listingObjectMark marks repo_list itself. It reaches every repo, so only a
// role on every repo may hold it. Straza calls it with the listing token and
// never offers it to agents.
var listingObjectMark = objectMark{Type: objectTypeRepo, Reach: "all"}

// listsObjectsMark is the value of metaListsObjects on repo_list.
type listsObjectsMark struct {
	Type       string            `json:"type"`
	Attributes []listedAttribute `json:"attributes"`
}

// listedAttribute names one attribute repo_list reports, and who can set it:
// "org" for organization owners only, "object" for repo admins too.
type listedAttribute struct {
	Name  string `json:"name"`
	SetBy string `json:"setBy"`
}

func ptr[T any](v T) *T { return &v }

// Every tool reaches GitHub, an outside service whose content other people
// write: issues, pull requests, files. So the repo tools say openWorldHint
// true. readOnlyHint and idempotentHint are false when omitted, which is what
// a write tool means.

// readHints are the annotations of a repo tool that only reads.
func readHints(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: ptr(false),
		IdempotentHint:  true,
		OpenWorldHint:   ptr(true),
	}
}

// writeHints are the annotations of a repo tool that writes. None of the
// writes is idempotent: a second call makes a second pull request, issue or
// comment, and a retried merge is the agent's decision, not the client's.
func writeHints(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: ptr(destructive),
		IdempotentHint:  false,
		OpenWorldHint:   ptr(true),
	}
}

// repoArg is the repo argument every repo tool embeds: required, top level,
// a string in owner/name form.
type repoArg struct {
	Repo string `json:"repo" jsonschema:"the repository as owner/name, exactly as GitHub spells it, for example Strazahq/pay-api"`
}

func (a repoArg) repoName() string { return a.Repo }

// repoNamer is an input that names its repo.
type repoNamer interface{ repoName() string }

// addRepoTool registers a tool that works on one repo. It sets the object
// mark, and runs the checks every repo tool shares before h: the raw
// arguments carry exactly one key "repo", and its value is a well formed
// owner/name. h then resolves the repo and mints its token through access.
func addRepoTool[In repoNamer, Out any](server *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	t.Meta = mcp.Meta{metaObject: repoObjectMark}
	mcp.AddTool(server, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		if err := checkRepoKey(req.Params.Arguments); err != nil {
			return nil, zero, err
		}
		if err := checkRepoName(in.repoName()); err != nil {
			return nil, zero, err
		}
		return h(ctx, req, in)
	})
}

// checkRepoName refuses a repo argument that is not owner/name as GitHub
// allows them: an owner of ASCII letters, digits and hyphens, and a name of
// ASCII letters, digits, '.', '_' and '-' that is not "." or "..".
func checkRepoName(s string) error {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || !validOwner(owner) || !validName(name) {
		return invalidInputf(repoArgument, "repo must be owner/name, for example Strazahq/pay-api, got %s.", quoted(truncate(plainText(s), 120)))
	}
	return nil
}

func validOwner(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isASCIIAlnum(r) && r != '-' {
			return false
		}
	}
	return true
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !isASCIIAlnum(r) && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlnum(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// checkRepoKey reads the raw arguments and refuses a second "repo" key, and
// any other top-level key that folds to "repo", such as "Repo".
//
// Straza reads the repo from the same raw arguments. A JSON reader that
// matches keys without regard to case, or that keeps the first of two
// duplicates where Go keeps the last, would see another repo than ghapp, so
// ghapp accepts only arguments every reader agrees on. The SDK's inferred
// schema forbids additional properties, which already refuses "Repo" next to
// "repo", but a duplicate "repo" passes it: the SDK validates a map, where the
// last duplicate wins.
func checkRepoKey(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	notObject := refuse(CodeInvalidInput, "", "The arguments are not a JSON object.")
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	switch d, ok := tok.(json.Delim); {
	case err != nil:
		return notObject
	case tok == nil:
		return nil // null: the schema decides
	case !ok || d != '{':
		return notObject
	}
	seen := 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return notObject
		}
		key, _ := tok.(string)
		switch {
		case key == repoArgument:
			seen++
			if seen > 1 {
				return refuse(CodeInvalidInput, repoArgument, "Send repo once. The arguments carry it more than once.")
			}
		case strings.EqualFold(key, repoArgument):
			return invalidInputf(repoArgument, "Send the argument as repo, in lower case, not %s.", quoted(truncate(plainText(key), 40)))
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return notObject
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return notObject
	}
	return nil
}
