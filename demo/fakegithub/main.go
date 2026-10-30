// Command fakegithub is a local stand-in for the parts of GitHub that
// ghapp-mcp-server uses, for the demo in demo/run.sh. It is not GitHub.
//
// It serves, on one loopback address:
//
//   - the App endpoints, which take only an App JWT signed RS256 with the demo
//     App's key, checked against its public key;
//   - installation tokens, whose repository_ids and permissions every later
//     call is held to, as GitHub holds them;
//   - the REST calls the 15 tools make: repos, branches, contents, pulls,
//     issues, comments, merge, search, and custom properties;
//   - git's smart HTTP, through `git http-backend`, on bare repos it seeds,
//     which takes Basic x-access-token:<token> for that token's repos only,
//     with contents write for a push.
//
// It writes an audit line, the way GitHub's audit log would, for each git
// push and each API write: actor ghapp[bot] and hashed_token, the base64 of
// the SHA-256 of the token.
package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var (
		listen    = flag.String("listen", "127.0.0.1:0", "the loopback address to serve on")
		appID     = flag.String("app-id", "424242", "the demo App's id, the iss of its JWT")
		pubKey    = flag.String("app-public-key", "", "the demo App's public key, PEM")
		gitRoot   = flag.String("git-root", "", "the directory that holds the bare repos, made and seeded at start")
		auditPath = flag.String("audit", "", "the file the audit lines are appended to, as JSON lines")
		readyFile = flag.String("ready-file", "", "a file to write the base URL to once serving")
		freePorts = flag.Int("free-ports", 0, "print this many free loopback ports and exit")
	)
	flag.Parse()
	log.SetPrefix("fakegithub (stand-in, not GitHub): ")
	log.SetFlags(log.Ltime)

	if *freePorts > 0 {
		ports, err := findFreePorts(*freePorts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(strings.Join(ports, " "))
		return
	}
	if err := run(*listen, *appID, *pubKey, *gitRoot, *auditPath, *readyFile); err != nil {
		log.Fatal(err)
	}
}

func run(listen, appID, pubKeyPath, gitRoot, auditPath, readyFile string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("-listen %q is not host:port: %w", listen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen %q: the fake serves loopback only", listen)
	}
	if pubKeyPath == "" || gitRoot == "" || auditPath == "" {
		return errors.New("give -app-public-key, -git-root and -audit")
	}
	pemBytes, err := os.ReadFile(pubKeyPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return fmt.Errorf("%s is not PEM", pubKeyPath)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("%s: %w", pubKeyPath, err)
	}
	audit, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer audit.Close()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		return errors.New("git is not installed")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		return fmt.Errorf("git --exec-path: %w", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		return errors.New("git http-backend is not installed")
	}

	f, err := newFake(appID, pub, gitRoot, gitBin, backend, audit)
	if err != nil {
		return err
	}
	if err := f.seed(); err != nil {
		return fmt.Errorf("seeding the repos: %w", err)
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	base := "http://" + ln.Addr().String()
	f.base = base
	log.Printf("serving the GitHub API and git at %s, repos in %s", base, gitRoot)
	if readyFile != "" {
		if err := os.WriteFile(readyFile, []byte(base+"\n"), 0o600); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// findFreePorts asks the kernel for n distinct free loopback ports. The
// listeners stay open until all are found, so the ports differ.
func findFreePorts(n int) ([]string, error) {
	var ports []string
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer ln.Close()
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		ports = append(ports, port)
	}
	return ports, nil
}
