package main

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// routes is the REST API: the App endpoints, which take the App JWT, and
// every other, which takes an installation token.
func (f *fake) routes() http.Handler {
	mux := http.NewServeMux()
	// As the App, with its JWT.
	mux.HandleFunc("GET /app/installations", f.listInstallations)
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", f.repoInstallation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", f.mintToken)
	// With an installation token.
	mux.HandleFunc("GET /installation/repositories", f.installationRepos)
	mux.HandleFunc("GET /repositories/{id}", f.repositoryByID)
	mux.HandleFunc("GET /repos/{owner}/{repo}", f.repository)
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches", f.branches)
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents", f.contents)
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", f.contents)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", f.listPulls)
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", f.createPull)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", f.getPull)
	mux.HandleFunc("PUT /repos/{owner}/{repo}/pulls/{number}/merge", f.mergePull)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues", f.listIssues)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", f.createIssue)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", f.getIssue)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", f.createComment)
	mux.HandleFunc("GET /search/code", f.searchCode)
	mux.HandleFunc("GET /search/issues", f.searchIssues)
	mux.HandleFunc("GET /orgs/{org}/properties/schema", f.propertySchema)
	mux.HandleFunc("GET /orgs/{org}/properties/values", f.propertyValues)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "Not Found")
	})
	return mux
}

// --- paging ---

func paging(r *http.Request) (page, perPage int) {
	page, perPage = 1, 30
	q := r.URL.Query()
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		perPage = min(v, 100)
	}
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	return page, perPage
}

// pageOf cuts one page out of all and reports whether more follow.
func pageOf[T any](all []T, page, perPage int) ([]T, bool) {
	start := (page - 1) * perPage
	if start >= len(all) {
		return []T{}, false
	}
	end := min(start+perPage, len(all))
	return all[start:end], end < len(all)
}

// setNext sets GitHub's Link header for the next page.
func (f *fake) setNext(w http.ResponseWriter, r *http.Request, page, perPage int, more bool) {
	if !more {
		return
	}
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page+1))
	q.Set("per_page", strconv.Itoa(perPage))
	w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.base, r.URL.Path, q.Encode()))
}

// --- JSON shapes ---

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func userJSON(login string) map[string]any {
	kind := "User"
	if strings.HasSuffix(login, "[bot]") {
		kind = "Bot"
	}
	return map[string]any{"login": login, "type": kind}
}

func (f *fake) repoJSON(r *repo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"node_id":        base64.StdEncoding.EncodeToString([]byte("R_" + strconv.FormatInt(r.ID, 10))),
		"name":           r.Name,
		"full_name":      r.fullName(),
		"private":        true,
		"visibility":     "private",
		"archived":       false,
		"owner":          map[string]any{"login": r.Owner, "type": "Organization"},
		"html_url":       f.base + "/" + r.fullName(),
		"url":            f.base + "/repos/" + r.fullName(),
		"default_branch": r.DefaultBranch,
	}
}

func (f *fake) installationJSON(in installation) map[string]any {
	return map[string]any{
		"id":                   in.ID,
		"app_id":               f.appID,
		"account":              map[string]any{"login": in.Account, "type": in.AccountType},
		"target_type":          in.AccountType,
		"repository_selection": "all",
		"permissions":          appPerms,
	}
}

func (f *fake) issueURL(r *repo, is *issue) string {
	kind := "issues"
	if is.Pull {
		kind = "pull"
	}
	return fmt.Sprintf("%s/%s/%s/%d", f.base, r.fullName(), kind, is.Number)
}

func (f *fake) issueJSON(r *repo, is *issue) map[string]any {
	labels := []any{}
	for _, l := range is.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	out := map[string]any{
		"id":             is.ID,
		"number":         is.Number,
		"title":          is.Title,
		"body":           is.Body,
		"state":          is.State,
		"user":           userJSON(is.Author),
		"labels":         labels,
		"comments":       len(is.Comments),
		"html_url":       f.issueURL(r, is),
		"repository_url": f.base + "/repos/" + r.fullName(),
		"created_at":     timeOrNil(is.Created),
		"updated_at":     timeOrNil(is.Updated),
		"closed_at":      timeOrNil(is.Closed),
	}
	if is.Pull {
		out["pull_request"] = map[string]any{
			"html_url":  f.issueURL(r, is),
			"merged_at": timeOrNil(is.MergedAt),
		}
	}
	return out
}

// pullJSON is a pull request with its branches' commits read from git. The
// caller holds mu.
func (f *fake) pullJSON(r *repo, is *issue) map[string]any {
	headSHA, _ := f.revParse(r, "refs/heads/"+is.Head)
	baseSHA, _ := f.revParse(r, "refs/heads/"+is.Base)
	repoRef := map[string]any{"id": r.ID, "full_name": r.fullName(), "name": r.Name}
	var mergeable any
	if is.State == "open" {
		mergeable = f.canMerge(r, baseSHA, headSHA)
	}
	return map[string]any{
		"id":               is.ID,
		"number":           is.Number,
		"state":            is.State,
		"title":            is.Title,
		"body":             is.Body,
		"draft":            is.Draft,
		"merged":           is.Merged,
		"mergeable":        mergeable,
		"merge_commit_sha": is.MergeSHA,
		"user":             userJSON(is.Author),
		"head":             map[string]any{"ref": is.Head, "sha": headSHA, "label": r.Owner + ":" + is.Head, "repo": repoRef},
		"base":             map[string]any{"ref": is.Base, "sha": baseSHA, "label": r.Owner + ":" + is.Base, "repo": repoRef},
		"html_url":         f.issueURL(r, is),
		"created_at":       timeOrNil(is.Created),
		"updated_at":       timeOrNil(is.Updated),
		"closed_at":        timeOrNil(is.Closed),
		"merged_at":        timeOrNil(is.MergedAt),
	}
}

// --- as the App ---

func (f *fake) listInstallations(w http.ResponseWriter, r *http.Request) {
	if !f.checkAppJWT(w, r) {
		return
	}
	page, perPage := paging(r)
	f.mu.Lock()
	var all []map[string]any
	for _, in := range f.insts {
		all = append(all, f.installationJSON(in))
	}
	f.mu.Unlock()
	items, more := pageOf(all, page, perPage)
	f.setNext(w, r, page, perPage, more)
	writeJSON(w, http.StatusOK, items)
}

func (f *fake) repoInstallation(w http.ResponseWriter, r *http.Request) {
	if !f.checkAppJWT(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.repoByName(r.PathValue("owner"), r.PathValue("repo"))
	if rp == nil {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, in := range f.insts {
		if in.ID == rp.Installation {
			writeJSON(w, http.StatusOK, f.installationJSON(in))
			return
		}
	}
	apiError(w, http.StatusNotFound, "Not Found")
}

// mintToken makes an installation token with exactly the repository ids and
// permissions asked for, refusing what the App was not granted.
func (f *fake) mintToken(w http.ResponseWriter, r *http.Request) {
	if !f.checkAppJWT(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || !slices.ContainsFunc(f.insts, func(in installation) bool { return in.ID == id }) {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Repositories  []string          `json:"repositories"`
		Permissions   map[string]string `json:"permissions"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	perms := req.Permissions
	if len(perms) == 0 {
		// GitHub gives every permission of the installation.
		perms = maps.Clone(appPerms)
	}
	for name, level := range perms {
		granted, ok := appPerms[name]
		if !ok || levelRank(level) == 0 || levelRank(level) > levelRank(granted) {
			apiError(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
			return
		}
	}
	tok := &token{Installation: id, Perms: maps.Clone(perms), Expires: f.now().Add(time.Hour).Truncate(time.Second)}
	var repos []any
	ids := slices.Clone(req.RepositoryIDs)
	for _, name := range req.Repositories {
		rp := f.repoByName(orgLogin, name)
		if rp == nil {
			apiError(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
		ids = append(ids, rp.ID)
	}
	if len(ids) > 0 {
		tok.RepoIDs = []int64{}
		for _, rid := range ids {
			rp := f.repoByID(rid)
			if rp == nil || rp.Installation != id {
				apiError(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
				return
			}
			if !slices.Contains(tok.RepoIDs, rid) {
				tok.RepoIDs = append(tok.RepoIDs, rid)
				repos = append(repos, f.repoJSON(rp))
			}
		}
	}
	value := newTokenValue()
	f.tokens[value] = tok
	shown := maps.Clone(perms)
	shown["metadata"] = "read"
	ans := map[string]any{
		"token":                value,
		"expires_at":           tok.Expires.UTC().Format(time.RFC3339),
		"permissions":          shown,
		"repository_selection": "all",
	}
	reach := "every repo of the installation"
	if tok.RepoIDs != nil {
		ans["repository_selection"], ans["repositories"] = "selected", repos
		reach = fmt.Sprintf("repo ids %v", tok.RepoIDs)
	}
	log.Printf("minted an installation token for %s, permissions %v, hashed_token %s", reach, perms, hashedToken(value))
	writeJSON(w, http.StatusCreated, ans)
}

// --- with a token: repos ---

func (f *fake) installationRepos(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	page, perPage := paging(r)
	f.mu.Lock()
	var all []map[string]any
	for _, rp := range f.sortedRepos() {
		if tok.reaches(rp) {
			all = append(all, f.repoJSON(rp))
		}
	}
	f.mu.Unlock()
	items, more := pageOf(all, page, perPage)
	f.setNext(w, r, page, perPage, more)
	sel := "all"
	if tok.RepoIDs != nil {
		sel = "selected"
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "repository_selection": sel, "repositories": items})
}

// sortedRepos are the repos by full name. The caller holds mu.
func (f *fake) sortedRepos() []*repo {
	out := slices.Clone(f.repos)
	slices.SortFunc(out, func(a, b *repo) int { return cmp.Compare(a.fullName(), b.fullName()) })
	return out
}

func (f *fake) repositoryByID(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.repoByID(id)
	if rp == nil || !tok.reaches(rp) {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, f.repoJSON(rp))
}

func (f *fake) repository(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "metadata", "read")
	if rp == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, http.StatusOK, f.repoJSON(rp))
}

func (f *fake) branches(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "contents", "read")
	if rp == nil {
		return
	}
	refs, err := f.refs(rp)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "fakegithub could not read the branches")
		return
	}
	var all []map[string]any
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		branch, ok := strings.CutPrefix(name, "refs/heads/")
		if !ok {
			continue
		}
		all = append(all, map[string]any{"name": branch, "commit": map[string]any{"sha": refs[name]}, "protected": false})
	}
	page, perPage := paging(r)
	items, more := pageOf(all, page, perPage)
	f.setNext(w, r, page, perPage, more)
	writeJSON(w, http.StatusOK, items)
}

func (f *fake) contents(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "contents", "read")
	if rp == nil {
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = rp.DefaultBranch
	}
	p := strings.Trim(r.PathValue("path"), "/")
	entries, isDir, blob, err := f.readPath(rp, ref, p)
	switch {
	case errors.Is(err, errNoObject):
		apiError(w, http.StatusNotFound, "Not Found")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "fakegithub could not read the file")
		return
	}
	if isDir {
		var out []map[string]any
		for _, e := range entries {
			out = append(out, map[string]any{"type": e.kind, "name": path.Base(e.path), "path": e.path, "sha": e.sha, "size": e.size,
				"html_url": fmt.Sprintf("%s/%s/blob/%s/%s", f.base, rp.fullName(), ref, e.path)})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	enc := base64.StdEncoding.EncodeToString(blob.content)
	var lines []string
	for len(enc) > 60 {
		lines, enc = append(lines, enc[:60]), enc[60:]
	}
	lines = append(lines, enc)
	writeJSON(w, http.StatusOK, map[string]any{
		"type":     blob.kind,
		"encoding": "base64",
		"size":     len(blob.content),
		"name":     path.Base(p),
		"path":     p,
		"sha":      blob.sha,
		"content":  strings.Join(lines, "\n") + "\n",
		"html_url": fmt.Sprintf("%s/%s/blob/%s/%s", f.base, rp.fullName(), ref, p),
	})
}

// --- with a token: pull requests ---

// stateFilter reads ?state=, default open.
func stateFilter(r *http.Request) (string, bool) {
	switch s := r.URL.Query().Get("state"); s {
	case "":
		return "open", true
	case "open", "closed", "all":
		return s, true
	}
	return "", false
}

func (f *fake) listPulls(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "pull_requests", "read")
	if rp == nil {
		return
	}
	state, ok := stateFilter(r)
	if !ok {
		validationFailed(w, "PullRequest", "state", "invalid", "")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []map[string]any
	for _, is := range slices.Backward(rp.issues) {
		if is.Pull && (state == "all" || is.State == state) {
			all = append(all, f.pullJSON(rp, is))
		}
	}
	page, perPage := paging(r)
	items, more := pageOf(all, page, perPage)
	f.setNext(w, r, page, perPage, more)
	writeJSON(w, http.StatusOK, items)
}

// numbered finds issue or pull request number n of rp. The caller holds mu.
func numbered(rp *repo, n string) *issue {
	num, err := strconv.Atoi(n)
	if err != nil {
		return nil
	}
	for _, is := range rp.issues {
		if is.Number == num {
			return is
		}
	}
	return nil
}

func (f *fake) getPull(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "pull_requests", "read")
	if rp == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := numbered(rp, r.PathValue("number"))
	if is == nil || !is.Pull {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, f.pullJSON(rp, is))
}

func (f *fake) createPull(w http.ResponseWriter, r *http.Request) {
	tok, value := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "pull_requests", "write")
	if rp == nil {
		return
	}
	var req struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if owner, branch, ok := strings.Cut(req.Head, ":"); ok {
		if !strings.EqualFold(owner, rp.Owner) {
			validationFailed(w, "PullRequest", "head", "invalid", "")
			return
		}
		req.Head = branch
	}
	if strings.TrimSpace(req.Title) == "" {
		validationFailed(w, "PullRequest", "title", "missing_field", "")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	headSHA, err := f.revParse(rp, "refs/heads/"+req.Head)
	if err != nil {
		validationFailed(w, "PullRequest", "head", "invalid", "")
		return
	}
	baseSHA, err := f.revParse(rp, "refs/heads/"+req.Base)
	if err != nil {
		validationFailed(w, "PullRequest", "base", "invalid", "")
		return
	}
	for _, is := range rp.issues {
		if is.Pull && is.State == "open" && is.Head == req.Head && is.Base == req.Base {
			validationFailed(w, "PullRequest", "", "custom", "A pull request already exists for "+rp.Owner+":"+req.Head+".")
			return
		}
	}
	if f.isAncestor(rp, headSHA, baseSHA) {
		validationFailed(w, "PullRequest", "", "custom", "No commits between "+req.Base+" and "+req.Head)
		return
	}
	now := f.now()
	is := &issue{ID: f.newID(), Number: rp.nextNumber, Title: req.Title, Body: req.Body, State: "open", Author: botLogin,
		Created: now, Updated: now, Pull: true, Draft: req.Draft, Head: req.Head, Base: req.Base}
	rp.nextNumber++
	rp.issues = append(rp.issues, is)
	f.record(auditEvent{Action: "pull_request.create", Repo: rp.fullName(), RepositoryID: rp.ID, HashedToken: hashedToken(value),
		RequestMethod: r.Method, RequestPath: r.URL.Path,
		Details: map[string]any{"number": is.Number, "head": is.Head, "base": is.Base}})
	writeJSON(w, http.StatusCreated, f.pullJSON(rp, is))
}

func (f *fake) mergePull(w http.ResponseWriter, r *http.Request) {
	tok, value := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "pull_requests", "write")
	if rp == nil {
		return
	}
	if !tok.has("contents", "write") {
		apiError(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	var req struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	rp.pushMu.Lock()
	defer rp.pushMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := numbered(rp, r.PathValue("number"))
	if is == nil || !is.Pull {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	if is.State != "open" {
		apiError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	headSHA, err1 := f.revParse(rp, "refs/heads/"+is.Head)
	baseSHA, err2 := f.revParse(rp, "refs/heads/"+is.Base)
	if err1 != nil || err2 != nil {
		apiError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	if req.SHA != "" && !strings.EqualFold(req.SHA, headSHA) {
		apiError(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
		return
	}
	method := cmp.Or(req.MergeMethod, "merge")
	sha, err := f.merge(rp, is, method, baseSHA, headSHA)
	switch {
	case errors.Is(err, errConflict):
		apiError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	case errors.Is(err, errNotRebasable):
		apiError(w, http.StatusMethodNotAllowed, "This branch can't be rebased")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "fakegithub could not merge")
		return
	}
	now := f.now()
	is.State, is.Merged, is.MergedAt, is.Closed, is.Updated, is.MergeSHA = "closed", true, now, now, now, sha
	f.record(auditEvent{Action: "pull_request.merge", Repo: rp.fullName(), RepositoryID: rp.ID, HashedToken: hashedToken(value),
		RequestMethod: r.Method, RequestPath: r.URL.Path,
		Refs:    []refUpdate{{Ref: "refs/heads/" + is.Base, Before: baseSHA, After: sha}},
		Details: map[string]any{"number": is.Number, "merge_method": method}})
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

// --- with a token: issues ---

func (f *fake) listIssues(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "issues", "read")
	if rp == nil {
		return
	}
	state, ok := stateFilter(r)
	if !ok {
		validationFailed(w, "Issue", "state", "invalid", "")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []map[string]any
	for _, is := range slices.Backward(rp.issues) {
		if state == "all" || is.State == state {
			all = append(all, f.issueJSON(rp, is))
		}
	}
	page, perPage := paging(r)
	items, more := pageOf(all, page, perPage)
	f.setNext(w, r, page, perPage, more)
	writeJSON(w, http.StatusOK, items)
}

func (f *fake) getIssue(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "issues", "read")
	if rp == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := numbered(rp, r.PathValue("number"))
	if is == nil {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, f.issueJSON(rp, is))
}

func (f *fake) createIssue(w http.ResponseWriter, r *http.Request) {
	tok, value := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "issues", "write")
	if rp == nil {
		return
	}
	var req struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		validationFailed(w, "Issue", "title", "missing_field", "")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	is := &issue{ID: f.newID(), Number: rp.nextNumber, Title: req.Title, Body: req.Body, State: "open", Author: botLogin,
		Labels: req.Labels, Created: now, Updated: now}
	rp.nextNumber++
	rp.issues = append(rp.issues, is)
	f.record(auditEvent{Action: "issue.create", Repo: rp.fullName(), RepositoryID: rp.ID, HashedToken: hashedToken(value),
		RequestMethod: r.Method, RequestPath: r.URL.Path, Details: map[string]any{"number": is.Number}})
	writeJSON(w, http.StatusCreated, f.issueJSON(rp, is))
}

// createComment adds a comment to an issue's or a pull request's
// conversation. A pull request takes pull_requests write or issues write, a
// plain issue issues write only.
func (f *fake) createComment(w http.ResponseWriter, r *http.Request) {
	tok, value := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	rp := f.repoFor(w, r, tok, "metadata", "read")
	if rp == nil {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := numbered(rp, r.PathValue("number"))
	if is == nil {
		apiError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !tok.has("issues", "write") && !(is.Pull && tok.has("pull_requests", "write")) {
		apiError(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		validationFailed(w, "IssueComment", "body", "missing_field", "")
		return
	}
	now := f.now()
	c := comment{ID: f.newID(), Body: req.Body, Author: botLogin, Created: now}
	is.Comments = append(is.Comments, c)
	is.Updated = now
	f.record(auditEvent{Action: "issue_comment.create", Repo: rp.fullName(), RepositoryID: rp.ID, HashedToken: hashedToken(value),
		RequestMethod: r.Method, RequestPath: r.URL.Path, Details: map[string]any{"number": is.Number, "comment_id": c.ID}})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       userJSON(c.Author),
		"html_url":   fmt.Sprintf("%s#issuecomment-%d", f.issueURL(rp, is), c.ID),
		"created_at": timeOrNil(c.Created),
	})
}

// --- with a token: search ---

// searchQuery is a search's q: its words, and the repos its repo:
// qualifiers name, GitHub joining those with OR.
type searchQuery struct {
	terms     []string
	repos     []string
	kinds     map[string]bool // "issue", "pr"
	state     string
	otherQual bool
}

func parseSearch(q string) searchQuery {
	out := searchQuery{kinds: map[string]bool{}}
	for _, word := range strings.Fields(q) {
		key, val, ok := strings.Cut(word, ":")
		if !ok || val == "" {
			out.terms = append(out.terms, strings.ToLower(word))
			continue
		}
		switch strings.ToLower(key) {
		case "repo":
			out.repos = append(out.repos, val)
		case "is", "type":
			switch strings.ToLower(val) {
			case "issue":
				out.kinds["issue"] = true
			case "pr", "pull-request":
				out.kinds["pr"] = true
			case "open", "closed":
				out.state = strings.ToLower(val)
			}
		case "state":
			out.state = strings.ToLower(val)
		default:
			out.otherQual = true
		}
	}
	return out
}

// searchRepos are the repos a search covers for tok: those its repo:
// qualifiers name, or every repo tok reaches. ok is false when a named repo
// cannot be searched. The caller holds mu.
func (f *fake) searchRepos(tok *token, sq searchQuery) ([]*repo, bool) {
	if len(sq.repos) == 0 {
		var out []*repo
		for _, rp := range f.sortedRepos() {
			if tok.reaches(rp) {
				out = append(out, rp)
			}
		}
		return out, true
	}
	var out []*repo
	for _, name := range sq.repos {
		owner, n, _ := strings.Cut(name, "/")
		rp := f.repoByName(owner, n)
		if rp == nil || !tok.reaches(rp) {
			return nil, false
		}
		if !slices.Contains(out, rp) {
			out = append(out, rp)
		}
	}
	return out, true
}

func (f *fake) searchCode(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	sq := parseSearch(r.URL.Query().Get("q"))
	if len(sq.terms) == 0 {
		validationFailed(w, "Search", "q", "invalid", "Must include at least one search term.")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	repos, ok := f.searchRepos(tok, sq)
	if !ok {
		validationFailed(w, "Search", "q", "invalid", "The listed users and repositories cannot be searched either because the resources do not exist or you do not have permission to view them.")
		return
	}
	var all []map[string]any
	for _, rp := range repos {
		if !tok.has("contents", "read") {
			continue
		}
		hits, err := f.grep(rp, sq.terms)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "fakegithub could not search")
			return
		}
		for _, h := range hits {
			all = append(all, map[string]any{
				"name": path.Base(h.path), "path": h.path, "sha": h.sha, "score": 1,
				"html_url":   fmt.Sprintf("%s/%s/blob/%s/%s", f.base, rp.fullName(), rp.DefaultBranch, h.path),
				"repository": f.repoJSON(rp),
			})
		}
	}
	page, perPage := paging(r)
	items, _ := pageOf(all, page, perPage)
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "incomplete_results": false, "items": items})
}

func (f *fake) searchIssues(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil {
		return
	}
	sq := parseSearch(r.URL.Query().Get("q"))
	f.mu.Lock()
	defer f.mu.Unlock()
	repos, ok := f.searchRepos(tok, sq)
	if !ok {
		validationFailed(w, "Search", "q", "invalid", "The listed users and repositories cannot be searched either because the resources do not exist or you do not have permission to view them.")
		return
	}
	var all []map[string]any
	for _, rp := range repos {
		for _, is := range slices.Backward(rp.issues) {
			kind := "issue"
			perm := "issues"
			if is.Pull {
				kind, perm = "pr", "pull_requests"
			}
			if !tok.has(perm, "read") || (len(sq.kinds) > 0 && !sq.kinds[kind]) || (sq.state != "" && sq.state != is.State) {
				continue
			}
			text := strings.ToLower(is.Title + "\n" + is.Body)
			if !slices.ContainsFunc(sq.terms, func(t string) bool { return !strings.Contains(text, t) }) {
				item := f.issueJSON(rp, is)
				item["score"] = 1
				all = append(all, item)
			}
		}
	}
	page, perPage := paging(r)
	items, _ := pageOf(all, page, perPage)
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "incomplete_results": false, "items": items})
}

// --- with a token: custom properties ---

// orgFor checks that tok belongs to org's installation and reads custom
// properties. It answers 404 or 403 otherwise.
func (f *fake) orgFor(w http.ResponseWriter, r *http.Request, tok *token) bool {
	org := r.PathValue("org")
	f.mu.Lock()
	ok := slices.ContainsFunc(f.insts, func(in installation) bool {
		return in.ID == tok.Installation && strings.EqualFold(in.Account, org) && in.AccountType == "Organization"
	})
	f.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "Not Found")
		return false
	}
	if !tok.has("organization_custom_properties", "read") {
		apiError(w, http.StatusForbidden, "Resource not accessible by integration")
		return false
	}
	return true
}

func (f *fake) propertySchema(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil || !f.orgFor(w, r, tok) {
		return
	}
	writeJSON(w, http.StatusOK, []any{map[string]any{
		"property_name":      "team",
		"value_type":         "single_select",
		"required":           true,
		"description":        "The team that owns the repo",
		"allowed_values":     []string{"payments", "platform", "web"},
		"values_editable_by": "org_actors",
	}})
}

func (f *fake) propertyValues(w http.ResponseWriter, r *http.Request) {
	tok, _ := f.tokenFor(w, r)
	if tok == nil || !f.orgFor(w, r, tok) {
		return
	}
	f.mu.Lock()
	var all []map[string]any
	for _, rp := range f.sortedRepos() {
		if !tok.reaches(rp) {
			continue
		}
		all = append(all, map[string]any{
			"repository_id":        rp.ID,
			"repository_name":      rp.Name,
			"repository_full_name": rp.fullName(),
			"properties":           []any{map[string]any{"property_name": "team", "value": rp.Team}},
		})
	}
	f.mu.Unlock()
	page, perPage := paging(r)
	items, more := pageOf(all, page, perPage)
	f.setNext(w, r, page, perPage, more)
	writeJSON(w, http.StatusOK, items)
}
