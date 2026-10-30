package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// jwtSkew is the clock drift GitHub allows on iat.
const jwtSkew = 60 * time.Second

// jwtMaxLife is the longest an App JWT may live, as on GitHub.
const jwtMaxLife = 10 * time.Minute

// bearer is the credential of the Authorization header, "Bearer x" or
// GitHub's older "token x".
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	for _, scheme := range []string{"Bearer ", "bearer ", "token "} {
		if v, ok := strings.CutPrefix(h, scheme); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// checkAppJWT answers 401 and reports false unless the request carries an
// App JWT that the demo App's key signed with RS256, issued by the demo App,
// live now, and no longer than 10 minutes, as GitHub checks it.
func (f *fake) checkAppJWT(w http.ResponseWriter, r *http.Request) bool {
	raw := bearer(r)
	if strings.Count(raw, ".") != 2 {
		apiError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		apiError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	var c jwt.Claims
	if err := tok.Claims(f.pub, &c); err != nil {
		// The signature does not verify with the App's public key.
		apiError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return false
	}
	now := f.now()
	switch {
	case c.Issuer != f.appID:
		apiError(w, http.StatusUnauthorized, "'Issuer' claim ('iss') must be the App's id or client id")
	case c.IssuedAt == nil || c.IssuedAt.Time().After(now.Add(jwtSkew)):
		apiError(w, http.StatusUnauthorized, "'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued")
	case c.Expiry == nil || !now.Before(c.Expiry.Time()):
		apiError(w, http.StatusUnauthorized, "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires")
	case c.Expiry.Time().Sub(c.IssuedAt.Time()) > jwtMaxLife:
		apiError(w, http.StatusUnauthorized, "'Expiration time' claim ('exp') is too far in the future")
	default:
		return true
	}
	return false
}

// tokenFor answers 401 and returns nil unless the request carries a live
// installation token. It returns the token and its value.
func (f *fake) tokenFor(w http.ResponseWriter, r *http.Request) (*token, string) {
	value := bearer(r)
	f.mu.Lock()
	tok, ok := f.tokens[value]
	f.mu.Unlock()
	if value == "" || !ok || !f.now().Before(tok.Expires) {
		apiError(w, http.StatusUnauthorized, "Bad credentials")
		return nil, ""
	}
	return tok, value
}

// repoFor resolves the repo of a /repos/{owner}/{repo}/ request for tok and
// checks that tok holds perm at level on it. It answers 404 for a repo the
// token cannot see, as GitHub does, and 403 for a permission it lacks.
func (f *fake) repoFor(w http.ResponseWriter, r *http.Request, tok *token, perm, level string) *repo {
	f.mu.Lock()
	rp := f.repoByName(r.PathValue("owner"), r.PathValue("repo"))
	f.mu.Unlock()
	if rp == nil || !tok.reaches(rp) {
		apiError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if !tok.has(perm, level) {
		apiError(w, http.StatusForbidden, "Resource not accessible by integration")
		return nil
	}
	return rp
}

// apiError answers with GitHub's error body.
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"message":           msg,
		"documentation_url": "https://docs.github.com/rest",
		"status":            http.StatusText(status),
	})
}

// validationFailed answers 422 the way GitHub answers a bad field.
func validationFailed(w http.ResponseWriter, resource, field, code, msg string) {
	e := map[string]any{"resource": resource, "field": field, "code": code}
	if msg != "" {
		e["message"] = msg
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"message":           "Validation Failed",
		"errors":            []any{e},
		"documentation_url": "https://docs.github.com/rest",
		"status":            "422",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Media-Type", "github.v3; format=json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
