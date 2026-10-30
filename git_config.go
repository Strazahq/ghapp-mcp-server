package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults of git transport.
const (
	defaultGitUpstream     = "https://github.com"
	defaultGitMaxPushBytes = 2 << 30 // GitHub's limit for one push
	defaultGitTimeout      = time.Hour
)

// GitConfig is git transport through ghapp's git endpoint, which takes
// Straza grants. It is on when PublicURL is set, which needs GrantIssuer,
// and only over --http: serveStdio turns it off.
type GitConfig struct {
	// PublicURL is the URL agents reach ghapp at, in gitBaseURL's canonical
	// form.
	PublicURL string
	// Upstream is GitHub's git server, without a trailing slash.
	Upstream string
	// MaxPushBytes caps the request body of one push.
	MaxPushBytes int64
	// Timeout bounds one git request.
	Timeout time.Duration
	// GrantIssuer is Straza's public URL, exactly as its grants carry it in
	// iss.
	GrantIssuer string
	// GrantJWKSURL is where Straza's signing keys are read.
	GrantJWKSURL string
	// Notes are startup log lines about settings ghapp no longer reads.
	Notes []string
}

// On reports whether git transport is configured.
func (g GitConfig) On() bool { return g.PublicURL != "" }

// gitConfigFromEnv reads the git transport settings. Without
// GHAPP_PUBLIC_URL git transport is off, and any other git setting is
// refused, so a half setup does not pass for a working one. The endpoint
// takes Straza grants only, so GHAPP_PUBLIC_URL needs
// GHAPP_GIT_GRANT_ISSUER.
//
// Git tickets are retired. GHAPP_GIT_TICKETS=false, which turned them off,
// still starts and does nothing, so a deployment that set it keeps running.
// GHAPP_GIT_TICKETS=true asks for what is gone and is refused.
func gitConfigFromEnv() (GitConfig, error) {
	var notes []string
	switch tickets := strings.TrimSpace(os.Getenv(envGitTickets)); tickets {
	case "":
	case "false":
		notes = append(notes, envGitTickets+"=false is set but does nothing: ghapp has no git tickets, and git goes through Straza's holder with a grant. It can be removed")
	case "true":
		return GitConfig{}, fmt.Errorf("%s=true asks for git tickets, which ghapp no longer has: git goes through Straza's holder with a grant. Unset %s", envGitTickets, envGitTickets)
	default:
		return GitConfig{}, fmt.Errorf("%s is no longer read and can only be false or unset, got %q. ghapp has no git tickets", envGitTickets, tickets)
	}
	if os.Getenv(envGitTicketKey) != "" {
		notes = append(notes, envGitTicketKey+" is set but no longer read: ghapp has no git tickets. It can be removed")
	}

	public := strings.TrimSpace(os.Getenv(envPublicURL))
	upstream := strings.TrimSpace(os.Getenv(envGitUpstream))
	maxPush := strings.TrimSpace(os.Getenv(envGitMaxPushBytes))
	timeout := strings.TrimSpace(os.Getenv(envGitTimeout))
	issuer := strings.TrimSpace(os.Getenv(envGitGrantIssuer))
	jwks := strings.TrimSpace(os.Getenv(envGitGrantJWKSURL))
	if public == "" {
		for _, set := range []struct{ name, v string }{
			{envGitUpstream, upstream}, {envGitMaxPushBytes, maxPush}, {envGitTimeout, timeout},
			{envGitGrantIssuer, issuer}, {envGitGrantJWKSURL, jwks},
		} {
			if set.v != "" {
				return GitConfig{}, fmt.Errorf("%s is set but %s is not. Git transport is on only with %s, the URL agents reach ghapp at", set.name, envPublicURL, envPublicURL)
			}
		}
		return GitConfig{Notes: notes}, nil
	}
	if issuer == "" {
		return GitConfig{}, fmt.Errorf("%s is set but %s is not. The git endpoint takes Straza grants only: set %s to Straza's public URL, or unset %s to turn git off",
			envPublicURL, envGitGrantIssuer, envGitGrantIssuer, envPublicURL)
	}

	g := GitConfig{Upstream: defaultGitUpstream, MaxPushBytes: defaultGitMaxPushBytes, Timeout: defaultGitTimeout, Notes: notes}
	var err error
	if g.PublicURL, err = gitBaseURL(envPublicURL, public); err != nil {
		return GitConfig{}, err
	}
	if upstream != "" {
		if g.Upstream, err = gitBaseURL(envGitUpstream, upstream); err != nil {
			return GitConfig{}, err
		}
	}
	if maxPush != "" {
		n, err := strconv.ParseInt(maxPush, 10, 64)
		if err != nil || n <= 0 {
			return GitConfig{}, fmt.Errorf("%s must be a number of bytes above 0, such as 2147483648, got %q", envGitMaxPushBytes, maxPush)
		}
		g.MaxPushBytes = n
	}
	if timeout != "" {
		d, err := time.ParseDuration(timeout)
		if err != nil || d <= 0 {
			return GitConfig{}, fmt.Errorf("%s must be a duration above 0, such as 30m or 2h, got %q", envGitTimeout, timeout)
		}
		g.Timeout = d
	}
	// The issuer is kept as given, since a grant's iss must equal it.
	if _, err := gitBaseURL(envGitGrantIssuer, issuer); err != nil {
		return GitConfig{}, err
	}
	g.GrantIssuer = issuer
	g.GrantJWKSURL = strings.TrimRight(issuer, "/") + strazaJWKSPath
	if jwks != "" {
		u, err := url.Parse(jwks)
		if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" ||
			u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
			return GitConfig{}, fmt.Errorf("%s must be an https URL, or http on loopback, such as https://straza.example%s", envGitGrantJWKSURL, strazaJWKSPath)
		}
		g.GrantJWKSURL = jwks
	}
	return g, nil
}

// gitBaseURL checks a base URL of git transport: https, or http on a
// loopback host only, so a grant or a token never crosses a network in the
// clear. It returns the URL in the canonical form Straza's manifest parser
// stores straza.git.url in: scheme and host in lower case, no default port,
// and the path as written without a trailing slash. Straza puts that form
// in every grant's aud, which ghapp compares with GHAPP_PUBLIC_URL byte for
// byte.
func gitBaseURL(env, s string) (string, error) {
	// The URL is quoted only once it is known to carry no user or password.
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s is not a URL. Give a base URL such as https://ghapp.example", env)
	case u.User != nil, u.RawQuery != "", u.Fragment != "", u.Opaque != "":
		return "", fmt.Errorf("%s must be a plain base URL, with no user, query or fragment", env)
	case u.Scheme != "https" && u.Scheme != "http", u.Host == "":
		return "", fmt.Errorf("%s must be an https URL such as https://ghapp.example, got %q", env, s)
	case u.Scheme == "http" && !isLoopbackHost(u.Hostname()):
		return "", fmt.Errorf("%s must use https on a host other than loopback, got %q", env, s)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/"), nil
}
