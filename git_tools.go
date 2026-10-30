package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// permContentsWrite is what a push needs. A fetch needs permContentsRead.
var permContentsWrite = github.Permissions{"contents": "write"}

// gitOpPerms are the token permissions of each git operation, at the git
// tools and at the git endpoint alike.
func gitOpPerms(op string) github.Permissions {
	if op == gitOpPush {
		return permContentsWrite
	}
	return permContentsRead
}

// branchArgument is git_push's argument besides repo.
const branchArgument = "branch"

// branchPlaceholder stands for the branch in a push command when the agent
// named none. A shell refuses to run it as it is.
const branchPlaceholder = "<branch>"

// gitHowInput is git_fetch's input.
type gitHowInput struct {
	repoArg
}

// gitPushHowInput is git_push's input.
type gitPushHowInput struct {
	repoArg
	Branch string `json:"branch,omitempty" jsonschema:"the branch the returned command pushes HEAD to, such as main or feature/login. Without it the command holds <branch> for you to replace"`
}

// gitHowOutput is what git_push and git_fetch answer: the checked repo and
// the plain git command for it. It holds no secret.
type gitHowOutput struct {
	Op           string `json:"op" jsonschema:"push or fetch"`
	Repo         string `json:"repo" jsonschema:"the repository, owner/name as GitHub spells it"`
	RepoID       string `json:"repoId" jsonschema:"GitHub's repository id"`
	Remote       string `json:"remote" jsonschema:"the URL git uses for this repo, GitHub's own. Straza's holder on this machine takes git's requests to it"`
	Command      string `json:"command" jsonschema:"the plain git command: a push of HEAD to the branch, or a clone"`
	FetchCommand string `json:"fetchCommand,omitempty" jsonschema:"for fetch, the git command that fetches every branch into an existing clone instead"`
}

// registerGitTools registers git_fetch and git_push when git transport is on.
// Two tools, so Straza decides per tool whether an agent may push, and a
// Straza role binds them for git through Straza's holder as well. They check
// the repo and say how to use plain git. Neither changes anything, so both
// carry read hints, but git_push stays out in read-only mode with the other
// writes, since what it opens is a push.
func (t *tools) registerGitTools(server *mcp.Server) {
	if !t.cfg.Git.On() {
		return
	}
	addRepoTool(server, &mcp.Tool{
		Name:  "git_fetch",
		Title: "Check a git clone or fetch",
		Description: "Check that you may clone or fetch one repository, and get the git command for it. " +
			"Then clone and fetch with plain git: Straza's holder on this machine asks Straza for each one, so no token is needed.",
		Annotations: readHints("Check a git clone or fetch"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in gitHowInput) (*mcp.CallToolResult, gitHowOutput, error) {
		return t.gitHow(ctx, req, gitOpFetch, in.Repo, "")
	})
	if t.cfg.ReadOnly {
		return
	}
	addRepoTool(server, &mcp.Tool{
		Name:  "git_push",
		Title: "Check a git push",
		Description: "Check that you may push to one repository, and get the git command for it. " +
			"Then push with plain git: Straza's holder on this machine asks Straza for each push with its branch and commits, " +
			"so you need no token. A policy may ask a person to approve. " +
			"A push may create and update branches under refs/heads/ only: no deletes and no tags.",
		Annotations: readHints("Check a git push"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in gitPushHowInput) (*mcp.CallToolResult, gitHowOutput, error) {
		return t.gitHow(ctx, req, gitOpPush, in.Repo, in.Branch)
	})
}

// gitHow runs the checks every repo tool runs for op on the repo, and the
// token mint with the operation's permissions, so a repo the agent may not
// reach or a permission the App lacks shows now and not at the push. It
// answers with the plain git command and hands out nothing: the token is
// dropped.
func (t *tools) gitHow(ctx context.Context, req *mcp.CallToolRequest, op, repo, branch string) (*mcp.CallToolResult, gitHowOutput, error) {
	if branch != "" && !commandBranch(branch) {
		return nil, gitHowOutput{}, invalidInputf(branchArgument,
			"branch must be a branch name such as main or feature/login: ASCII letters, digits, '.', '_', '-' and '/' between parts, got %s.", quoted(truncate(plainText(branch), 120)))
	}
	acc, err := t.access(ctx, req, repo, gitOpPerms(op))
	if err != nil {
		return nil, gitHowOutput{}, err
	}
	out := gitHowOutput{
		Op:     op,
		Repo:   acc.Repo.FullName,
		RepoID: strconv.FormatInt(acc.Repo.ID, 10),
		Remote: strings.TrimRight(t.cfg.Git.Upstream, "/") + "/" + acc.Repo.FullName + ".git",
	}
	if op == gitOpPush {
		target, run := branch, "Run command from structuredContent"
		if target == "" {
			target = branchPlaceholder
			run = "Replace " + branchPlaceholder + " in command from structuredContent with the branch, and run it"
		}
		out.Command = "git push " + shellWord(out.Remote) + " " + shellWord("HEAD:refs/heads/"+target)
		return textResult(fmt.Sprintf("You may push to %s (id %s). %s, or push with any plain git command. "+
			"Straza's holder on this machine asks Straza for the push with its branch and commits, and a policy may ask a person to approve. "+
			"No token is needed.", out.Repo, out.RepoID, run)), out, nil
	}
	out.Command = "git clone " + shellWord(out.Remote)
	out.FetchCommand = "git fetch " + shellWord(out.Remote) + " " + shellWord("+refs/heads/*:refs/remotes/origin/*")
	return textResult(fmt.Sprintf("You may clone and fetch %s (id %s). Run command from structuredContent, or fetchCommand in an existing clone, "+
		"or any plain git command. Straza's holder on this machine asks Straza for each one. No token is needed.", out.Repo, out.RepoID)), out, nil
}

// shellWord quotes s for a POSIX shell unless it is made of characters a
// shell takes as they are. The placeholder is left bare, so that a command
// run before it is replaced fails in the shell.
func shellWord(s string) string {
	if strings.HasSuffix(s, branchPlaceholder) {
		return s
	}
	for _, r := range s {
		if !isASCIIAlnum(r) && !strings.ContainsRune("@%+=:,./_-", r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// commandBranch reports whether s is a branch the git endpoint lets a push
// update (validBranch) that also stands in a command line as it is: ASCII
// letters, digits, '.', '_', '-' and '/', not starting with '-'.
func commandBranch(s string) bool {
	if len(s) > 200 || strings.HasPrefix(s, "-") || !validBranch(s) {
		return false
	}
	for _, r := range s {
		if !isASCIIAlnum(r) && !strings.ContainsRune("._-/", r) {
			return false
		}
	}
	return true
}
