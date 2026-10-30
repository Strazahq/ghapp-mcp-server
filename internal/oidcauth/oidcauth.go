// Package oidcauth verifies an identity provider's access tokens for one
// issuer and one audience, and says which listed client a token belongs to.
// The signature, issuer, audience and expiry checks are left to
// github.com/coreos/go-oidc rather than written by hand. The package knows
// nothing about MCP or Straza.
package oidcauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// DefaultClientClaim is the claim read for the client id when none is set.
// It is the authorized party, which Keycloak sets to the client that asked
// for the token.
const DefaultClientClaim = "azp"

// ErrNoClientClaim means a verified token does not say which client it was
// issued to.
var ErrNoClientClaim = errors.New("the token does not name its client")

// ErrClientNotListed means a verified token was issued to a client that is
// not one of the listed clients.
var ErrClientNotListed = errors.New("the token's client is not listed")

// Claims are the verified fields of a token.
type Claims struct {
	// Client is the value of the client claim, one of the listed clients.
	Client string
	// Subject is the token's sub. For client credentials it names the
	// client's service account.
	Subject string
	// Expiry is when the token expires.
	Expiry time.Time
}

// Authenticator verifies tokens for one issuer and audience, and accepts
// only tokens issued to a listed client.
type Authenticator struct {
	verifier    *oidc.IDTokenVerifier
	clientClaim string
	clients     map[string]bool
}

// New reads the issuer's discovery document, and with it the JWKS address,
// and returns an Authenticator that requires the audience and one of the
// clients in clientClaim. An empty clientClaim means DefaultClientClaim.
// discoveryURL, when set, is the full URL of the discovery document, read in
// place of the issuer's well-known path, for a server that reaches the
// identity provider at another address than its public one. Either way the
// document must name issuer exactly. It performs network I/O.
func New(ctx context.Context, issuer, discoveryURL, audience, clientClaim string, clients []string) (*Authenticator, error) {
	switch {
	case issuer == "":
		return nil, errors.New("no issuer is set")
	case audience == "":
		return nil, errors.New("no audience is set, so any token from the issuer would do")
	case len(clients) == 0:
		return nil, errors.New("no client is listed, so no token could be accepted")
	}
	provider, err := newProvider(ctx, issuer, discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("reading the discovery document of issuer %q: %w. Check that this server reaches the issuer, and that the issuer is spelled exactly as its tokens name it",
			issuer, err)
	}
	return newAuthenticator(provider.Verifier(&oidc.Config{ClientID: audience}), clientClaim, clients), nil
}

// maxDiscoveryBytes caps how much of a discovery document newProvider reads.
const maxDiscoveryBytes = 1 << 20

// supportedAlgorithms are the signing algorithms that oidc.NewProvider keeps
// from a discovery document.
var supportedAlgorithms = map[string]bool{
	oidc.RS256: true, oidc.RS384: true, oidc.RS512: true,
	oidc.ES256: true, oidc.ES384: true, oidc.ES512: true,
	oidc.PS256: true, oidc.PS384: true, oidc.PS512: true,
	oidc.EdDSA: true,
}

// newProvider reads the issuer's metadata from its well-known path, or from
// discoveryURL exactly as given when one is set. The document must name
// issuer byte for byte on both paths.
func newProvider(ctx context.Context, issuer, discoveryURL string) (*oidc.Provider, error) {
	if discoveryURL == "" {
		return oidc.NewProvider(ctx, issuer)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("the discovery URL %s cannot be requested: %w", discoveryURL, err)
	}
	// oidc.NewProvider uses the client that oidc.ClientContext put on ctx, so this does too.
	client := http.DefaultClient
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		client = c
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the discovery document at %s failed: %w", discoveryURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the discovery document at %s answered %s instead of 200 OK. The URL usually ends in /.well-known/openid-configuration", discoveryURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the discovery document at %s failed: %w", discoveryURL, err)
	}
	if len(body) > maxDiscoveryBytes {
		return nil, fmt.Errorf("the answer from %s is larger than 1 MiB, so it is not a discovery document", discoveryURL)
	}
	var cfg oidc.ProviderConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("the answer from %s is not a JSON discovery document: %v", discoveryURL, err)
	}
	if cfg.IssuerURL != issuer {
		return nil, fmt.Errorf("the discovery document at %s names issuer %q, not %q", discoveryURL, cfg.IssuerURL, issuer)
	}
	// ProviderConfig keeps every algorithm the document lists, and the
	// verifier accepts that list. Filtering it as oidc.NewProvider does keeps
	// the accepted set the same on both paths.
	var algs []string
	for _, a := range cfg.Algorithms {
		if supportedAlgorithms[a] {
			algs = append(algs, a)
		}
	}
	cfg.Algorithms = algs
	return cfg.NewProvider(ctx), nil
}

func newAuthenticator(v *oidc.IDTokenVerifier, clientClaim string, clients []string) *Authenticator {
	if clientClaim == "" {
		clientClaim = DefaultClientClaim
	}
	listed := make(map[string]bool, len(clients))
	for _, c := range clients {
		if c != "" {
			listed[c] = true
		}
	}
	return &Authenticator{verifier: v, clientClaim: clientClaim, clients: listed}
}

// Verify checks the token's signature against the issuer's JWKS, its
// issuer, its audience and its expiry, and then that its client claim names
// a listed client. A non-nil error means the token is not to be trusted.
// Errors never contain the token.
func (a *Authenticator) Verify(ctx context.Context, rawToken string) (Claims, error) {
	tok, err := a.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Claims{}, err
	}
	var all map[string]any
	if err := tok.Claims(&all); err != nil {
		return Claims{}, fmt.Errorf("decoding the token's claims: %w", err)
	}
	client, _ := all[a.clientClaim].(string)
	if client == "" {
		return Claims{}, fmt.Errorf("%w: claim %q is missing or not a non-empty string", ErrNoClientClaim, a.clientClaim)
	}
	if !a.clients[client] {
		return Claims{}, fmt.Errorf("%w: claim %q is %q", ErrClientNotListed, a.clientClaim, client)
	}
	return Claims{Client: client, Subject: tok.Subject, Expiry: tok.Expiry}, nil
}
