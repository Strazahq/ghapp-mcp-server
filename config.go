package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// Environment variables. Credentials come only from the environment at run
// time, never from code, fixtures or logs.
const (
	envAppID          = "GHAPP_APP_ID"
	envPrivateKey     = "GHAPP_PRIVATE_KEY"      // the PEM itself
	envPrivateKeyFile = "GHAPP_PRIVATE_KEY_FILE" // or a path to it
	envAPIURL         = "GHAPP_API_URL"

	// envReadOnly set to exactly "true" leaves the write tools out, so the
	// server only reads. Any other value, or none, serves every tool.
	envReadOnly = "GHAPP_MCP_READ_ONLY"
	// envAttributes lists the custom properties repo_list reports as
	// attributes, comma separated, such as "team,env". Others stay out.
	envAttributes = "GHAPP_MCP_ATTRIBUTES"
	// envAgentFooter set to exactly "false" leaves out the line that ends
	// what an agent writes and names it. Any other value, or none, keeps it.
	envAgentFooter = "GHAPP_MCP_AGENT_FOOTER"

	envOIDCIssuer   = "GHAPP_MCP_OIDC_ISSUER"
	envOIDCAudience = "GHAPP_MCP_OIDC_AUDIENCE"
	// envOIDCDiscoveryURL is the full URL of the issuer's discovery document,
	// for a server that reaches the identity provider at another address
	// than its public one. Default: the issuer's well-known path.
	envOIDCDiscoveryURL = "GHAPP_MCP_OIDC_DISCOVERY_URL"
	// envOIDCClients lists the identity provider clients whose tokens may
	// call tools, comma separated. Each is registered with Straza's client
	// assertion key as its only credential.
	envOIDCClients = "GHAPP_MCP_OIDC_CLIENTS"
	// envOIDCClientClaim names the token claim that carries the client id.
	// Default "azp".
	envOIDCClientClaim = "GHAPP_MCP_OIDC_CLIENT_CLAIM"
	// envGatewaySecret is a static bearer only the gateway holds, for setups
	// without client credentials.
	envGatewaySecret = "GHAPP_MCP_GATEWAY_SECRET"
	// envListingToken is the static bearer that opens repo_list and nothing
	// else.
	envListingToken = "GHAPP_MCP_LISTING_TOKEN"
)

// Git transport through Straza grants, read by gitConfigFromEnv in
// git_config.go.
const (
	// envPublicURL is the URL agents reach ghapp at, such as
	// https://ghapp.example. It turns git transport on, and needs
	// envGitGrantIssuer.
	envPublicURL = "GHAPP_PUBLIC_URL"
	// envGitUpstream is GitHub's git server. Default https://github.com.
	envGitUpstream = "GHAPP_GIT_UPSTREAM"
	// envGitMaxPushBytes caps the body of one push. Default 2 GiB, GitHub's
	// own limit.
	envGitMaxPushBytes = "GHAPP_GIT_MAX_PUSH_BYTES"
	// envGitTimeout bounds one git request, as a Go duration. Default 1h.
	envGitTimeout = "GHAPP_GIT_TIMEOUT"
	// envGitGrantIssuer is Straza's public URL, the iss of its grants, which
	// are what opens the git endpoint.
	envGitGrantIssuer = "GHAPP_GIT_GRANT_ISSUER"
	// envGitGrantJWKSURL is where ghapp reads Straza's signing keys. Default
	// the issuer's /.well-known/straza/jwks.json.
	envGitGrantJWKSURL = "GHAPP_GIT_GRANT_JWKS_URL"
	// envGitTickets and envGitTicketKey set up git tickets, which are
	// retired. GHAPP_GIT_TICKETS=false and the key are noted at start and
	// do nothing, and GHAPP_GIT_TICKETS=true is refused.
	envGitTickets   = "GHAPP_GIT_TICKETS"
	envGitTicketKey = "GHAPP_GIT_TICKET_KEY"
)

// defaultOIDCClientClaim is the claim read for the client id when none is set.
const defaultOIDCClientClaim = "azp"

// Config is the server's configuration.
type Config struct {
	// GitHub is how the App signs in and which API it calls.
	GitHub github.Config

	// ReadOnly leaves the write tools out of tools/list. Off, every write
	// applies when called: behind Straza, a role holds a write tool or not.
	ReadOnly bool
	// Attributes are the custom property names repo_list reports.
	Attributes []string
	// NoAgentFooter leaves out the footer pr_create, pr_comment,
	// issue_create and issue_comment add for an agent caller. False, the
	// default, keeps it, so GitHub shows which agent wrote under the App's
	// bot.
	NoAgentFooter bool

	// OIDCIssuer, with OIDCAudience, turns on OIDC caller authentication:
	// tokens from this issuer for this audience.
	OIDCIssuer string
	// OIDCDiscoveryURL reads the issuer's discovery document from another
	// address. Empty: the issuer's own.
	OIDCDiscoveryURL string
	// OIDCAudience is the audience a caller's token must carry.
	OIDCAudience string
	// OIDCClients are the clients whose tokens may call tools.
	OIDCClients []string
	// OIDCClientClaim is the claim that names a token's client.
	OIDCClientClaim string
	// GatewaySecret is the static bearer only the gateway holds.
	GatewaySecret string
	// ListingToken is the static bearer that opens repo_list and nothing
	// else.
	ListingToken string

	// Git is git transport through ghapp's git endpoint, on with
	// GHAPP_PUBLIC_URL and over --http only.
	Git GitConfig

	// Dev is --dev: stdio, or --http on loopback without caller
	// authentication, for development and tests. ghapp works only behind
	// Straza, which never reaches it either way.
	Dev bool
}

// CallerAuth reports whether any caller authentication is configured, which
// --http needs unless Dev, and which lets it bind a non-loopback address.
func (c Config) CallerAuth() bool {
	return c.OIDCIssuer != "" || c.GatewaySecret != ""
}

// configFromEnv reads the configuration and says what is missing or wrong.
func configFromEnv() (Config, error) {
	cfg := Config{
		GitHub: github.Config{
			AppID:   strings.TrimSpace(os.Getenv(envAppID)),
			BaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envAPIURL)), "/"),
		},
		ReadOnly:         os.Getenv(envReadOnly) == "true",
		Attributes:       splitList(os.Getenv(envAttributes)),
		NoAgentFooter:    os.Getenv(envAgentFooter) == "false",
		OIDCIssuer:       strings.TrimSpace(os.Getenv(envOIDCIssuer)),
		OIDCDiscoveryURL: strings.TrimSpace(os.Getenv(envOIDCDiscoveryURL)),
		OIDCAudience:     strings.TrimSpace(os.Getenv(envOIDCAudience)),
		OIDCClients:      splitList(os.Getenv(envOIDCClients)),
		OIDCClientClaim:  strings.TrimSpace(os.Getenv(envOIDCClientClaim)),
		// Secrets are trimmed: a mounted secret file often ends in a newline,
		// and a bearer never holds whitespace, so an untrimmed secret would
		// never match.
		GatewaySecret: strings.TrimSpace(os.Getenv(envGatewaySecret)),
		ListingToken:  strings.TrimSpace(os.Getenv(envListingToken)),
	}
	if cfg.GitHub.BaseURL == "" {
		cfg.GitHub.BaseURL = "https://api.github.com"
	}
	if cfg.OIDCClientClaim == "" {
		cfg.OIDCClientClaim = defaultOIDCClientClaim
	}
	if cfg.GitHub.AppID == "" {
		return Config{}, fmt.Errorf("%s is not set. Set it to the GitHub App's id, which the App's settings page shows", envAppID)
	}
	key, err := privateKey()
	if err != nil {
		return Config{}, err
	}
	cfg.GitHub.PrivateKeyPEM = key
	if cfg.OIDCDiscoveryURL != "" && cfg.OIDCIssuer == "" {
		return Config{}, fmt.Errorf("%s is set but %s is not. The discovery URL is only read for an issuer", envOIDCDiscoveryURL, envOIDCIssuer)
	}
	if len(cfg.Attributes) > maxAttributes {
		return Config{}, fmt.Errorf("%s lists %d properties. Straza takes at most %d attributes per repo, so list %d or fewer", envAttributes, len(cfg.Attributes), maxAttributes, maxAttributes)
	}
	if (cfg.OIDCIssuer == "") != (cfg.OIDCAudience == "") {
		return Config{}, fmt.Errorf("set both %s and %s, or neither", envOIDCIssuer, envOIDCAudience)
	}
	if cfg.OIDCIssuer != "" && len(cfg.OIDCClients) == 0 {
		return Config{}, fmt.Errorf("%s is set but %s is empty. List the identity provider clients that may call tools, each registered with Straza's client assertion key only", envOIDCIssuer, envOIDCClients)
	}
	if cfg.ListingToken != "" && cfg.ListingToken == cfg.GatewaySecret {
		return Config{}, fmt.Errorf("%s and %s must be different secrets: the listing token opens repo_list only", envListingToken, envGatewaySecret)
	}
	if cfg.Git, err = gitConfigFromEnv(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// privateKey reads the App's private key from the environment or a file.
func privateKey() ([]byte, error) {
	pem := os.Getenv(envPrivateKey)
	file := strings.TrimSpace(os.Getenv(envPrivateKeyFile))
	switch {
	case pem != "" && file != "":
		return nil, fmt.Errorf("set %s or %s, not both", envPrivateKey, envPrivateKeyFile)
	case pem != "":
		return []byte(pem), nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", envPrivateKeyFile, err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("the App's private key is not set. Set %s to the PEM, or %s to its path", envPrivateKey, envPrivateKeyFile)
}

// splitList splits a comma-separated list and drops empty items.
func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
