package github

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testToken is a fake installation token. Tests check that it never appears
// in an error text, and never print it: failure messages go through safe.
const testToken = "ghs_fakeTokenValueForTests0123456789"

// testAppID is the App id the tests sign in with.
const testAppID = "12345"

// testKey is one RSA key for the whole test binary, generated on first use.
// No key file is ever committed.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// pkcs1PEM encodes key as GitHub generates App keys.
func pkcs1PEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// pkcs8PEM encodes any private key as PKCS#8.
func pkcs8PEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding a PKCS#8 test key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// jwtPattern finds a compact JWS in a text, to redact it.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// safe is err's text with the test token and any JWT redacted, for failure
// messages.
func safe(err error) string {
	if err == nil {
		return "<nil>"
	}
	return jwtPattern.ReplaceAllString(strings.ReplaceAll(err.Error(), testToken, "<token>"), "<jwt>")
}

// seen is one request the fake GitHub received.
type seen struct {
	Method string
	Path   string // escaped, as sent
	Query  string
	Auth   string
	Header http.Header
	Body   []byte
}

// fakeGitHub is a fake GitHub REST API on httptest. It records every request
// and serves the handlers a test registers; anything else is a 404.
type fakeGitHub struct {
	srv  *httptest.Server
	mux  *http.ServeMux
	mu   sync.Mutex
	reqs []seen
}

// newFake starts a fake GitHub, closed when the test ends.
func newFake(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{mux: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Query:  r.URL.RawQuery,
			Auth:   r.Header.Get("Authorization"),
			Header: r.Header.Clone(),
			Body:   body,
		})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// handle registers h for a ServeMux pattern such as "GET /repos/{o}/{r}".
func (f *fakeGitHub) handle(pattern string, h http.HandlerFunc) { f.mux.HandleFunc(pattern, h) }

// requests returns every request so far.
func (f *fakeGitHub) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

// hits counts the requests to an escaped path.
func (f *fakeGitHub) hits(path string) int {
	n := 0
	for _, r := range f.requests() {
		if r.Path == path {
			n++
		}
	}
	return n
}

// newTestApp returns an App signed in with the test key against f.
func newTestApp(t *testing.T, f *fakeGitHub) *App {
	t.Helper()
	app, err := NewApp(Config{
		AppID:         testAppID,
		PrivateKeyPEM: pkcs1PEM(testKey()),
		BaseURL:       f.srv.URL,
		HTTPClient:    f.srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app
}

// testTok is an installation token for the fake.
func testTok() Token {
	return Token{Value: testToken, ExpiresAt: time.Now().Add(time.Hour), InstallationID: 7}
}

// verifyJWT checks that an Authorization header carries an App JWT the test
// key signed, and returns its claims.
func verifyJWT(t *testing.T, auth string) jwt.Claims {
	t.Helper()
	signed, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization is not a bearer")
	}
	parsed, err := jwt.ParseSigned(signed, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("the App JWT does not parse: %v", err)
	}
	var claims jwt.Claims
	if err := parsed.Claims(&testKey().PublicKey, &claims); err != nil {
		t.Fatalf("the App JWT does not verify with the public key: %v", err)
	}
	return claims
}

// writeJSON answers v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
