package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// startFake serves a seeded fake and returns it with ghapp's own GitHub
// client, signed in with key.
func startFake(t *testing.T, key *rsa.PrivateKey) (*fake, *github.App, *bytes.Buffer) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git http-backend is not installed")
	}
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	audit := &bytes.Buffer{}
	f, err := newFake("424242", &appKey.PublicKey, t.TempDir(), gitBin, backend, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.seed(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.base = srv.URL
	if key == nil {
		key = appKey
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	app, err := github.NewApp(github.Config{AppID: "424242", PrivateKeyPEM: pemKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return f, app, audit
}

const payAPI int64 = 812345678

// The App endpoints take a JWT signed with the App's key only, and a token
// reaches its repo ids with its permissions only.
func TestTokensAreHeldToTheirScope(t *testing.T) {
	ctx := context.Background()
	_, app, audit := startFake(t, nil)

	insts, err := app.Installations(ctx)
	if err != nil || len(insts) != 1 || insts[0].Account != "Strazahq" {
		t.Fatalf("Installations = %+v, %v", insts, err)
	}
	inst, err := app.InstallationForRepo(ctx, "Strazahq/pay-api")
	if err != nil || inst != instID {
		t.Fatalf("InstallationForRepo = %d, %v", inst, err)
	}
	if _, err := app.InstallationForRepo(ctx, "Strazahq/nope"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("an unknown repo: %v, want not found", err)
	}

	tok, err := app.MintToken(ctx, inst, []int64{payAPI}, github.Permissions{"pull_requests": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := app.RepositoryByID(ctx, tok, payAPI); err != nil || r.FullName != "Strazahq/pay-api" {
		t.Errorf("RepositoryByID = %+v, %v", r, err)
	}
	// Another repo of the same installation is out of the token's reach.
	if _, err := app.RepositoryByID(ctx, tok, 812345690); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("another repo: %v, want not found", err)
	}
	var pull struct {
		Number int    `json:"number"`
		Body   string `json:"body"`
	}
	err = app.Do(ctx, tok, http.MethodPost, "/repos/Strazahq/pay-api/pulls", nil,
		map[string]any{"title": "t", "head": "add-healthcheck", "base": "main", "body": "b"}, &pull)
	if err != nil || pull.Number != 2 {
		t.Fatalf("create pull = %+v, %v", pull, err)
	}
	err = app.Do(ctx, tok, http.MethodPost, "/repos/Strazahq/straza/pulls", nil,
		map[string]any{"title": "t", "head": "main", "base": "main"}, nil)
	if !errors.Is(err, github.ErrNotFound) {
		t.Errorf("a pull on another repo: %v, want not found", err)
	}
	// pull_requests write does not open issues.
	var se *github.StatusError
	err = app.Do(ctx, tok, http.MethodPost, "/repos/Strazahq/pay-api/issues", nil, map[string]any{"title": "t"}, nil)
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden {
		t.Errorf("an issue with pull_requests write: %v, want 403", err)
	}
	// Nor the contents.
	err = app.Do(ctx, tok, http.MethodGet, "/repos/Strazahq/pay-api/contents/README.md", nil, nil, nil)
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden {
		t.Errorf("contents with pull_requests write: %v, want 403", err)
	}
	if !strings.Contains(audit.String(), `"action":"pull_request.create"`) || !strings.Contains(audit.String(), hashedToken(tok.Value)) {
		t.Errorf("the audit lacks the pull with the token's hash:\n%s", audit)
	}

	// More than the App was granted, or a repo outside the installation.
	if _, err := app.MintToken(ctx, inst, []int64{payAPI}, github.Permissions{"administration": "write"}); !errors.As(err, &se) || se.StatusCode != 422 {
		t.Errorf("a permission the App lacks: %v, want 422", err)
	}
	if _, err := app.MintToken(ctx, inst, []int64{999}, github.Permissions{"contents": "read"}); !errors.As(err, &se) || se.StatusCode != 422 {
		t.Errorf("a repo outside the installation: %v, want 422", err)
	}

	// The listing reads properties with a token for the whole installation.
	all, err := app.MintToken(ctx, inst, nil, github.Permissions{"metadata": "read", "organization_custom_properties": "read"})
	if err != nil {
		t.Fatal(err)
	}
	repos, more, err := app.InstallationRepos(ctx, all, 1)
	if err != nil || more || len(repos) != 4 {
		t.Errorf("InstallationRepos = %d repos, more %v, %v", len(repos), more, err)
	}
	vals, _, err := app.PropertyValues(ctx, all, "Strazahq", 1)
	if err != nil || len(vals) != 4 || vals[0].FullName != "Strazahq/pay-api" || vals[0].Values["team"][0] != "payments" {
		t.Errorf("PropertyValues = %+v, %v", vals, err)
	}
	if _, _, err := app.PropertyValues(ctx, tok, "Strazahq", 1); !errors.As(err, &se) || se.StatusCode != http.StatusForbidden {
		t.Errorf("properties with a pull_requests token: %v, want 403", err)
	}
}

// A JWT signed with another key opens nothing.
func TestJWTOfAnotherKey(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, app, _ := startFake(t, other)
	var se *github.StatusError
	if _, err := app.Installations(context.Background()); !errors.As(err, &se) || se.StatusCode != http.StatusUnauthorized {
		t.Errorf("Installations with another key: %v, want 401", err)
	}
}

// git over HTTP takes a token for the repo, and a push needs contents write.
func TestGitNeedsTheRightToken(t *testing.T) {
	ctx := context.Background()
	f, app, audit := startFake(t, nil)
	read, err := app.MintToken(ctx, instID, []int64{payAPI}, github.Permissions{"contents": "read"})
	if err != nil {
		t.Fatal(err)
	}
	write, err := app.MintToken(ctx, instID, []int64{payAPI}, github.Permissions{"contents": "write"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := app.MintToken(ctx, instID, []int64{812345690}, github.Permissions{"contents": "write"})
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.invalid", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.invalid")
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	url := func(tok github.Token) string {
		return strings.Replace(f.base, "http://", "http://x-access-token:"+tok.Value+"@", 1) + "/Strazahq/pay-api.git"
	}
	if out, err := git(tmp, "clone", "-q", url(read), "work"); err != nil {
		t.Fatalf("clone with contents read: %v\n%s", err, out)
	}
	if _, err := git(tmp, "clone", "-q", url(other), "nope"); err == nil {
		t.Error("a token for another repo cloned pay-api")
	}
	work := filepath.Join(tmp, "work")
	if out, err := git(work, "commit", "-q", "--allow-empty", "-m", "x"); err != nil {
		t.Fatal(out)
	}
	if out, err := git(work, "push", "-q", url(read), "HEAD:refs/heads/x"); err == nil || !strings.Contains(out, "Write access to repository not granted") {
		t.Errorf("a push with contents read: %v\n%s", err, out)
	}
	if out, err := git(work, "push", "-q", url(write), "HEAD:refs/heads/x"); err != nil {
		t.Fatalf("a push with contents write: %v\n%s", err, out)
	}
	if !strings.Contains(audit.String(), `"action":"git.push"`) || !strings.Contains(audit.String(), `"ref":"refs/heads/x"`) ||
		!strings.Contains(audit.String(), hashedToken(write.Value)) {
		t.Errorf("the audit lacks the push:\n%s", audit)
	}
}
