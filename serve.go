package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
	"github.com/strazahq/ghapp-mcp-server/internal/oidcauth"
)

// shutdownSignals are the signals that stop the server cleanly: Ctrl-C from a
// terminal, and SIGTERM, which Kubernetes, Docker and systemd send to stop it.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// discoveryTimeout bounds the read of the issuer's discovery document at
// startup, so an issuer that never answers does not hang the start.
const discoveryTimeout = 30 * time.Second

// serveStdio runs the server over stdio, with --dev only.
func serveStdio(gh github.API, cfg Config) error {
	if cfg.Git.On() {
		// No git endpoint without --http, so no git tools either.
		log.Printf("%s is set, but git transport needs --http, so the git tools stay off on stdio", envPublicURL)
		cfg.Git = GitConfig{}
	}
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	defer stop()

	log.Printf("%s %s serving on stdio for development, --dev (GitHub: %s; tools: %s)",
		serverName, version, cfg.GitHub.BaseURL, toolState(cfg))
	return newMCPServer(gh, cfg).Run(ctx, &mcp.StdioTransport{})
}

// serveHTTP runs the streamable HTTP transport at /mcp.
//
// It needs caller authentication, and then every request at /mcp needs a
// bearer that bearerVerifier accepts. /git has its own, the Straza grant.
// Without caller authentication it starts only with --dev, on loopback. There
// is no flag to bind a non-loopback address without caller authentication.
func serveHTTP(addr string, gh github.API, cfg Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	defer stop()

	if err := checkCallerSetup(cfg); err != nil {
		return err
	}
	if !cfg.CallerAuth() && !cfg.Dev {
		return fmt.Errorf("ghapp works only behind Straza, so --http needs %s or %s, for Straza's calls. For development, --dev serves a loopback address without caller authentication",
			envGatewaySecret, envOIDCIssuer)
	}
	bind, err := resolveBindAddr(addr, cfg.CallerAuth())
	if err != nil {
		return err
	}

	var authn *oidcauth.Authenticator
	if cfg.OIDCIssuer != "" {
		dctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
		authn, err = oidcauth.New(dctx, cfg.OIDCIssuer, cfg.OIDCDiscoveryURL, cfg.OIDCAudience, cfg.OIDCClientClaim, cfg.OIDCClients)
		cancel()
		if err != nil {
			return fmt.Errorf("setting up OIDC caller authentication from %s: %w", envOIDCIssuer, err)
		}
	}

	handler := mcpHTTPHandler(newMCPServer(gh, cfg), cfg, authn)
	if cfg.Git.On() {
		// The git endpoint has its own auth, the Straza grant, and no MCP
		// bearer: Straza's holder brings a grant and nothing else.
		git := newGitEndpoint(gh, cfg.Git)
		git.readOnly = cfg.ReadOnly
		handler = withGitEndpoint(handler, git)
		logGitState(cfg.Git)
	}
	httpServer := &http.Server{
		Addr:              bind,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down gracefully when signalled.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("%s %s serving on http://%s/mcp (GitHub: %s; tools: %s; callers: %s)",
		serverName, version, bind, cfg.GitHub.BaseURL, toolState(cfg), callerState(cfg))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as Shutdown starts. Waiting here lets the
	// requests in flight finish before the process exits.
	<-shutdownDone
	return nil
}

// checkCallerSetup refuses a listing token without the gateway secret or
// OIDC. The listing token is for Straza, which must also prove its tool
// calls. Starting would leave two choices, both wrong: ignore the token on a
// loopback bind, so that it opens every tool, or require it, so that no
// caller could reach any tool but repo_list.
func checkCallerSetup(cfg Config) error {
	if cfg.ListingToken != "" && !cfg.CallerAuth() {
		return fmt.Errorf("%s is set, but neither %s nor %s is, so no caller could call a tool other than repo_list. Set one of them for tool calls through Straza, or for development unset %s and use --dev to serve loopback without caller authentication",
			envListingToken, envGatewaySecret, envOIDCIssuer, envListingToken)
	}
	return nil
}

// callerAuthOn reports whether mcpHTTPHandler installs caller
// authentication. A listing token alone turns it on as well, so the token
// never opens more than repo_list, although serveHTTP refuses that setup.
func callerAuthOn(cfg Config, authn *oidcauth.Authenticator) bool {
	return cfg.CallerAuth() || cfg.ListingToken != "" || authn != nil
}

// mcpHTTPHandler mounts server's streamable HTTP handler at /mcp and /mcp/.
// With caller authentication on, every request must carry an accepted
// bearer, and callerGate runs before every method. authn is nil without
// OIDC.
func mcpHTTPHandler(server *mcp.Server, cfg Config, authn *oidcauth.Authenticator) http.Handler {
	authOn := callerAuthOn(cfg, authn)
	if authOn {
		// Added last, so it is the outermost middleware and refuses before
		// any other runs.
		server.AddReceivingMiddleware(callerGate)
	}

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			SessionTimeout: 5 * time.Minute,
			Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		},
	)

	var handler http.Handler = streamable
	if authOn {
		handler = sdkauth.RequireBearerToken(bearerVerifier(cfg, authn), nil)(streamable)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)
	return mux
}

// callerState describes caller authentication for the startup log. It
// names what is configured and never a secret.
func callerState(cfg Config) string {
	if !cfg.CallerAuth() {
		return "no caller authentication, loopback only, for development (--dev)"
	}
	var parts []string
	if cfg.OIDCIssuer != "" {
		parts = append(parts, fmt.Sprintf("OIDC bearer from %s for audience %s, %d client(s) by claim %s",
			cfg.OIDCIssuer, cfg.OIDCAudience, len(cfg.OIDCClients), cfg.OIDCClientClaim))
	}
	if cfg.GatewaySecret != "" {
		parts = append(parts, "gateway secret")
	}
	if cfg.ListingToken != "" {
		parts = append(parts, "listing token on repo_list only")
	}
	return strings.Join(parts, ", ")
}

// resolveBindAddr normalizes a --http address. A bare port or an empty host
// binds 127.0.0.1. A non-loopback host is refused unless allowNonLoopback,
// which serveHTTP sets only with caller authentication configured.
func resolveBindAddr(addr string, allowNonLoopback bool) (string, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return "", errors.New("the --http address is empty. Give a port, such as 8080, or host:port")
	}
	// Allow a bare port, such as "8080".
	if !strings.Contains(a, ":") {
		a = ":" + a
	}

	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return "", fmt.Errorf("the --http address %q is not host:port: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("the --http address %q has no port. Give one, such as 127.0.0.1:8080", addr)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !isLoopbackHost(host) && !allowNonLoopback {
		return "", fmt.Errorf("refusing to bind --http to %q, which is not loopback, without caller authentication. Set %s or %s, or bind 127.0.0.1, ::1 or localhost",
			host, envOIDCIssuer, envGatewaySecret)
	}
	return net.JoinHostPort(host, port), nil
}

// isLoopbackHost reports whether host is a loopback address or "localhost".
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
