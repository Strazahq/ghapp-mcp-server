package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// gitPath is git's smart HTTP at /{owner}/{name}.git/, as on github.com.
var gitPath = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// serveGit serves one request of git's smart HTTP protocol through `git
// http-backend`. It takes the installation token as the Basic password, as
// GitHub does, for that token's repos only, with contents write for a push
// and contents read for a fetch.
func (f *fake) serveGit(w http.ResponseWriter, r *http.Request, owner, name, tail string) {
	service := tail
	if tail == "info/refs" {
		service = r.URL.Query().Get("service")
		if r.Method != http.MethodGet {
			gitError(w, http.StatusMethodNotAllowed, "info/refs takes GET.")
			return
		}
	} else if r.Method != http.MethodPost {
		gitError(w, http.StatusMethodNotAllowed, tail+" takes POST.")
		return
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		gitError(w, http.StatusForbidden, "fakegithub serves git's smart HTTP protocol only.")
		return
	}
	_, password, ok := r.BasicAuth()
	if !ok || password == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		gitError(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	f.mu.Lock()
	tok, known := f.tokens[password]
	rp := f.repoByName(owner, name)
	f.mu.Unlock()
	if !known || !f.now().Before(tok.Expires) {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		gitError(w, http.StatusUnauthorized, "Invalid username or token. Password authentication is not supported for Git operations.")
		return
	}
	if rp == nil || !tok.reaches(rp) {
		gitError(w, http.StatusNotFound, "Repository not found.")
		return
	}
	push := service == "git-receive-pack"
	if push && !tok.has("contents", "write") {
		gitError(w, http.StatusForbidden, "Write access to repository not granted.")
		return
	}
	if !tok.has("contents", "read") {
		gitError(w, http.StatusForbidden, "Read access to repository not granted.")
		return
	}

	// git http-backend reads the repo from the path: the canonical one.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + rp.fullName() + ".git/" + tail
	r2.URL.RawPath = ""
	r2.Header.Del("Authorization")
	r2.Body = r.Body
	backend := &cgi.Handler{
		Path: f.backend,
		Root: "/",
		Env: append(f.gitEnv(),
			"GIT_PROJECT_ROOT="+f.gitRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER="+botLogin,
		),
	}
	if !push || tail == "info/refs" {
		backend.ServeHTTP(w, r2)
		return
	}

	// A push: the refs before and after tell what it changed.
	rp.pushMu.Lock()
	defer rp.pushMu.Unlock()
	before, err := f.refs(rp)
	if err != nil {
		gitError(w, http.StatusInternalServerError, "fakegithub could not read the refs.")
		return
	}
	backend.ServeHTTP(w, r2)
	after, err := f.refs(rp)
	if err != nil {
		log.Printf("reading the refs of %s after a push: %v", rp.fullName(), err)
		return
	}
	all := maps.Clone(before)
	maps.Copy(all, after)
	var changed []refUpdate
	for _, ref := range slices.Sorted(maps.Keys(all)) {
		if before[ref] != after[ref] {
			changed = append(changed, refUpdate{Ref: ref, Before: orZeros(before[ref]), After: orZeros(after[ref])})
		}
	}
	if len(changed) > 0 {
		f.record(auditEvent{Action: "git.push", Repo: rp.fullName(), RepositoryID: rp.ID, HashedToken: hashedToken(password),
			Transport: "http", Refs: changed})
	}
}

func orZeros(sha string) string {
	if sha == "" {
		return strings.Repeat("0", 40)
	}
	return sha
}

// gitError answers git with one plain line, which git prints as remote: ...
func gitError(w http.ResponseWriter, status int, line string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, line)
}

// --- running git ---

// gitEnv keeps the machine's git config out of every git the fake runs.
func (f *fake) gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(f.gitRoot, ".home"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LANG=C", "LC_ALL=C",
	}
}

// git runs git in rp's bare repo, or in the git root for rp nil.
func (f *fake) git(rp *repo, extraEnv []string, stdin []byte, args ...string) (string, error) {
	dir := f.gitRoot
	if rp != nil {
		dir = f.bareDir(rp)
	}
	cmd := exec.Command(f.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(f.gitEnv(), extraEnv...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), &gitFailure{args: args, err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return out.String(), nil
}

type gitFailure struct {
	args   []string
	err    error
	stderr string
}

func (e *gitFailure) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitFailure) exitCode() int {
	var ee *exec.ExitError
	if errors.As(e.err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (f *fake) bareDir(rp *repo) string {
	return filepath.Join(f.gitRoot, rp.Owner, rp.Name+".git")
}

// errNoObject is a ref or path that does not exist.
var errNoObject = errors.New("no such object")

// safeRef refuses a ref that git could read as an option or a path.
func safeRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "-") && !strings.ContainsAny(ref, ": \t\n\x00")
}

func (f *fake) revParse(rp *repo, ref string) (string, error) {
	if !safeRef(ref) {
		return "", errNoObject
	}
	out, err := f.git(rp, nil, nil, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil {
		return "", errNoObject
	}
	return strings.TrimSpace(out), nil
}

// refs maps every ref of rp to its object.
func (f *fake) refs(rp *repo) (map[string]string, error) {
	out, err := f.git(rp, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			refs[name] = sha
		}
	}
	return refs, nil
}

// isAncestor reports whether a is an ancestor of b, or the same commit.
func (f *fake) isAncestor(rp *repo, a, b string) bool {
	_, err := f.git(rp, nil, nil, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// mergeTree writes the tree of merging head into base, or errConflict.
func (f *fake) mergeTree(rp *repo, base, head string) (string, error) {
	out, err := f.git(rp, nil, nil, "merge-tree", "--write-tree", "--no-messages", base, head)
	var gf *gitFailure
	if errors.As(err, &gf) && gf.exitCode() == 1 {
		return "", errConflict
	}
	if err != nil {
		return "", err
	}
	tree, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(tree), nil
}

func (f *fake) canMerge(rp *repo, base, head string) bool {
	if base == "" || head == "" {
		return false
	}
	_, err := f.mergeTree(rp, base, head)
	return err == nil
}

var (
	errConflict     = errors.New("the branches conflict")
	errNotRebasable = errors.New("the head cannot be rebased onto the base")
)

// merge merges pull request is with method and moves its base branch. It
// returns the new head of the base branch.
func (f *fake) merge(rp *repo, is *issue, method, base, head string) (string, error) {
	env := []string{
		"GIT_AUTHOR_NAME=" + botLogin, "GIT_AUTHOR_EMAIL=ghapp[bot]@users.noreply.example",
		"GIT_COMMITTER_NAME=GitHub (fakegithub)", "GIT_COMMITTER_EMAIL=noreply@fakegithub.invalid",
	}
	var sha string
	switch method {
	case "merge", "squash":
		tree, err := f.mergeTree(rp, base, head)
		if err != nil {
			return "", err
		}
		args := []string{"commit-tree", tree, "-p", base}
		msg := fmt.Sprintf("%s (#%d)", is.Title, is.Number)
		if method == "merge" {
			args = append(args, "-p", head)
			msg = fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s", is.Number, rp.Owner, is.Head, is.Title)
		}
		out, err := f.git(rp, env, nil, append(args, "-m", msg)...)
		if err != nil {
			return "", err
		}
		sha = strings.TrimSpace(out)
	case "rebase":
		if !f.isAncestor(rp, base, head) {
			return "", errNotRebasable
		}
		sha = head
	default:
		return "", fmt.Errorf("unknown merge method %q", method)
	}
	if _, err := f.git(rp, nil, nil, "update-ref", "refs/heads/"+is.Base, sha, base); err != nil {
		return "", err
	}
	return sha, nil
}

// pathEntry is one file or directory a contents read answers with.
type pathEntry struct {
	kind    string // file, dir, symlink or submodule
	path    string
	sha     string
	size    int64
	content []byte
}

// readPath reads p at ref: a directory's entries, or a file with its
// content. The root is p "".
func (f *fake) readPath(rp *repo, ref, p string) (entries []pathEntry, isDir bool, file pathEntry, err error) {
	if !safeRef(ref) || strings.HasPrefix(p, "-") {
		return nil, false, pathEntry{}, errNoObject
	}
	spec := ref + ":" + p
	kind, err := f.git(rp, nil, nil, "cat-file", "-t", spec)
	if err != nil {
		return nil, false, pathEntry{}, errNoObject
	}
	if strings.TrimSpace(kind) == "tree" {
		out, err := f.git(rp, nil, nil, "ls-tree", "-l", "-z", spec)
		if err != nil {
			return nil, false, pathEntry{}, err
		}
		for _, item := range strings.Split(out, "\x00") {
			meta, name, ok := strings.Cut(item, "\t")
			if !ok {
				continue
			}
			fields := strings.Fields(meta)
			if len(fields) < 4 {
				continue
			}
			size, _ := strconv.ParseInt(fields[3], 10, 64)
			full := name
			if p != "" {
				full = p + "/" + name
			}
			entries = append(entries, pathEntry{kind: entryKind(fields[0], fields[1]), path: full, sha: fields[2], size: size})
		}
		return entries, true, pathEntry{}, nil
	}
	// A file: its mode tells a symlink from a file.
	out, err := f.git(rp, nil, nil, "ls-tree", "-z", ref, "--", p)
	if err != nil || out == "" {
		return nil, false, pathEntry{}, errNoObject
	}
	fields := strings.Fields(strings.SplitN(out, "\t", 2)[0])
	if len(fields) < 3 {
		return nil, false, pathEntry{}, errNoObject
	}
	content, err := f.git(rp, nil, nil, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, pathEntry{}, err
	}
	return nil, false, pathEntry{kind: entryKind(fields[0], fields[1]), path: p, sha: fields[2], size: int64(len(content)), content: []byte(content)}, nil
}

func entryKind(mode, typ string) string {
	switch {
	case typ == "tree":
		return "dir"
	case typ == "commit":
		return "submodule"
	case mode == "120000":
		return "symlink"
	}
	return "file"
}

// codeHit is one file a code search found.
type codeHit struct{ path, sha string }

// grep finds the files on rp's default branch that hold every term, without
// regard to case.
func (f *fake) grep(rp *repo, terms []string) ([]codeHit, error) {
	args := []string{"grep", "-l", "-I", "-i", "-F", "--all-match"}
	for _, t := range terms {
		args = append(args, "-e", t)
	}
	args = append(args, rp.DefaultBranch, "--")
	out, err := f.git(rp, nil, nil, args...)
	var gf *gitFailure
	if errors.As(err, &gf) && gf.exitCode() == 1 {
		return nil, nil // no match
	}
	if err != nil {
		return nil, err
	}
	var hits []codeHit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		_, p, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		sha, err := f.git(rp, nil, nil, "rev-parse", rp.DefaultBranch+":"+p)
		if err != nil {
			return nil, err
		}
		hits = append(hits, codeHit{path: p, sha: strings.TrimSpace(sha)})
	}
	return hits, nil
}

// --- seeding ---

// seedFile is one file of a seed commit.
type seedFile struct{ path, content string }

// seedCommit is one commit a seed branch gets.
type seedCommit struct {
	branch  string // the branch it lands on
	from    string // the branch it starts from, "" for a first commit
	message string
	files   []seedFile
}

// seedRepo is one repo the fake starts with.
type seedRepo struct {
	id      int64
	name    string
	team    string
	commits []seedCommit
	issues  []issue
}

var seedRepos = []seedRepo{
	{
		id: 812345678, name: "pay-api", team: "payments",
		commits: []seedCommit{
			{branch: "main", message: "First commit", files: []seedFile{
				{"README.md", "# pay-api\n\nThe payments API of Strazahq. Demo data of fakegithub.\n"},
				{"src/login.js", "// Signs a user in.\nexport function login(user, password) {\n  // BUG: a reset password is stored trimmed, but compared as typed.\n  return user.passwordHash === hash(password);\n}\n\nfunction hash(s) {\n  return 'h:' + s;\n}\n"},
			}},
			{branch: "add-healthcheck", from: "main", message: "Add a health check", files: []seedFile{
				{"src/health.js", "// Answers the load balancer's health check.\nexport function health() {\n  return { status: 'ok' };\n}\n"},
			}},
		},
		issues: []issue{{Title: "Login fails after a password reset", Author: "maria-payments", State: "open",
			Body: "After a reset, a password with a trailing space never matches. See src/login.js."}},
	},
	{
		id: 812345679, name: "pay-web", team: "payments",
		commits: []seedCommit{{branch: "main", message: "First commit", files: []seedFile{
			{"README.md", "# pay-web\n\nThe payments web front end. Demo data of fakegithub.\n"},
			{"index.html", "<!doctype html>\n<title>Pay</title>\n"},
		}}},
	},
	{
		id: 812345690, name: "straza", team: "platform",
		commits: []seedCommit{{branch: "main", message: "First commit", files: []seedFile{
			{"README.md", "# straza\n\nThe platform team's repo. Demo data of fakegithub.\n"},
		}}},
	},
	{
		id: 812345700, name: "site", team: "web",
		commits: []seedCommit{{branch: "main", message: "First commit", files: []seedFile{
			{"README.md", "# site\n\nThe public web site. Demo data of fakegithub.\n"},
			{"index.html", "<!doctype html>\n<title>Strazahq</title>\n"},
		}}},
	},
}

// seedDate is the date of every seed commit, so seed commits are the same on
// every run.
const seedDate = "2026-10-01T09:00:00Z"

// seed makes each bare repo with its seed commits and branches, and the
// seed issues.
func (f *fake) seed() error {
	if err := os.MkdirAll(filepath.Join(f.gitRoot, ".home"), 0o700); err != nil {
		return err
	}
	indexDir, err := os.MkdirTemp(f.gitRoot, ".index")
	if err != nil {
		return err
	}
	defer os.RemoveAll(indexDir)
	created, _ := time.Parse(time.RFC3339, seedDate)

	for _, s := range seedRepos {
		f.mu.Lock()
		rp := f.repoByID(s.id)
		f.mu.Unlock()
		dir := f.bareDir(rp)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("%s exists already: give -git-root an empty directory", dir)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return err
		}
		if _, err := f.git(nil, nil, nil, "init", "-q", "--bare", "--initial-branch=main", dir); err != nil {
			return err
		}
		// git http-backend serves a push only with this on.
		if _, err := f.git(rp, nil, nil, "config", "http.receivepack", "true"); err != nil {
			return err
		}
		for i, c := range s.commits {
			index := filepath.Join(indexDir, fmt.Sprintf("%s-%d", s.name, i))
			env := []string{
				"GIT_INDEX_FILE=" + index,
				"GIT_AUTHOR_NAME=Strazahq demo data", "GIT_AUTHOR_EMAIL=demo@strazahq.invalid", "GIT_AUTHOR_DATE=" + seedDate,
				"GIT_COMMITTER_NAME=Strazahq demo data", "GIT_COMMITTER_EMAIL=demo@strazahq.invalid", "GIT_COMMITTER_DATE=" + seedDate,
			}
			var parent string
			if c.from != "" {
				if parent, err = f.revParse(rp, "refs/heads/"+c.from); err != nil {
					return fmt.Errorf("%s: no branch %s to start %s from", s.name, c.from, c.branch)
				}
				if _, err := f.git(rp, env, nil, "read-tree", parent); err != nil {
					return err
				}
			}
			for _, file := range c.files {
				blob, err := f.git(rp, env, []byte(file.content), "hash-object", "-w", "--stdin")
				if err != nil {
					return err
				}
				if _, err := f.git(rp, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+","+file.path); err != nil {
					return err
				}
			}
			tree, err := f.git(rp, env, nil, "write-tree")
			if err != nil {
				return err
			}
			args := []string{"commit-tree", strings.TrimSpace(tree), "-m", c.message}
			if parent != "" {
				args = append(args, "-p", parent)
			}
			commit, err := f.git(rp, env, nil, args...)
			if err != nil {
				return err
			}
			if _, err := f.git(rp, nil, nil, "update-ref", "refs/heads/"+c.branch, strings.TrimSpace(commit)); err != nil {
				return err
			}
		}
		f.mu.Lock()
		for _, is := range s.issues {
			is.ID, is.Number, is.Created, is.Updated = f.newID(), rp.nextNumber, created, created
			rp.nextNumber++
			rp.issues = append(rp.issues, &is)
		}
		f.mu.Unlock()
	}
	return nil
}
