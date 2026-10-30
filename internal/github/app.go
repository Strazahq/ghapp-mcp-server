package github

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// REST API constants sent on every request.
const (
	defaultBaseURL = "https://api.github.com"
	defaultTimeout = 30 * time.Second
	apiVersion     = "2022-11-28"
	mediaType      = "application/vnd.github+json"
	userAgent      = "ghapp-mcp-server"
)

// App JWT timing, as GitHub recommends: backdate iat a minute against clock
// drift, stay under GitHub's 10 minute limit, and sign a new one a minute
// before the cached one expires.
const (
	jwtBackdate    = 60 * time.Second
	jwtLifetime    = 9 * time.Minute
	jwtRenewBefore = time.Minute
)

// App is the GitHub App client. It implements API and is safe for concurrent
// use.
type App struct {
	appID    string
	signer   jose.Signer
	base     string            // the REST API base, without a trailing slash
	scrubber *strings.Replacer // removes base and its host from error texts
	http     *http.Client
	now      func() time.Time // the clock, replaced in tests

	mu     sync.Mutex
	jwt    string    // the cached App JWT, a credential like a token
	jwtExp time.Time // when jwt expires
}

var _ API = (*App)(nil)

// NewApp returns the App client for cfg. It parses the private key, so a key
// that cannot sign fails here and not on the first call. The HTTP client is
// cfg.HTTPClient, copied, or one with a 30 s timeout, and it never follows a
// redirect: a 3xx answer becomes ErrRedirect.
func NewApp(cfg Config) (*App, error) {
	appID := strings.TrimSpace(cfg.AppID)
	if appID == "" {
		return nil, errors.New("the GitHub App id is empty. Set it to the App's id, which the App's settings page shows")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, fmt.Errorf("the GitHub App's private key cannot sign an RS256 JWT: %w", err)
	}
	base, u, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	// Longest first: at each position the first match wins.
	scrubber := strings.NewReplacer(
		base, "the GitHub API",
		u.Host, "the GitHub API host",
		u.Hostname(), "the GitHub API host",
	)

	client := &http.Client{Timeout: defaultTimeout}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient // a copy, so the caller's client keeps its own CheckRedirect
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &App{
		appID:    appID,
		signer:   signer,
		base:     base,
		scrubber: scrubber,
		http:     client,
		now:      time.Now,
	}, nil
}

// parsePrivateKey reads the App's RSA private key from PEM, PKCS#1 ("RSA
// PRIVATE KEY", what GitHub generates) or PKCS#8 ("PRIVATE KEY"). Its errors
// say what is wrong with the key and never quote it.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		hint := ""
		if strings.Contains(string(pemBytes), `\n`) {
			hint = `. It holds a literal \n: if it came through an environment variable, its line breaks must be real line breaks`
		}
		return nil, fmt.Errorf("the GitHub App's private key is not PEM. Use the .pem file GitHub generated for the App, from -----BEGIN to -----END with its line breaks%s", hint)
	}
	if strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") || block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, errors.New("the GitHub App's private key is encrypted. Give the unencrypted .pem file GitHub generated, or decrypt it with openssl and keep the result as secret as the original")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the GitHub App's private key is not a valid PKCS#1 RSA key: %w. Generate a new key on the App's settings page", err)
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the GitHub App's private key is not a valid PKCS#8 key: %w. Generate a new key on the App's settings page", err)
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the GitHub App's private key is a %T, not an RSA key. GitHub App keys are RSA: use the .pem file GitHub generated", parsed)
		}
		return key, nil
	}
	return nil, fmt.Errorf("the GitHub App's private key PEM holds a %q block, not a private key. Use the .pem file GitHub generated for the App, which starts with -----BEGIN RSA PRIVATE KEY-----", block.Type)
}

// parseBaseURL checks the REST API base and returns it without a trailing
// slash, with its parsed form. Empty means https://api.github.com.
func parseBaseURL(raw string) (string, *url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", nil, errors.New("the GitHub API base URL is not usable. Give an absolute http or https URL without credentials, query or fragment, such as https://api.github.com or https://github.example.com/api/v3")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), u, nil
}

// appJWT returns the App's JWT: RS256, iss the App id, iat a minute ago, exp
// in 9 minutes. It is cached and reused until a minute before it expires.
func (a *App) appJWT() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.jwt != "" && now.Before(a.jwtExp.Add(-jwtRenewBefore)) {
		return a.jwt, nil
	}
	exp := now.Add(jwtLifetime)
	signed, err := jwt.Signed(a.signer).Claims(jwt.Claims{
		Issuer:   a.appID,
		IssuedAt: jwt.NewNumericDate(now.Add(-jwtBackdate)),
		Expiry:   jwt.NewNumericDate(exp),
	}).Serialize()
	if err != nil {
		return "", fmt.Errorf("signing the GitHub App JWT: %w", err)
	}
	a.jwt, a.jwtExp = signed, exp
	return signed, nil
}

// dropJWT forgets the cached JWT if it is still signed, after GitHub refused
// it, so the next call signs a new one.
func (a *App) dropJWT(signed string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt == signed {
		a.jwt, a.jwtExp = "", time.Time{}
	}
}
