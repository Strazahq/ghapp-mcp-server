// Command client plays the demo's scenario as the agent joe-macos-agent and
// prints a readable transcript. It talks MCP over stdio to fakestraza, the
// stand-in for the agent's Straza session, and runs real git for the clone
// and the pushes, through fakeholder, the stand-in for Straza's holder.
// demo/run.sh starts what it needs first.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	agent   = "joe-macos-agent"
	payAPI  = "Strazahq/pay-api"
	payID   = "812345678"
	straza  = "Strazahq/straza"
	refused = "pr_create on Strazahq/straza was refused: none of your roles on ghapp covers that repo."
)

type options struct {
	fakestraza  string // the fakestraza binary
	ghappURL    string // ghapp's base URL, as agents reach it
	ghappLog    string
	githubAudit string
	strazaAudit string
	gitRoot     string // fakegithub's bare repos
	work        string // the agent's working directory
	out         string // the transcript file
	holderURL   string // the stand-in holder
	holderLog   string
}

func main() {
	var o options
	flag.StringVar(&o.fakestraza, "fakestraza", "", "the fakestraza binary")
	flag.StringVar(&o.ghappURL, "ghapp-url", "", "ghapp's base URL, such as http://127.0.0.1:8080")
	flag.StringVar(&o.ghappLog, "ghapp-log", "", "ghapp's log, its stderr")
	flag.StringVar(&o.githubAudit, "github-audit", "", "fakegithub's audit log")
	flag.StringVar(&o.strazaAudit, "straza-audit", "", "fakestraza's audit log")
	flag.StringVar(&o.gitRoot, "github-git-root", "", "fakegithub's bare repos")
	flag.StringVar(&o.work, "work", "", "an empty directory for the agent's clones")
	flag.StringVar(&o.out, "out", "", "also write the transcript to this file")
	flag.StringVar(&o.holderURL, "holder-url", "", "the stand-in holder's base URL")
	flag.StringVar(&o.holderLog, "holder-log", "", "the stand-in holder's log")
	flag.Parse()
	for name, v := range map[string]string{"-fakestraza": o.fakestraza, "-ghapp-url": o.ghappURL, "-ghapp-log": o.ghappLog,
		"-github-audit": o.githubAudit, "-straza-audit": o.strazaAudit, "-github-git-root": o.gitRoot, "-work": o.work,
		"-holder-url": o.holderURL, "-holder-log": o.holderLog} {
		if v == "" {
			fmt.Fprintf(os.Stderr, "client: %s is required\n", name)
			os.Exit(2)
		}
	}
	var w io.Writer = os.Stdout
	var file *os.File
	if o.out != "" {
		var err error
		if file, err = os.Create(o.out); err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
			os.Exit(1)
		}
		w = io.MultiWriter(os.Stdout, file)
	}
	d := &demo{o: o, t: &transcript{w: w}}
	code := d.run(context.Background())
	if file != nil {
		if err := file.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// demo is the scenario's state as it runs.
type demo struct {
	o  options
	t  *transcript
	cs *mcp.ClientSession

	failed []string

	clone   string // step e's clone
	gitBase string // the GitHub URL the git tools name, the holder's setting
	pushed  string // the commit step f pushed
}

// check records whether what holds, and says so.
func (d *demo) check(id string, ok bool, what string) {
	if ok {
		d.t.field("Check", "ok: "+what)
		return
	}
	d.t.field("Check", "FAILED: "+what)
	d.failed = append(d.failed, id+": "+what)
}

func (d *demo) run(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	gitVersion, _ := exec.Command("git", "version").Output()
	d.t.title("ghapp-mcp-server demo: one agent, one role, four repos")
	d.t.field("When", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	d.t.field("Real", "ghapp-mcp-server, built from this repo for this run, and "+strings.TrimSpace(string(gitVersion))+".")
	d.t.field("Stand-ins", "fakegithub stands in for GitHub, fakestraza for Straza, and fakeholder for Straza's holder on the agent's machine. None is the real thing: each does only what this demo needs, on this machine, offline.")
	d.t.field("Agent", agent+", holding the role ghapp-write-payments: pr_create, pr_get, issue_create, git_fetch and git_push, on repos where team is payments (demo/roles.json).")
	d.t.field("Callers", "fakestraza calls ghapp with the gateway secret, and with the listing token for repo_list. Behind the gateway secret ghapp cannot tell agents apart.")

	steps := []struct {
		id    string
		title string
		run   func(context.Context) error
	}{
		{"a", "tools/list: what the agent sees", d.stepA},
		{"b", "repo_list, as the stand-in calls it with the listing token", d.stepB},
		{"c", "pr_create on Strazahq/pay-api: a covered repo", d.stepC},
		{"d", "pr_create on Strazahq/straza: a repo no role covers", d.stepD},
		{"e", "git_fetch, then the clone it gives, with plain git through Straza's holder", d.stepE},
		{"f", "A commit, git_push with branch fix-login, and the push it gives", d.stepF},
		{"g", "Plain git straight to ghapp, past the holder", d.stepG},
		{"h", "A tag push", d.stepH},
		{"i", "A push to Strazahq/straza, a repo no role covers", d.stepI},
		{"j", "Audit: one push, followed through three logs", d.stepJ},
	}
	for _, s := range steps {
		d.t.step(s.id, s.title)
		if err := s.run(ctx); err != nil {
			d.t.field("Error", err.Error())
			d.failed = append(d.failed, s.id+": "+err.Error())
		}
	}
	if d.cs != nil {
		_ = d.cs.Close()
	}

	// Nothing secret in the transcript: no secret of the stand-in, and no
	// grant, which only the holder sees.
	leaked := strings.Contains(d.t.copy.String(), "DPoP ey")
	for _, s := range []string{os.Getenv("FAKESTRAZA_GATEWAY_SECRET"), os.Getenv("FAKESTRAZA_LISTING_TOKEN")} {
		if s != "" && strings.Contains(d.t.copy.String(), s) {
			leaked = true
		}
	}
	d.t.printf("\n")
	d.t.rule("=")
	if leaked {
		d.failed = append(d.failed, "a grant or a secret appears in the transcript")
	} else {
		d.t.field("Secrets", "No grant, gateway secret or listing token appears in this transcript, and no GitHub token ever reached the agent's side.")
	}
	if len(d.failed) == 0 {
		d.t.field("Result", fmt.Sprintf("All %d steps behaved as expected.", len(steps)))
		d.t.rule("=")
		return 0
	}
	d.t.field("Result", fmt.Sprintf("%d check(s) failed:\n%s", len(d.failed), strings.Join(d.failed, "\n")))
	d.t.rule("=")
	return 1
}

// --- MCP ---

// call calls a tool through fakestraza and returns its result.
func (d *demo) call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	return d.cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// structured decodes a result's structuredContent into out.
func structured(res *mcp.CallToolResult, out any) error {
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// called says which tool a step calls through fakestraza, with its
// arguments on a line of their own.
func (d *demo) called(label, tool string, args map[string]any) {
	d.t.field(label, tool+", through fakestraza, with the arguments:")
	d.t.block(argsText(args))
}

func argsText(args map[string]any) string {
	b, _ := json.Marshal(args)
	return string(b)
}

// --- running programs ---

type result struct {
	out  string
	code int
	err  error
}

func (r result) ok() bool { return r.err == nil && r.code == 0 }

func run(dir string, env []string, name string, args ...string) result {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return result{out: buf.String(), code: ee.ExitCode()}
	}
	return result{out: buf.String(), err: err}
}

// sh runs a command line as an agent's shell would.
func sh(dir string, env []string, line string) result {
	return run(dir, env, "/bin/sh", "-c", line)
}

// agentEnv is the environment of the agent's own git: no credential, no
// git config but the repo's own, and its name on commits.
func (d *demo) agentEnv() []string {
	home := filepath.Join(d.o.work, "agent-home")
	_ = os.MkdirAll(home, 0o700)
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=" + agent, "GIT_AUTHOR_EMAIL=" + agent + "@agents.strazahq.invalid",
		"GIT_COMMITTER_NAME=" + agent, "GIT_COMMITTER_EMAIL=" + agent + "@agents.strazahq.invalid",
		"LANG=C", "LC_ALL=C",
	}
}

// bare is fakegithub's bare repo of pay-api.
func (d *demo) bare() string { return filepath.Join(d.o.gitRoot, "Strazahq", "pay-api.git") }

// githubGit runs git on fakegithub's bare repo of pay-api, to look at what
// GitHub's stand-in holds.
func (d *demo) githubGit(args ...string) result {
	return run(d.o.gitRoot, d.agentEnv(), "git", append([]string{"--git-dir=" + d.bare()}, args...)...)
}

func (d *demo) githubRef(ref string) string {
	r := d.githubGit("rev-parse", "--verify", "-q", ref)
	return strings.TrimSpace(r.out)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// newLog reads a log from offset on.
func newLog(path string, from int64) []logEntry {
	entries, _ := readLog(path, from)
	return entries
}
