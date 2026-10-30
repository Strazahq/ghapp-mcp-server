package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The git steps. The agent types plain git, and Straza's holder on its
// machine (here the stand-in fakeholder) asks Straza for a grant for each
// git request and brings it to ghapp. git_fetch and git_push hand out no
// secret: they check the repo and give the command.

// gitHowOut is the structuredContent of git_fetch and git_push.
type gitHowOut struct {
	Op           string `json:"op"`
	Repo         string `json:"repo"`
	RepoID       string `json:"repoId"`
	Remote       string `json:"remote"`
	Command      string `json:"command"`
	FetchCommand string `json:"fetchCommand"`
}

// gitTool calls git_fetch or git_push and shows what it returns, all of it:
// it holds no secret.
func (d *demo) gitTool(ctx context.Context, tool string, args map[string]any) (gitHowOut, error) {
	d.called("Called", tool, args)
	res, err := d.call(ctx, tool, args)
	if err != nil {
		return gitHowOut{}, err
	}
	if res.IsError {
		d.t.field("Returned", "an error")
		d.t.block(resultText(res))
		return gitHowOut{}, errors.New(tool + " was refused")
	}
	var out gitHowOut
	if err := structured(res, &out); err != nil {
		return gitHowOut{}, err
	}
	b, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
	d.t.field("Returned", "text, then structuredContent:")
	d.t.block(resultText(res) + "\n\n" + string(b))
	d.gitBase = strings.TrimSuffix(out.Remote, out.Repo+".git")
	return out, nil
}

// holderEnv is the agent's git environment with the one setting Straza's
// holder needs, here in the environment rather than in a config file, so
// the command stays plain git. It sends git's URLs under base, the GitHub
// URL the git tools name, to the holder. In this demo that is the fake
// GitHub, ghapp's GHAPP_GIT_UPSTREAM.
func (d *demo) holderEnv() []string {
	return append(d.agentEnv(), "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url."+d.o.holderURL+"/git/.insteadOf", "GIT_CONFIG_VALUE_0="+d.gitBase)
}

// holderLines are the stand-in holder's log lines from mark on, without its
// prefix.
func (d *demo) holderLines(mark int64) string {
	var out []string
	for _, e := range newLog(d.o.holderLog, mark) {
		_, rest, ok := strings.Cut(e.raw, "holder): ")
		if !ok {
			rest = e.raw
		}
		out = append(out, rest)
	}
	return strings.Join(out, "\n")
}

// grantLines are the stand-in Straza's grant decisions from mark on.
func (d *demo) grantLines(mark int64) ([]logEntry, string) {
	entries := filter(newLog(d.o.strazaAudit, mark), "", map[string]string{"kind": "grant"})
	var out []string
	for _, e := range entries {
		out = append(out, e.show("tool", "op", "repo", "refs", "decision", "role", "grant_id", "decision_id"))
	}
	return entries, strings.Join(out, "\n")
}

// --- e ---

func (d *demo) stepE(ctx context.Context) error {
	if d.o.holderURL == "" {
		return errors.New("no holder: run the demo with demo/run.sh")
	}
	ghMark, stMark, hoMark := logSize(d.o.ghappLog), logSize(d.o.strazaAudit), logSize(d.o.holderLog)
	out, err := d.gitTool(ctx, "git_fetch", map[string]any{"repo": payAPI})
	if err != nil {
		return err
	}
	env := d.holderEnv()
	d.t.field("Setting", "url."+d.o.holderURL+"/git/.insteadOf "+d.gitBase+", the one git setting Straza's holder needs. "+d.gitBase+" is the fake GitHub here.")
	d.t.field("Ran", "the command from structuredContent as it is, with /bin/sh:")
	r := sh(d.o.work, env, out.Command)
	d.t.block("$ " + out.Command + "\n" + r.out)
	dir := filepath.Join(d.o.work, "pay-api")
	if !r.ok() {
		d.check("e", false, "the clone worked")
		return nil
	}
	files := run(dir, env, "git", "ls-files")
	cfg := run(dir, env, "git", "config", "--list", "--show-origin")
	d.t.field("Then", "git ls-files; git config --list --show-origin:")
	d.t.block(strings.TrimRight(files.out, "\n") + "\n\n" + cfg.out)
	d.t.field("Holder", "what the stand-in holder asked Straza for, from its log:")
	d.t.block(d.holderLines(hoMark))
	grants, text := d.grantLines(stMark)
	d.t.field("Straza", "the stand-in's audit: each grant, checked as a call of git_fetch:")
	d.t.block(text)
	var lines []string
	spent, viaGrant, all := 0, 0, 0
	for _, e := range filter(newLog(d.o.ghappLog, ghMark), "git", nil) {
		all++
		lines = append(lines, e.show("op", "step", "repo", "via", "grant_id", "spent", "status", "outcome"))
		if e.get("via") == "grant" {
			viaGrant++
		}
		if e.get("spent") == "true" {
			spent++
		}
	}
	d.t.field("ghapp log", "one line per HTTP request of the clone:")
	d.t.block(strings.Join(lines, "\n"))
	d.t.field("Shows", "git_fetch handed out no secret, only the command. Plain git went to the holder, which brought ghapp a fetch grant for each request. ghapp spent the grant whose answer started the pack, and streamed GitHub's answer with a token that never left ghapp. Nothing landed in git's config.")
	_, statErr := os.Stat(filepath.Join(dir, "src", "login.js"))
	d.check("e", statErr == nil, "the clone brought main, with src/login.js")
	d.check("e", !strings.Contains(strings.ToLower(cfg.out), "extraheader") && !strings.Contains(cfg.out, "credential"), "the repo's config holds no header and no helper")
	d.check("e", len(grants) > 0 && all == viaGrant && spent == 1, "every ghapp request came with a grant, and one fetch grant was spent")
	d.clone = dir
	return nil
}

// --- f ---

const fixedLogin = `// Signs a user in.
export function login(user, password) {
  // A reset password is stored trimmed, so compare it trimmed.
  return user.passwordHash === hash(password.trim());
}

function hash(s) {
  return 'h:' + s;
}
`

func (d *demo) stepF(ctx context.Context) error {
	if d.clone == "" {
		return errors.New("no clone from step e")
	}
	ghMark, stMark, auMark, hoMark := logSize(d.o.ghappLog), logSize(d.o.strazaAudit), logSize(d.o.githubAudit), logSize(d.o.holderLog)
	env := d.holderEnv()
	if r := run(d.clone, env, "git", "switch", "-q", "-c", "fix-login"); !r.ok() {
		return fmt.Errorf("git switch: %s", r.out)
	}
	if err := os.WriteFile(filepath.Join(d.clone, "src", "login.js"), []byte(fixedLogin), 0o600); err != nil {
		return err
	}
	commit := run(d.clone, env, "git", "commit", "-q", "-am", "Trim the password before comparing it")
	diff := run(d.clone, env, "git", "show", "--stat", "--format=%h %an: %s", "HEAD")
	if !commit.ok() {
		return fmt.Errorf("git commit: %s", commit.out)
	}
	head := strings.TrimSpace(run(d.clone, env, "git", "rev-parse", "HEAD").out)
	d.t.field("Edited", "src/login.js on a new branch fix-login, and committed:")
	d.t.block(diff.out)
	out, err := d.gitTool(ctx, "git_push", map[string]any{"repo": payAPI, "branch": "fix-login"})
	if err != nil {
		return err
	}
	d.t.field("Ran", "the command from structuredContent as it is:")
	r := sh(d.clone, env, out.Command)
	d.t.block("$ " + out.Command + "\n" + r.out)
	d.pushed = head
	glog := d.githubGit("log", "--format=%h %an: %s", "fix-login")
	d.t.field("GitHub", "git log fix-login in the fake GitHub's bare repo:")
	d.t.block(glog.out)
	d.t.field("Holder", "")
	d.t.block(d.holderLines(hoMark))
	grants, text := d.grantLines(stMark)
	d.t.field("Straza", "the stand-in's audit: each grant, checked as a call of git_push:")
	d.t.block(text)
	var granted []logEntry
	for _, e := range grants {
		// The push grant that names the updates. The one without refs opened
		// info/refs.
		if e.get("decision") == "allowed" && e.get("refs") != "" {
			granted = append(granted, e)
		}
	}
	var lines []string
	matched := false
	for _, e := range filter(newLog(d.o.ghappLog, ghMark), "git", nil) {
		lines = append(lines, e.show("step", "repo", "via", "grant_id", "decision", "spent", "refs", "status", "outcome"))
		if len(granted) == 1 && e.get("decision") == granted[0].get("decision_id") && e.get("spent") == "true" && e.get("outcome") == "ok" {
			matched = true
		}
	}
	d.t.field("ghapp log", "")
	d.t.block(strings.Join(lines, "\n"))
	audit := filter(newLog(d.o.githubAudit, auMark), "git.push", nil)
	if len(audit) > 0 {
		d.t.field("GitHub", "the fake GitHub's audit:")
		d.t.block(audit[0].show("actor", "repo", "refs", "hashed_token"))
	}
	d.t.field("Shows", "Straza saw the branch and the commits before any code moved, and its decision id is in ghapp's line for the push. The grant works only with the holder's key, which the agent cannot read, and nothing GitHub accepts reached the agent.")
	d.check("f", r.ok(), "the plain git push succeeded")
	d.check("f", d.githubRef("refs/heads/fix-login") == head, "the fake GitHub's fix-login is the agent's commit "+short(head))
	d.check("f", len(granted) == 1 && strings.Contains(granted[0].get("refs"), "refs/heads/fix-login 0000000.."+short(head)),
		"Straza granted git_push for exactly refs/heads/fix-login, new at "+short(head))
	d.check("f", matched, "ghapp took the push on that grant and spent it")
	d.check("f", len(audit) == 1 && audit[0].get("actor") == "ghapp[bot]", "the fake GitHub's audit shows one git.push by ghapp[bot]")
	return nil
}

// --- g ---

// stepG sends git straight to ghapp's /git, past the holder, as an agent
// that found ghapp's URL might.
func (d *demo) stepG(ctx context.Context) error {
	if d.clone == "" || d.pushed == "" {
		return errors.New("no push from step f")
	}
	env := d.agentEnv()
	run(d.clone, env, "git", "commit", "-q", "--allow-empty", "-m", "One more")
	url := d.o.ghappURL + "/git/" + payAPI + ".git"
	cmd := "git push " + url + " HEAD:refs/heads/fix-login"
	d.t.field("Ran", "an empty commit, then plain git straight to ghapp's git endpoint, without the holder's setting:")
	r := sh(d.clone, env, cmd)
	d.t.block("$ " + cmd + "\n" + r.out)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/info/refs?service=git-receive-pack", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	d.t.field("ghapp", "what ghapp answers such a request, read here with Go's HTTP client:")
	d.t.block(fmt.Sprintf("%s\nWWW-Authenticate: %s\n\n%s", resp.Status, resp.Header.Get("WWW-Authenticate"), body))
	d.t.field("Shows", "ghapp's git endpoint takes a Straza grant with the holder's proof and nothing else. Its challenge is DPoP only, so git has no password to ask for, and the agent holds nothing that opens it.")
	d.check("g", !r.ok(), "the push straight to ghapp failed")
	d.check("g", resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == `DPoP algs="EdDSA"` &&
		strings.Contains(string(body), "Straza grants only"), "ghapp answered 401 with the DPoP challenge only")
	d.check("g", d.githubRef("refs/heads/fix-login") == d.pushed, "the fake GitHub's fix-login did not move")
	// Back to the commit step f pushed, so the next steps start from it.
	run(d.clone, env, "git", "reset", "-q", "--hard", d.pushed)
	return nil
}

// --- h ---

func (d *demo) stepH(ctx context.Context) error {
	if d.clone == "" {
		return errors.New("no clone from step e")
	}
	ghMark, auMark := logSize(d.o.ghappLog), logSize(d.o.githubAudit)
	env := d.holderEnv()
	run(d.clone, env, "git", "tag", "v1.0.0")
	trace := filepath.Join(d.o.work, "trace-h.txt")
	cmd := "git push " + d.gitBase + payAPI + ".git refs/tags/v1.0.0"
	d.t.field("Ran", "git tag v1.0.0, then plain git through the holder, with GIT_TRACE_PACKET to a file:")
	r := sh(d.clone, append(env, "GIT_TRACE_PACKET="+trace), cmd)
	d.t.block("$ " + cmd + "\n" + r.out)
	var ng []string
	if b, err := os.ReadFile(trace); err == nil {
		// "git< " is the line as git read it, one pkt-line; the sideband
		// line before it holds the same bytes still framed.
		for _, l := range strings.Split(string(b), "\n") {
			if i := strings.Index(l, "git< ng refs/"); i >= 0 {
				ng = append(ng, l[i+len("git< "):])
			}
		}
	}
	d.t.field("ng line", "the report-status line ghapp sent, from git's packet trace:")
	d.t.block(strings.Join(ng, "\n"))
	var lines []string
	for _, e := range filter(newLog(d.o.ghappLog, ghMark), "git", map[string]string{"op": "push"}) {
		lines = append(lines, e.show("step", "via", "refs", "spent", "status", "outcome", "reason"))
	}
	d.t.field("ghapp log", "")
	d.t.block(strings.Join(lines, "\n"))
	newPushes := len(filter(newLog(d.o.githubAudit, auMark), "git.push", nil))
	d.t.field("GitHub", fmt.Sprintf("tag v1.0.0 in the bare repo: %q; new git.push audit lines: %d", d.githubRef("refs/tags/v1.0.0"), newPushes))
	d.t.field("Shows", "The stand-in Straza granted the tag, since its role covers the repo. ghapp reads the ref updates before it asks GitHub for a token, and lets through branch updates only. git sends the whole pack before it reads an answer, so the refusal comes back as git's own per-ref status: an ng line, shown as [remote rejected].")
	d.check("h", !r.ok() && strings.Contains(r.out, "[remote rejected]") && strings.Contains(r.out, "ghapp does not push tags"), "git printed the rejection with ghapp's reason")
	d.check("h", len(ng) == 1 && strings.HasPrefix(ng[0], "ng refs/tags/v1.0.0 ghapp does not push tags"), "the ng line names the tag and ghapp's reason")
	d.check("h", d.githubRef("refs/tags/v1.0.0") == "" && newPushes == 0, "the tag never reached the fake GitHub")
	return nil
}

// --- i ---

func (d *demo) stepI(ctx context.Context) error {
	if d.clone == "" {
		return errors.New("no clone from step e")
	}
	ghMark, stMark, hoMark := logSize(d.o.ghappLog), logSize(d.o.strazaAudit), logSize(d.o.holderLog)
	cmd := "git push " + d.gitBase + straza + ".git HEAD:refs/heads/fix-login"
	d.t.field("Ran", "the same branch, pushed through the holder to "+straza+", which no role covers:")
	r := sh(d.clone, d.holderEnv(), cmd)
	d.t.block("$ " + cmd + "\n" + r.out)
	d.t.field("Holder", "")
	d.t.block(d.holderLines(hoMark))
	_, text := d.grantLines(stMark)
	d.t.field("Straza", "")
	d.t.block(text)
	seen := len(filter(newLog(d.o.ghappLog, ghMark), "git", nil))
	d.t.field("Shows", "Straza refused the grant, the holder passed its sentence to git, and ghapp never saw the push.")
	d.check("i", !r.ok() && strings.Contains(r.out, "git_push on "+straza+" was refused"), "git printed Straza's refusal")
	d.check("i", seen == 0, "ghapp never saw the refused push")
	return nil
}

// --- j ---

func (d *demo) stepJ(ctx context.Context) error {
	gh := newLog(d.o.ghappLog, 0)
	audit := newLog(d.o.githubAudit, 0)
	stz := newLog(d.o.strazaAudit, 0)
	mints := filter(gh, "token minted", nil)
	mintsOf := map[string][]logEntry{}
	for _, m := range mints {
		mintsOf[m.get("token_hash")] = append(mintsOf[m.get("token_hash")], m)
	}

	// The push of step f through all three logs: the ones whose new commit
	// is step f's.
	var push, grant, gitLine logEntry
	for _, e := range filter(audit, "git.push", nil) {
		if strings.Contains(e.get("refs"), `"after":"`+d.pushed+`"`) && push.raw == "" {
			push = e
		}
	}
	for _, e := range filter(stz, "", map[string]string{"kind": "grant", "decision": "allowed"}) {
		if strings.HasSuffix(e.get("refs"), ".."+short(d.pushed)) && grant.raw == "" {
			grant = e
		}
	}
	for _, e := range filter(gh, "git", map[string]string{"op": "push", "spent": "true", "outcome": "ok"}) {
		if strings.HasSuffix(e.get("refs"), " "+d.pushed) && gitLine.raw == "" {
			gitLine = e
		}
	}
	d.t.field("Push", "step f's push of fix-login, "+short(d.pushed)+", in each log:")
	d.t.block(fmt.Sprintf("%-20s %s\n%-20s %s\n%-20s %s",
		"1 Straza (stand-in)", grant.show("agent", "tool", "repo", "refs", "decision", "decision_id"),
		"2 ghapp", gitLine.show("op", "repo", "sub", "decision", "refs", "hashed_token"),
		"3 GitHub (stand-in)", push.show("actor", "repo", "hashed_token")))
	d.t.field("Links", "Straza's line names the agent and its decision id, which ghapp's line carries from the grant. ghapp's line and GitHub's share the token hash.")

	// Every write GitHub logged, beside the mint that made its token.
	rows := []string{fmt.Sprintf("%-26s %-19s  %-19s  %s", "GitHub audit event", "hashed_token", "ghapp token_hash", "")}
	used := map[string][]string{}
	var order []string
	matched, events := 0, 0
	for _, e := range audit {
		events++
		h := e.get("hashed_token")
		what := e.msg
		if e.msg == "git.push" {
			var refs []struct {
				Ref   string `json:"ref"`
				After string `json:"after"`
			}
			if json.Unmarshal([]byte(e.get("refs")), &refs) == nil && len(refs) == 1 {
				what += " " + strings.TrimPrefix(refs[0].Ref, "refs/heads/") + " " + short(refs[0].After)
			}
		}
		mark, mh := "NO MATCH", "(none)"
		if len(mintsOf[h]) > 0 {
			matched++
			mark, mh = "same", abbrev(mintsOf[h][0].get("token_hash"))
		}
		if _, seen := used[h]; !seen {
			order = append(order, h)
		}
		used[h] = append(used[h], what)
		rows = append(rows, fmt.Sprintf("%-26s %-19s  %-19s  %s", what, abbrev(h), mh, mark))
	}
	var details []string
	for _, h := range order {
		for _, m := range mintsOf[h] {
			details = append(details, fmt.Sprintf("%s minted once, for caller %s, repo_ids %s, %s; used by %s",
				abbrev(h), m.get("caller.kind"), m.get("repo_ids"), permsText(m.get("perms")), strings.Join(used[h], " and ")))
		}
	}
	d.t.field("Hashes", "each event in the fake GitHub's audit beside ghapp's \"token minted\" line with the same hash. The first 16 characters are shown, and the whole hash is compared:")
	d.t.block(strings.Join(rows, "\n") + "\n\n" + strings.Join(details, "\n"))
	d.t.field("Shows", "GitHub sees one bot, ghapp[bot]. The hash tells the tokens apart, and ghapp's log says whom each token was for and what it could reach: the pull request's for the gateway, the push's for the agent session named in the grant.")
	d.check("j", events >= 2 && matched == events, fmt.Sprintf("all %d GitHub audit events match a ghapp mint by hash", events))
	d.check("j", grant.raw != "" && gitLine.get("decision") == grant.get("decision_id"), "ghapp's git line carries Straza's decision id for the push")
	d.check("j", push.raw != "" && gitLine.get("hashed_token") == push.get("hashed_token") && push.get("hashed_token") != "", "ghapp's git line and GitHub's git.push of step f carry the same hash")
	pushMint := mintsOf[push.get("hashed_token")]
	d.check("j", len(pushMint) == 1 && pushMint[0].get("caller.kind") == "grant", "the push's token was minted once, for the grant's agent session")
	return nil
}

func abbrev(h string) string {
	if len(h) > 16 {
		return h[:16] + "..."
	}
	return h
}
