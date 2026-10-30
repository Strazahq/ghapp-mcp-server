package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		header      map[string]string
		body        string
		notFound    bool
		wantStatus  int // for a *StatusError
		wantMessage string
		rateLimited bool
	}{
		{name: "404", status: 404, body: `{"message":"Not Found"}`, notFound: true},
		{name: "422", status: 422, body: `{"message":"Validation Failed","errors":[{"code":"invalid"}]}`, wantStatus: 422, wantMessage: "Validation Failed"},
		{name: "422 with reasons", status: 422, body: `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"not all refs are readable"},{"code":"custom","message":"second reason"}]}`, wantStatus: 422, wantMessage: "Validation Failed: not all refs are readable; second reason"},
		{name: "403 rate limit", status: 403, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1760000000"},
			body: `{"message":"API rate limit exceeded"}`, wantStatus: 403, wantMessage: "API rate limit exceeded", rateLimited: true},
		{name: "403 secondary rate limit", status: 403, header: map[string]string{"Retry-After": "60", "X-RateLimit-Remaining": "4000"},
			body: `{"message":"You have exceeded a secondary rate limit"}`, wantStatus: 403, wantMessage: "You have exceeded a secondary rate limit", rateLimited: true},
		{name: "403 forbidden", status: 403, header: map[string]string{"X-RateLimit-Remaining": "4999"},
			body: `{"message":"Resource not accessible by integration"}`, wantStatus: 403, wantMessage: "Resource not accessible by integration"},
		{name: "429", status: 429, body: `{"message":"Too Many Requests"}`, wantStatus: 429, wantMessage: "Too Many Requests", rateLimited: true},
		{name: "500", status: 500, body: `oops, not JSON`, wantStatus: 500},
		{name: "401", status: 401, body: `{"message":"Bad credentials"}`, wantStatus: 401, wantMessage: "Bad credentials"},
		{name: "409 with a multi-line message", status: 409, body: `{"message":"Merge conflict\nin\tfile"}`, wantStatus: 409, wantMessage: "Merge conflict in file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.handle("GET /repos/Strazahq/pay-api/pulls", func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			app := newTestApp(t, f)
			err := app.Do(t.Context(), testTok(), http.MethodGet, "/repos/Strazahq/pay-api/pulls", nil, nil, nil)
			if err == nil {
				t.Fatalf("status %d succeeded", tc.status)
			}
			if !strings.Contains(err.Error(), "GET /repos/Strazahq/pay-api/pulls") {
				t.Errorf("error %q does not name the method and path", safe(err))
			}
			if strings.Contains(err.Error(), f.srv.URL) || strings.Contains(err.Error(), testToken) {
				t.Errorf("error %q names the base URL or the token", safe(err))
			}
			if tc.notFound {
				if !errors.Is(err, ErrNotFound) {
					t.Errorf("error %q, want ErrNotFound", safe(err))
				}
				var se *StatusError
				if errors.As(err, &se) {
					t.Error("a 404 is also a *StatusError")
				}
				return
			}
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error %q is not a *StatusError", safe(err))
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRedirect) {
				t.Errorf("status %d matches a sentinel", tc.status)
			}
			if se.StatusCode != tc.wantStatus || se.Method != http.MethodGet || se.Path != "/repos/Strazahq/pay-api/pulls" {
				t.Errorf("StatusError %+v", se)
			}
			if !strings.HasPrefix(se.Status, fmt.Sprint(tc.wantStatus)) {
				t.Errorf("status line %q", se.Status)
			}
			if se.Message != tc.wantMessage {
				t.Errorf("message %q, want %q", se.Message, tc.wantMessage)
			}
			if se.RateLimited != tc.rateLimited {
				t.Errorf("RateLimited %v, want %v", se.RateLimited, tc.rateLimited)
			}
		})
	}
}

func TestRedirectRefused(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the client followed a redirect to another server: %s %s", r.Method, r.URL.Path)
	}))
	defer elsewhere.Close()

	cases := []struct {
		name   string
		status int
		target string // "" for the fake's own /repos/Strazahq/new-name
		call   func(*App) error
	}{
		{"Repository, renamed", http.StatusMovedPermanently, "", func(a *App) error {
			_, err := a.Repository(context.Background(), testTok(), "Strazahq/old-name")
			return err
		}},
		{"RepositoryByID, transferred", http.StatusTemporaryRedirect, "", func(a *App) error {
			_, err := a.RepositoryByID(context.Background(), testTok(), 99)
			return err
		}},
		{"InstallationForRepo", http.StatusMovedPermanently, "", func(a *App) error {
			_, err := a.InstallationForRepo(context.Background(), "Strazahq/old-name")
			return err
		}},
		{"Do POST, 308", http.StatusPermanentRedirect, "", func(a *App) error {
			return a.Do(context.Background(), testTok(), http.MethodPost, "/repos/Strazahq/old-name/issues", nil, map[string]string{"title": "x"}, nil)
		}},
		{"Do GET to another server", http.StatusFound, "elsewhere", func(a *App) error {
			return a.Do(context.Background(), testTok(), http.MethodGet, "/repos/Strazahq/old-name/pulls", nil, nil, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			target := f.srv.URL + "/repos/Strazahq/new-name"
			if tc.target == "elsewhere" {
				target = elsewhere.URL + "/repos/Strazahq/new-name"
			}
			redirect := func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, tc.status) }
			f.handle("/repos/Strazahq/old-name/", redirect)
			f.handle("/repos/Strazahq/old-name", redirect)
			f.handle("/repositories/99", redirect)
			f.handle("/repos/Strazahq/new-name", func(w http.ResponseWriter, r *http.Request) {
				t.Error("the client followed the redirect")
				writeJSON(w, http.StatusOK, map[string]any{"id": 99, "full_name": "Strazahq/new-name"})
			})

			// A caller's client that would follow redirects is overridden.
			app, err := NewApp(Config{AppID: testAppID, PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: f.srv.URL, HTTPClient: &http.Client{}})
			if err != nil {
				t.Fatal(err)
			}
			err = tc.call(app)
			if !errors.Is(err, ErrRedirect) {
				t.Fatalf("error %s, want ErrRedirect", safe(err))
			}
			if f.hits("/repos/Strazahq/new-name") != 0 {
				t.Error("the redirect target was hit")
			}
			if strings.Contains(err.Error(), f.srv.URL) || strings.Contains(err.Error(), "new-name") {
				t.Errorf("error %q names the base URL or the redirect target", safe(err))
			}
			if len(f.requests()) != 1 {
				t.Errorf("%d requests, want 1", len(f.requests()))
			}
		})
	}
}

func TestDo(t *testing.T) {
	f := newFake(t)
	f.handle("POST /repos/Strazahq/pay-api/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": 55, "body": "hello"})
	})
	f.handle("DELETE /repos/Strazahq/pay-api/git/refs/heads/x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	app := newTestApp(t, f)

	var out struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	query := url.Values{"note": {"a b&c"}}
	err := app.Do(t.Context(), testTok(), http.MethodPost, "/repos/Strazahq/pay-api/issues/7/comments", query, map[string]string{"body": "hello"}, &out)
	if err != nil {
		t.Fatalf("Do: %s", safe(err))
	}
	if out.ID != 55 || out.Body != "hello" {
		t.Errorf("out %+v", out)
	}
	req := f.requests()[0]
	if string(req.Body) != `{"body":"hello"}` || req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("body %s, Content-Type %q", req.Body, req.Header.Get("Content-Type"))
	}
	if req.Query != "note=a+b%26c" {
		t.Errorf("query %q", req.Query)
	}

	// A 204 with out set leaves out alone; no body sends no Content-Type.
	var none map[string]any
	if err := app.Do(t.Context(), testTok(), http.MethodDelete, "/repos/Strazahq/pay-api/git/refs/heads/x", nil, nil, &none); err != nil {
		t.Fatalf("Do DELETE: %s", safe(err))
	}
	if none != nil {
		t.Errorf("out %v after a 204", none)
	}
	if ct := f.requests()[1].Header.Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type %q without a body", ct)
	}
}

func TestRequestHeaders(t *testing.T) {
	f := newFake(t)
	f.handle("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 1, "full_name": "a/b"})
	})
	app := newTestApp(t, f)
	_, _ = app.InstallationForRepo(t.Context(), "a/b")
	_, _ = app.Repository(t.Context(), testTok(), "a/b")
	_ = app.Do(t.Context(), testTok(), http.MethodGet, "/x", nil, nil, nil)
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for _, r := range reqs {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("%s: Accept %q", r.Path, got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("%s: X-GitHub-Api-Version %q", r.Path, got)
		}
		if got := r.Header.Get("User-Agent"); got != "ghapp-mcp-server" {
			t.Errorf("%s: User-Agent %q", r.Path, got)
		}
	}
}

func TestBaseURLWithPath(t *testing.T) {
	f := newFake(t)
	f.handle("GET /api/v3/repos/Strazahq/pay-api", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 1, "full_name": "Strazahq/pay-api"})
	})
	app, err := NewApp(Config{AppID: testAppID, PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: f.srv.URL + "/api/v3/", HTTPClient: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Repository(t.Context(), testTok(), "Strazahq/pay-api"); err != nil {
		t.Fatalf("Repository under /api/v3: %s", safe(err))
	}
}

func TestResponseCap(t *testing.T) {
	f := newFake(t)
	f.handle("GET /exact", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(append(append([]byte("["), bytes.Repeat([]byte(" "), maxResponseBytes-2)...), ']'))
	})
	f.handle("GET /over", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(append(append([]byte("["), bytes.Repeat([]byte(" "), maxResponseBytes-1)...), ']'))
	})
	app := newTestApp(t, f)
	var out []int
	if err := app.Do(t.Context(), testTok(), http.MethodGet, "/exact", nil, nil, &out); err != nil {
		t.Errorf("a 10 MiB answer: %s", safe(err))
	}
	err := app.Do(t.Context(), testTok(), http.MethodGet, "/over", nil, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "larger than 10 MiB") {
		t.Errorf("an answer over 10 MiB: %s, want a refusal", safe(err))
	}
}

func TestDecodeError(t *testing.T) {
	f := newFake(t)
	f.handle("GET /x", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id": "not a number"}`)) })
	var out struct {
		ID int64 `json:"id"`
	}
	err := newTestApp(t, f).Do(t.Context(), testTok(), http.MethodGet, "/x", nil, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "GET /x: decoding GitHub's answer") {
		t.Errorf("a wrong answer: %s, want a decode error naming the call", safe(err))
	}
}

func TestTransportError(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	base := gone.URL
	host := strings.TrimPrefix(base, "http://")
	gone.Close()

	app, err := NewApp(Config{AppID: testAppID, PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	err = app.Do(t.Context(), testTok(), http.MethodGet, "/repos/a/b", nil, nil, nil)
	if err == nil {
		t.Fatal("a call to a closed server succeeded")
	}
	text := err.Error()
	if !strings.Contains(text, "GET /repos/a/b: GitHub did not answer") {
		t.Errorf("error %q does not say GitHub did not answer", safe(err))
	}
	if strings.Contains(text, base) || strings.Contains(text, host) || strings.Contains(text, "127.0.0.1") || strings.Contains(text, testToken) {
		t.Errorf("error %q names the address or the token", safe(err))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := newFake(t)
	err = newTestApp(t, f).Do(ctx, testTok(), http.MethodGet, "/x", nil, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call: %s, want context.Canceled", safe(err))
	}
}

// TestNoSecretInErrors runs every failure a call can meet and checks that no
// error text holds the installation token or the App JWT. It never prints
// the error text, which is what it guards.
func TestNoSecretInErrors(t *testing.T) {
	f := newFake(t)
	f.handle("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		var code int
		_, _ = fmt.Sscan(r.PathValue("code"), &code)
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Location", "https://evil.example/"+testToken)
		w.WriteHeader(code)
		// GitHub echoing the credential back must not reach the error text.
		_, _ = w.Write([]byte(`{"message":"` + testToken + ` ` + r.Header.Get("Authorization") + `"}`))
	})
	f.handle("/broken", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"x": "` + testToken + `"`))
	})
	f.handle("/app/installations/1/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token": "` + testToken + `", "expires_at": "` + testToken + `"}`))
	})
	f.handle("/app/installations/2/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."})
	})
	f.handle("/repos/{o}/{r}/installation", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": r.Header.Get("Authorization")})
	})
	app := newTestApp(t, f)
	tok := testTok()
	ctx := t.Context()

	type call struct {
		name string
		run  func() error
	}
	var calls []call
	for _, code := range []int{301, 302, 304, 400, 401, 403, 404, 409, 422, 429, 500, 502, 503} {
		path := fmt.Sprintf("/status/%d", code)
		calls = append(calls, call{path, func() error { return app.Do(ctx, tok, http.MethodGet, path, nil, nil, nil) }})
	}
	calls = append(calls,
		call{"broken JSON", func() error { var v any; return app.Do(ctx, tok, http.MethodGet, "/broken", nil, nil, &v) }},
		call{"mint with a broken expiry", func() error { _, err := app.MintToken(ctx, 1, nil, Permissions{"contents": "read"}); return err }},
		call{"mint refused", func() error { _, err := app.MintToken(ctx, 2, []int64{5}, Permissions{"contents": "read"}); return err }},
		call{"JWT echoed", func() error { _, err := app.InstallationForRepo(ctx, "a/b"); return err }},
		call{"bad path", func() error { return app.Do(ctx, tok, http.MethodGet, "/a/../"+testToken, nil, nil, nil) }},
		call{"bad method", func() error { return app.Do(ctx, tok, "TRACE", "/x", nil, nil, nil) }},
		call{"bad body", func() error { return app.Do(ctx, tok, http.MethodPost, "/x", nil, func() {}, nil) }},
		call{"bad token", func() error { return app.Do(ctx, Token{Value: testToken + "\n"}, http.MethodGet, "/x", nil, nil, nil) }},
		call{"repository 404", func() error { _, err := app.Repository(ctx, tok, "a/b"); return err }},
		call{"installation repos 404", func() error { _, _, err := app.InstallationRepos(ctx, tok, 1); return err }},
		call{"property values 404", func() error { _, _, err := app.PropertyValues(ctx, tok, "Strazahq", 1); return err }},
	)

	var jwts []string
	for _, c := range calls {
		err := c.run()
		if err == nil {
			t.Errorf("%s: succeeded, want an error", c.name)
			continue
		}
		for _, r := range f.requests() {
			if s, ok := strings.CutPrefix(r.Auth, "Bearer "); ok && s != testToken {
				jwts = append(jwts, s)
			}
		}
		text := fmt.Sprintf("%v %+v %s", err, err, err.Error())
		var se *StatusError
		if errors.As(err, &se) {
			text += " " + se.Message // Message can reach a caller too
		}
		if strings.Contains(text, testToken) {
			t.Errorf("%s: the error text holds the installation token", c.name)
		}
		for _, j := range jwts {
			if strings.Contains(text, j) {
				t.Errorf("%s: the error text holds the App JWT", c.name)
			}
		}
		if strings.Contains(text, f.srv.URL) {
			t.Errorf("%s: the error text holds the base URL", c.name)
		}
	}
	if len(jwts) == 0 {
		t.Error("no App JWT was sent, so the check proved nothing")
	}
}

func TestTokenRedacted(t *testing.T) {
	tok := testTok()
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("minted", "token", tok)
	js, err := json.Marshal(map[string]any{"token": tok})
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{
		"%v":   fmt.Sprintf("%v", tok),
		"%+v":  fmt.Sprintf("%+v", tok),
		"%#v":  fmt.Sprintf("%#v", tok),
		"%s":   fmt.Sprintf("%s", tok),
		"%q":   fmt.Sprintf("%q", tok),
		"&tok": fmt.Sprintf("%v", &tok),
		"slog": logged.String(),
		"json": string(js),
	}
	for name, text := range texts {
		if strings.Contains(text, testToken) {
			t.Errorf("%s shows the token value", name)
		}
	}
	if !strings.Contains(texts["%v"], "InstallationID: 7") {
		t.Errorf("%%v shows %q, want the installation id", texts["%v"])
	}
}
