package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- a ---

func (d *demo) stepA(ctx context.Context) error {
	cmd := exec.Command(d.o.fakestraza, "mcp", "ghapp")
	if f, err := os.OpenFile(filepath.Join(d.o.work, "fakestraza.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stderr = f
		defer f.Close()
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "demo-client", Title: agent, Version: "demo"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("starting fakestraza mcp ghapp: %w", err)
	}
	d.cs = cs
	d.t.field("Called", "tools/list, in the agent's MCP session over stdio with `fakestraza mcp ghapp`, as an agent runs `straza mcp ghapp`.")
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	d.t.field("Returned", strings.Join(names, ", "))
	if init := cs.InitializeResult(); init != nil {
		d.t.field("Server", init.Instructions)
	}
	d.t.field("Shows", "The agent sees only the tools its role holds. Straza decides per tool, so pr_merge, the other write tools and repo_list are not even listed.")
	d.check("a", slices.Equal(names, []string{"git_fetch", "git_push", "issue_create", "pr_create", "pr_get"}),
		"the agent sees exactly the role's 5 tools")
	return nil
}

// --- b ---

func (d *demo) stepB(ctx context.Context) error {
	mark := logSize(d.o.ghappLog)
	d.t.field("Called", "`fakestraza list ghapp`: the stand-in calls repo_list on ghapp with the listing token, page after page until nextCursor is absent, as Straza does every 10 minutes.")
	r := run(d.o.work, os.Environ(), d.o.fakestraza, "list", "ghapp")
	if !r.ok() {
		d.t.block(r.out)
		return fmt.Errorf("fakestraza list ghapp failed: %v %d", r.err, r.code)
	}
	var listing struct {
		Pages   int `json:"pages"`
		Objects []struct {
			Value      string         `json:"value"`
			ID         string         `json:"id"`
			Attributes map[string]any `json:"attributes"`
		} `json:"objects"`
	}
	if err := json.Unmarshal([]byte(r.out), &listing); err != nil {
		return err
	}
	rows := []string{fmt.Sprintf("%-20s %-11s %s", "repo (value)", "id", "team")}
	got := map[string]string{}
	for _, o := range listing.Objects {
		team, _ := o.Attributes["team"].(string)
		rows = append(rows, fmt.Sprintf("%-20s %-11s %s", o.Value, o.ID, team))
		got[o.Value] = o.ID + " " + team
	}
	d.t.field("Returned", fmt.Sprintf("%d repos on %d page(s), with GitHub's id and the custom property team:", len(listing.Objects), listing.Pages))
	d.t.block(strings.Join(rows, "\n"))
	calls := filter(newLog(d.o.ghappLog, mark), "tool call", map[string]string{"tool": "repo_list"})
	if len(calls) > 0 {
		d.t.field("ghapp log", "")
		d.t.block(calls[0].show("tool", "caller.kind", "token_hash", "outcome"))
	}
	d.t.field("Shows", "Straza learns each repo's id and team from ghapp, which reads them from GitHub. The role's rule, team is payments, covers pay-api and pay-web, and nothing else. ghapp took the listing token as caller kind listing, which opens repo_list and no other tool.")
	d.check("b", got[payAPI] == payID+" payments" && got["Strazahq/pay-web"] != "" && strings.HasSuffix(got["Strazahq/pay-web"], " payments") &&
		strings.HasSuffix(got[straza], " platform") && strings.HasSuffix(got["Strazahq/site"], " web"),
		"4 repos, pay-api is 812345678 and team payments, straza is platform, site is web")
	d.check("b", len(calls) > 0 && calls[0].get("caller.kind") == "listing", "ghapp logged repo_list for caller kind listing")
	return nil
}

// --- c ---

func (d *demo) stepC(ctx context.Context) error {
	ghMark, auMark, stMark := logSize(d.o.ghappLog), logSize(d.o.githubAudit), logSize(d.o.strazaAudit)
	args := map[string]any{"repo": payAPI, "title": "Add a health check", "head": "add-healthcheck", "base": "main",
		"body": "Adds a health check for the load balancer."}
	d.called("Called", "pr_create", args)
	res, err := d.call(ctx, "pr_create", args)
	if err != nil {
		return err
	}
	d.t.field("Returned", "")
	d.t.block(resultText(res))
	var w struct {
		Applied bool   `json:"applied"`
		Number  int    `json:"number"`
		RepoID  string `json:"repoId"`
		URL     string `json:"url"`
	}
	if err := structured(res, &w); err != nil || res.IsError {
		return fmt.Errorf("pr_create did not apply: %s", resultText(res))
	}

	get := map[string]any{"repo": payAPI, "number": w.Number}
	d.called("Then", "pr_get", get)
	res, err = d.call(ctx, "pr_get", get)
	if err != nil {
		return err
	}
	var pr struct {
		Pull struct {
			Title  string `json:"title"`
			Body   string `json:"body"`
			Author string `json:"author"`
			Head   string `json:"head"`
			Base   string `json:"base"`
			State  string `json:"state"`
		} `json:"pull"`
	}
	if err := structured(res, &pr); err != nil || res.IsError {
		return fmt.Errorf("pr_get failed: %s", resultText(res))
	}
	p := pr.Pull
	d.t.field("Returned", "the pull request as the fake GitHub holds it:")
	d.t.block(fmt.Sprintf("#%d %q, %s, %s into %s, by %s\nbody: %s", w.Number, p.Title, p.State, p.Head, p.Base, p.Author,
		strings.ReplaceAll(p.Body, "\n", "\n      ")))
	footer := strings.Contains(p.Body, "Sent by agent")
	if footer {
		d.t.field("Footer", "The body ends with ghapp's footer naming the agent.")
	} else {
		d.t.field("Footer", "None. ghapp ends what an agent writes with \"Sent by agent `<client>` through ghapp-mcp-server.\" only when an identity provider token names the agent's client. The stand-in calls with the gateway secret, so ghapp does not know the agent, and GitHub shows ghapp[bot] alone. Straza's audit names the agent.")
	}

	stLines := newLog(d.o.strazaAudit, stMark)
	if len(stLines) > 0 {
		d.t.field("Straza", "the stand-in's audit:")
		d.t.block(stLines[0].show("agent", "tool", "repo", "decision", "role", "object_id"))
	}
	gh := newLog(d.o.ghappLog, ghMark)
	mints := filter(gh, "token minted", nil)
	calls := filter(gh, "tool call", map[string]string{"tool": "pr_create"})
	d.t.field("ghapp log", "")
	var shown []string
	for _, m := range mints {
		shown = append(shown, m.show("caller.kind", "repo_ids", "perms", "token_hash"))
	}
	for _, c := range calls {
		shown = append(shown, c.show("tool", "caller.kind", "repo", "repo_id", "token_hash", "outcome"))
	}
	d.t.block(strings.Join(shown, "\n"))
	audit := filter(newLog(d.o.githubAudit, auMark), "pull_request.create", nil)
	if len(audit) > 0 {
		d.t.field("GitHub", "the fake GitHub's audit:")
		d.t.block(audit[0].show("actor", "repo", "hashed_token"))
	}
	d.t.field("Shows", "Two locks. The stand-in found pay-api's team payments in its listing and sent the id 812345678 in params._meta[\"ai.straza/object-id\"]. ghapp minted a token for that one repo id with pull_requests write and contents read only, and GitHub's stand-in held the call to it.")

	d.check("c", w.Applied && w.Number > 0 && w.RepoID == payID, "the pull request was created in Strazahq/pay-api (812345678)")
	d.check("c", len(stLines) > 0 && stLines[0].get("object_id") == payID, "the stand-in sent object-id 812345678")
	d.check("c", len(calls) == 1 && calls[0].get("repo_id") == payID && calls[0].get("outcome") == "ok", "ghapp's tool call line has repo_id 812345678 and outcome ok")
	okMint := false
	for _, m := range mints {
		if m.get("repo_ids") == "[812345678]" && permsText(m.get("perms")) == "contents: read, pull_requests: write" {
			okMint = true
		}
	}
	d.check("c", okMint, "ghapp minted for repo_ids [812345678] with contents: read, pull_requests: write")
	d.check("c", !footer, "no footer, since behind the gateway secret ghapp does not know the agent")
	d.check("c", len(audit) == 1 && audit[0].get("actor") == "ghapp[bot]", "the fake GitHub's audit shows ghapp[bot] creating it")
	return nil
}

// --- d ---

func (d *demo) stepD(ctx context.Context) error {
	ghMark, auMark, stMark := logSize(d.o.ghappLog), logSize(d.o.githubAudit), logSize(d.o.strazaAudit)
	args := map[string]any{"repo": straza, "title": "Add a health check", "head": "main", "base": "main"}
	d.called("Called", "pr_create", args)
	res, err := d.call(ctx, "pr_create", args)
	if err != nil {
		return err
	}
	d.t.field("Returned", fmt.Sprintf("isError %v:", res.IsError))
	d.t.block(resultText(res))
	stLines := newLog(d.o.strazaAudit, stMark)
	if len(stLines) > 0 {
		d.t.field("Straza", "the stand-in's audit:")
		d.t.block(stLines[0].show("agent", "tool", "repo", "decision"))
	}
	seen := 0
	for _, e := range newLog(d.o.ghappLog, ghMark) {
		if strings.Contains(e.raw, straza) {
			seen++
		}
	}
	auditSeen := 0
	for _, e := range newLog(d.o.githubAudit, auMark) {
		if strings.Contains(e.raw, straza) {
			auditSeen++
		}
	}
	d.t.field("ghapp log", fmt.Sprintf("%d line(s) name Strazahq/straza since this call.", seen))
	d.t.field("GitHub", fmt.Sprintf("%d audit line(s) name Strazahq/straza.", auditSeen))
	d.t.field("Shows", "The repo's team is platform, and no role the agent holds covers it, so the call stops at Straza with Straza's sentence and the repos the roles do cover. ghapp and GitHub never see it.")
	first := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			first = tc.Text
		}
	}
	d.check("d", res.IsError && first == refused, "refused with Straza's sentence")
	d.check("d", seen == 0 && auditSeen == 0, "ghapp and the fake GitHub never saw the call")
	d.check("d", len(stLines) > 0 && stLines[0].get("decision") == "refused", "the stand-in's audit records the refusal")
	return nil
}
