package github

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestNewApp(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&testKey().PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	encrypted := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00"},
		Bytes:   []byte("not really"),
	})
	escaped := []byte(strings.ReplaceAll(string(pkcs1PEM(testKey())), "\n", `\n`))

	cases := []struct {
		name    string
		cfg     Config
		wantErr string // "" for success
	}{
		{"PKCS#1", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey())}, ""},
		{"PKCS#8", Config{AppID: "1", PrivateKeyPEM: pkcs8PEM(t, testKey())}, ""},
		{"client id as app id", Config{AppID: "Iv23liABC", PrivateKeyPEM: pkcs1PEM(testKey())}, ""},
		{"GHES base with path", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "https://ghe.example.com/api/v3/"}, ""},
		{"no app id", Config{AppID: "  ", PrivateKeyPEM: pkcs1PEM(testKey())}, "App id is empty"},
		{"no key", Config{AppID: "1"}, "not PEM"},
		{"not PEM", Config{AppID: "1", PrivateKeyPEM: []byte("hello")}, "not PEM"},
		{"escaped line breaks", Config{AppID: "1", PrivateKeyPEM: escaped}, `literal \n`},
		{"encrypted", Config{AppID: "1", PrivateKeyPEM: encrypted}, "is encrypted"},
		{"EC key", Config{AppID: "1", PrivateKeyPEM: pkcs8PEM(t, ecKey)}, "not an RSA key"},
		{"public key", Config{AppID: "1", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})}, `"PUBLIC KEY" block`},
		{"broken PKCS#1", Config{AppID: "1", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2, 3}})}, "not a valid PKCS#1"},
		{"broken PKCS#8", Config{AppID: "1", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}})}, "not a valid PKCS#8"},
		{"base without scheme", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "api.github.com"}, "base URL is not usable"},
		{"base with ftp", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "ftp://api.github.com"}, "base URL is not usable"},
		{"base with credentials", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "https://u:p@api.github.com"}, "base URL is not usable"},
		{"base with query", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "https://api.github.com/?x=1"}, "base URL is not usable"},
		{"base with fragment", Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "https://api.github.com/#x"}, "base URL is not usable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, err := NewApp(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("NewApp: %v", err)
				}
				if app == nil {
					t.Fatal("NewApp returned no App")
				}
				return
			}
			if err == nil {
				t.Fatalf("NewApp succeeded, want an error with %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NewApp error %q, want it to hold %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY-----\nMII") {
				t.Fatal("the error quotes the key")
			}
		})
	}
}

func TestNewAppDefaults(t *testing.T) {
	app, err := NewApp(Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey())})
	if err != nil {
		t.Fatal(err)
	}
	if app.base != "https://api.github.com" {
		t.Errorf("base %q, want https://api.github.com", app.base)
	}
	if app.http.Timeout != 30*time.Second {
		t.Errorf("timeout %v, want 30s", app.http.Timeout)
	}
	if app.http.CheckRedirect == nil || app.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("the default client follows redirects")
	}

	ghes, err := NewApp(Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), BaseURL: "https://ghe.example.com/api/v3/"})
	if err != nil {
		t.Fatal(err)
	}
	if ghes.base != "https://ghe.example.com/api/v3" {
		t.Errorf("base %q, want the trailing slash dropped", ghes.base)
	}

	// A caller's client keeps its own settings, and is not changed.
	own := &http.Client{Timeout: 5 * time.Second}
	app, err = NewApp(Config{AppID: "1", PrivateKeyPEM: pkcs1PEM(testKey()), HTTPClient: own})
	if err != nil {
		t.Fatal(err)
	}
	if app.http == own || own.CheckRedirect != nil {
		t.Error("NewApp changed the caller's client")
	}
	if app.http.Timeout != 5*time.Second {
		t.Errorf("timeout %v, want the caller's 5s", app.http.Timeout)
	}
	if app.http.CheckRedirect == nil || app.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("the caller's client follows redirects")
	}
}

func TestAppJWT(t *testing.T) {
	app, err := NewApp(Config{AppID: testAppID, PrivateKeyPEM: pkcs8PEM(t, testKey())})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }

	signed, err := app.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyJWT(t, "Bearer "+signed)
	if claims.Issuer != testAppID {
		t.Errorf("iss %q, want %q", claims.Issuer, testAppID)
	}
	if got, want := claims.IssuedAt.Time(), now.Add(-60*time.Second); !got.Equal(want) {
		t.Errorf("iat %v, want %v", got, want)
	}
	if got, want := claims.Expiry.Time(), now.Add(9*time.Minute); !got.Equal(want) {
		t.Errorf("exp %v, want %v", got, want)
	}

	parsed, err := jwt.ParseSigned(signed, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	if h := parsed.Headers[0]; h.Algorithm != "RS256" || h.ExtraHeaders[jose.HeaderType] != "JWT" {
		t.Errorf("header alg %q typ %v, want RS256 and JWT", h.Algorithm, h.ExtraHeaders[jose.HeaderType])
	}
	var raw map[string]any
	if err := parsed.UnsafeClaimsWithoutVerification(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 || raw["iss"] == nil || raw["iat"] == nil || raw["exp"] == nil {
		t.Errorf("claims %v, want exactly iss, iat and exp", raw)
	}

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.Claims(&other.PublicKey, &jwt.Claims{}); err == nil {
		t.Error("the App JWT verifies with another key")
	}

	// Cached until a minute before it expires, then signed anew.
	now = now.Add(7*time.Minute + 59*time.Second)
	again, err := app.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	if again != signed {
		t.Error("the JWT was signed anew before a minute before it expires")
	}
	now = now.Add(2 * time.Second)
	renewed, err := app.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	if renewed == signed {
		t.Error("the JWT was reused within a minute of expiring")
	}
	if got, want := verifyJWT(t, "Bearer "+renewed).Expiry.Time(), now.Add(9*time.Minute); !got.Equal(want) {
		t.Errorf("renewed exp %v, want %v", got, want)
	}
}

func TestAppJWTDroppedOn401(t *testing.T) {
	f := newFake(t)
	f.handle("GET /repos/{o}/{r}/installation", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "'Expiration time' claim ('exp') is too far in the future"})
	})
	app := newTestApp(t, f)
	if _, err := app.InstallationForRepo(t.Context(), "Strazahq/pay-api"); err == nil {
		t.Fatal("a 401 succeeded")
	}
	app.mu.Lock()
	cached := app.jwt
	app.mu.Unlock()
	if cached != "" {
		t.Error("the JWT GitHub refused is still cached")
	}
}
