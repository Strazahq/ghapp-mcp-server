package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/strazahq/ghapp-mcp-server/internal/oidcauth"
)

// callerExtraKey is the TokenInfo.Extra key that carries the Caller from the
// bearer verifier to callerGate.
const callerExtraKey = "ghapp-mcp-server/caller"

// staticTokenLifetime is the expiration given to a static bearer's
// TokenInfo. The SDK refuses a TokenInfo without a future expiration, and a
// static secret has none of its own, so each request gets a fresh one. It
// is checked right after verification and nowhere else.
const staticTokenLifetime = time.Minute

// staticSecret is a static bearer, kept as its SHA-256 sum. Comparing sums
// of equal length in constant time hides both the secret's bytes and its
// length from a caller who measures response times.
type staticSecret struct {
	sum [sha256.Size]byte
	set bool
}

func newStaticSecret(secret string) staticSecret {
	if secret == "" {
		return staticSecret{}
	}
	return staticSecret{sum: sha256.Sum256([]byte(secret)), set: true}
}

// matches reports in constant time whether token is the secret. An unset
// secret matches nothing.
func (s staticSecret) matches(token string) bool {
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], s.sum[:]) == 1 && s.set
}

// bearerVerifier accepts, in this order: the listing token, the gateway
// secret, and, when authn is set, an identity provider token for the
// configured issuer and audience whose client is listed. Anything else
// unwraps to sdkauth.ErrInvalidToken, which the SDK answers with HTTP 401
// before any method runs. The error text reaches the caller, so it says
// what to fix and never contains the token.
//
// TokenInfo.UserID binds an MCP session to the caller that opened it: the
// SDK refuses a request on that session from any other UserID.
func bearerVerifier(cfg Config, authn *oidcauth.Authenticator) sdkauth.TokenVerifier {
	listing := newStaticSecret(cfg.ListingToken)
	gateway := newStaticSecret(cfg.GatewaySecret)
	return func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		// Both comparisons always run, so the time taken does not tell
		// which secret a token came close to.
		isListing, isGateway := listing.matches(token), gateway.matches(token)
		switch {
		case isListing:
			return staticTokenInfo(Caller{Kind: callerListing}), nil
		case isGateway:
			return staticTokenInfo(Caller{Kind: callerGateway}), nil
		case authn == nil:
			return nil, fmt.Errorf("%w: the bearer token is neither %s nor %s. Check the credential the gateway sends",
				sdkauth.ErrInvalidToken, envGatewaySecret, envListingToken)
		case strings.Count(token, ".") != 2:
			// Not a JWT. Saying so here keeps a near miss of a static secret
			// out of the JWT parser's error text.
			return nil, fmt.Errorf("%w: the bearer token is neither %s, %s, nor a JWT. Check the credential the gateway sends",
				sdkauth.ErrInvalidToken, envGatewaySecret, envListingToken)
		}
		claims, err := authn.Verify(ctx, token)
		switch {
		case errors.Is(err, oidcauth.ErrNoClientClaim):
			return nil, fmt.Errorf("%w: %v. Add that claim at the identity provider, or set %s to the claim that carries the client id",
				sdkauth.ErrInvalidToken, err, envOIDCClientClaim)
		case errors.Is(err, oidcauth.ErrClientNotListed):
			return nil, fmt.Errorf("%w: %v. List a client in %s only when Straza's client assertion key is its one credential",
				sdkauth.ErrInvalidToken, err, envOIDCClients)
		case err != nil:
			return nil, fmt.Errorf("%w: the token is not valid for %s %q and %s %q: %v",
				sdkauth.ErrInvalidToken, envOIDCIssuer, cfg.OIDCIssuer, envOIDCAudience, cfg.OIDCAudience, err)
		}
		c := Caller{Kind: callerAgent, Client: claims.Client, Subject: claims.Subject}
		return &sdkauth.TokenInfo{
			UserID:     callerUserID(c),
			Expiration: claims.Expiry,
			Extra:      map[string]any{callerExtraKey: c},
		}, nil
	}
}

// staticTokenInfo is the TokenInfo for a static bearer.
func staticTokenInfo(c Caller) *sdkauth.TokenInfo {
	return &sdkauth.TokenInfo{
		UserID:     callerUserID(c),
		Expiration: time.Now().Add(staticTokenLifetime),
		Extra:      map[string]any{callerExtraKey: c},
	}
}

// callerUserID is the session owner for c: one per static secret, and one
// per client and subject for an agent. Quoting keeps a client id that holds
// a space from posing as another client and subject.
func callerUserID(c Caller) string {
	if c.Kind == callerAgent {
		return fmt.Sprintf("%s %q %q", c.Kind, c.Client, c.Subject)
	}
	return c.Kind
}
