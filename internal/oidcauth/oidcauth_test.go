package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testAudience = "ghapp-mcp"
	testClient   = "agent-build"
	testSubject  = "service-account-build"
)

// fakeIssuer serves a discovery document and the JWKS for its key on
// httptest, and signs tokens with go-jose. Its issuer is its own URL.
type fakeIssuer struct {
	*httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer
	// docIssuer is the issuer the discovery document names. Empty means the
	// fake's own URL.
	docIssuer string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := f.docIssuer
		if issuer == "" {
			issuer = f.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"jwks_uri":                              f.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "k1", Algorithm: "RS256", Use: "sig",
		}}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// claims are a valid client credentials token's claims, as Keycloak issues them.
func (f *fakeIssuer) claims() map[string]any {
	return map[string]any{
		"iss": f.URL,
		"aud": testAudience,
		"azp": testClient,
		"sub": testSubject,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
}

// mint signs claims with s, the fake's own signer unless another is given.
func mint(t *testing.T, s jose.Signer, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestNew(t *testing.T) {
	f := newFakeIssuer(t)
	ctx := context.Background()

	if _, err := New(ctx, f.URL, "", testAudience, "", []string{testClient}); err != nil {
		t.Fatalf("New against the fake issuer: %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	other := newFakeIssuer(t)
	other.docIssuer = "https://idp.example/realms/other"

	tests := []struct {
		name                     string
		issuer, audience, client string
		want                     string
	}{
		{name: "no issuer", audience: testAudience, client: testClient, want: "no issuer"},
		{name: "no audience", issuer: f.URL, client: testClient, want: "no audience"},
		{name: "no client", issuer: f.URL, audience: testAudience, want: "no client"},
		{name: "issuer unreachable", issuer: closed.URL, audience: testAudience, client: testClient, want: "reaches the issuer"},
		{name: "document names another issuer", issuer: other.URL, audience: testAudience, client: testClient, want: "did not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var clients []string
			if tt.client != "" {
				clients = []string{tt.client}
			}
			_, err := New(ctx, tt.issuer, "", tt.audience, "", clients)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// TestNewDiscoveryURL reads the document from a separate URL, and still
// requires it to name the configured issuer exactly.
func TestNewDiscoveryURL(t *testing.T) {
	f := newFakeIssuer(t)
	ctx := context.Background()
	doc := f.URL + "/.well-known/openid-configuration"

	if _, err := New(ctx, f.URL, doc, testAudience, "", []string{testClient}); err != nil {
		t.Fatalf("New with a discovery URL: %v", err)
	}
	if _, err := New(ctx, "https://idp.example/realms/other", doc, testAudience, "", []string{testClient}); err == nil || !strings.Contains(err.Error(), "names issuer") {
		t.Fatalf("New with a document for another issuer: error = %v, want one naming the issuer", err)
	}
	missing := f.URL + "/no-such-document"
	if _, err := New(ctx, f.URL, missing, testAudience, "", []string{testClient}); err == nil || !strings.Contains(err.Error(), "instead of 200 OK") {
		t.Fatalf("New with a missing document: error = %v, want a status error", err)
	}
}

func TestVerify(t *testing.T) {
	f := newFakeIssuer(t)
	stranger := newFakeIssuer(t)
	a, err := New(context.Background(), f.URL, "", testAudience, "", []string{testClient, "agent-deploy"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("a listed client's token", func(t *testing.T) {
		got, err := a.Verify(context.Background(), mint(t, f.signer, f.claims()))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.Client != testClient || got.Subject != testSubject || !got.Expiry.After(time.Now()) {
			t.Errorf("claims = %+v", got)
		}
	})

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		signer  jose.Signer
		wantErr error // nil means any error
		want    string
	}{
		{name: "wrong issuer", mutate: func(c map[string]any) { c["iss"] = "https://evil.example" }, want: "different provider"},
		{name: "wrong audience", mutate: func(c map[string]any) { c["aud"] = "another-service" }, want: "audience"},
		{name: "expired", mutate: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, want: "expired"},
		{name: "no expiry", mutate: func(c map[string]any) { delete(c, "exp") }, want: "expired"},
		{name: "signed by another key", signer: stranger.signer, want: "signature"},
		{name: "client not listed", mutate: func(c map[string]any) { c["azp"] = "agent-own-login" }, wantErr: ErrClientNotListed, want: `"agent-own-login"`},
		{name: "client claim missing", mutate: func(c map[string]any) { delete(c, "azp") }, wantErr: ErrNoClientClaim, want: `"azp"`},
		{name: "client claim empty", mutate: func(c map[string]any) { c["azp"] = "" }, wantErr: ErrNoClientClaim},
		{name: "client claim not a string", mutate: func(c map[string]any) { c["azp"] = []string{testClient} }, wantErr: ErrNoClientClaim},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := f.claims()
			if tt.mutate != nil {
				tt.mutate(c)
			}
			s := tt.signer
			if s == nil {
				s = f.signer
			}
			got, err := a.Verify(context.Background(), mint(t, s, c))
			if err == nil {
				t.Fatalf("Verify = %+v, want an error", got)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error %v is not %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}

	t.Run("not a JWT", func(t *testing.T) {
		if _, err := a.Verify(context.Background(), "not-a-token"); err == nil {
			t.Fatal("Verify accepted a string that is not a JWT")
		}
	})
}

func TestVerifyConfiguredClientClaim(t *testing.T) {
	f := newFakeIssuer(t)
	a, err := New(context.Background(), f.URL, "", testAudience, "client_id", []string{testClient})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := f.claims()
	c["azp"] = "someone-else"
	c["client_id"] = testClient
	got, err := a.Verify(context.Background(), mint(t, f.signer, c))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Client != testClient {
		t.Errorf("Client = %q, want the client_id claim %q", got.Client, testClient)
	}

	delete(c, "client_id")
	if _, err := a.Verify(context.Background(), mint(t, f.signer, c)); !errors.Is(err, ErrNoClientClaim) {
		t.Errorf("without client_id: error %v, want ErrNoClientClaim, not a fallback to azp", err)
	}
}
