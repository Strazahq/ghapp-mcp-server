// Command ghapp-mcp-server lets AI agents work with GitHub over the Model
// Context Protocol as a GitHub App. Every call gets a short-lived
// installation token for one repository and the permissions its tool needs.
// It works only behind Straza: it serves streamable HTTP with --http, and
// every call needs caller authentication, the gateway secret or OIDC. --dev
// serves stdio, or loopback without caller authentication, for development
// and tests.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

const serverName = "ghapp-mcp-server"

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.1-dev"

func main() {
	var (
		httpAddr    string
		dev         bool
		showVersion bool
	)
	flag.StringVar(&httpAddr, "http", "", "serve the streamable HTTP transport on this address. It needs "+envGatewaySecret+" or "+envOIDCIssuer+", so that only Straza calls ghapp. With --dev, a bare port or a loopback address works without them.")
	flag.BoolVar(&dev, "dev", false, "for development and tests only: serve stdio without --http, or --http on loopback without caller authentication. ghapp works only behind Straza, so never set it in production.")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("%s %s\n", serverName, version)
		return
	}

	if err := run(httpAddr, dev); err != nil {
		log.Fatalf("%s: %v", serverName, err)
	}
}

// run wires up the server and serves it over the selected transport.
func run(httpAddr string, dev bool) error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	cfg.Dev = dev
	for _, note := range cfg.Git.Notes {
		log.Print(note)
	}
	if httpAddr == "" && !cfg.Dev {
		return errStdioNeedsDev
	}
	gh, err := github.NewApp(cfg.GitHub)
	if err != nil {
		return err
	}

	// Protocol traffic owns stdout; diagnostics go to stderr.
	log.SetOutput(os.Stderr)

	if httpAddr == "" {
		return serveStdio(gh, cfg)
	}
	return serveHTTP(httpAddr, gh, cfg)
}

// errStdioNeedsDev refuses stdio outside development: Straza reaches ghapp
// over --http only.
var errStdioNeedsDev = errors.New("ghapp works only behind Straza, which reaches it over --http with " + envGatewaySecret + " or " + envOIDCIssuer +
	". Start it with --http. stdio is for development and tests: add --dev to use it")

// serverInstructions go to the client in the initialize result.
const serverInstructions = "Name a repository as owner/name, exactly as GitHub spells it, " +
	"for example Strazahq/pay-api. Each call works on that one repository."

// newMCPServer builds a server with every tool registered and a stable code
// on every tool error.
func newMCPServer(gh github.API, cfg Config) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version},
		&mcp.ServerOptions{Instructions: serverInstructions})
	registerTools(server, gh, cfg)
	server.AddReceivingMiddleware(errorCodes)
	return server
}

// toolState says for the startup log whether the write tools are served.
func toolState(cfg Config) string {
	if cfg.ReadOnly {
		return "read-only"
	}
	return "read and write"
}
