package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// gitEnvNames are every variable gitConfigFromEnv reads.
var gitEnvNames = []string{envPublicURL, envGitUpstream, envGitMaxPushBytes, envGitTimeout,
	envGitGrantIssuer, envGitGrantJWKSURL, envGitTickets, envGitTicketKey}

// setGitEnv sets the smallest valid environment and the git variables in
// vars, every other git variable empty.
func setGitEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	setEnv(t, base(nil))
	for _, name := range gitEnvNames {
		t.Setenv(name, vars[name])
	}
}

// withIssuer is vars with Straza's issuer, which GHAPP_PUBLIC_URL needs.
func withIssuer(vars map[string]string) map[string]string {
	out := map[string]string{envGitGrantIssuer: "https://straza.example"}
	for k, v := range vars {
		out[k] = v
	}
	return out
}

func TestGitConfigFromEnv(t *testing.T) {
	setGitEnv(t, nil)
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Git.On() {
		t.Errorf("git transport is on without %s: %+v", envPublicURL, cfg.Git)
	}

	setGitEnv(t, withIssuer(map[string]string{envPublicURL: "https://ghapp.example/"}))
	cfg, err = configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Git
	if !g.On() || g.PublicURL != "https://ghapp.example" || g.Upstream != "https://github.com" ||
		g.MaxPushBytes != 2<<30 || g.Timeout != time.Hour || g.GrantIssuer != "https://straza.example" ||
		g.GrantJWKSURL != "https://straza.example"+strazaJWKSPath || len(g.Notes) != 0 {
		t.Errorf("defaults = %+v", g)
	}

	setGitEnv(t, withIssuer(map[string]string{
		envPublicURL: "https://ghapp.example/base", envGitUpstream: "http://127.0.0.1:9418/",
		envGitMaxPushBytes: "1048576", envGitTimeout: "10m",
	}))
	cfg, err = configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	g = cfg.Git
	if g.PublicURL != "https://ghapp.example/base" || g.Upstream != "http://127.0.0.1:9418" || g.MaxPushBytes != 1<<20 ||
		g.Timeout != 10*time.Minute {
		t.Errorf("config = %+v", g)
	}
}

// Git tickets are gone. GHAPP_GIT_TICKETS=false and GHAPP_GIT_TICKET_KEY,
// which a deployment may still set, start with a note and do nothing, with
// git on or off. GHAPP_GIT_TICKETS=true asks for what is gone.
func TestGitConfigRetiredTickets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		on    bool
		notes []string
	}{
		{"tickets false", withIssuer(map[string]string{envPublicURL: "https://ghapp.example", envGitTickets: "false"}), true,
			[]string{envGitTickets + "=false is set but does nothing"}},
		{"tickets false, git off", map[string]string{envGitTickets: "false"}, false,
			[]string{envGitTickets + "=false is set but does nothing"}},
		{"a ticket key", withIssuer(map[string]string{envPublicURL: "https://ghapp.example", envGitTicketKey: "c2VjcmV0LWtleS12YWx1ZQ=="}), true,
			[]string{envGitTicketKey + " is set but no longer read"}},
		{"a ticket key, git off", map[string]string{envGitTicketKey: "c2VjcmV0LWtleS12YWx1ZQ=="}, false,
			[]string{envGitTicketKey + " is set but no longer read"}},
		{"both", withIssuer(map[string]string{envPublicURL: "https://ghapp.example", envGitTickets: "false", envGitTicketKey: "k"}), true,
			[]string{envGitTickets + "=false", envGitTicketKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setGitEnv(t, tc.env)
			cfg, err := configFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Git.On() != tc.on || len(cfg.Git.Notes) != len(tc.notes) {
				t.Fatalf("on %v, notes %q", cfg.Git.On(), cfg.Git.Notes)
			}
			for i, want := range tc.notes {
				if !strings.Contains(cfg.Git.Notes[i], want) {
					t.Errorf("note %q lacks %q", cfg.Git.Notes[i], want)
				}
			}
			if slices.ContainsFunc(cfg.Git.Notes, func(n string) bool { return strings.Contains(n, "c2VjcmV0") }) {
				t.Error("a note quotes the ticket key")
			}
		})
	}
}

func TestGitConfigRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"upstream without public URL", map[string]string{envGitUpstream: "https://github.com"}, envGitUpstream + " is set"},
		{"timeout without public URL", map[string]string{envGitTimeout: "1m"}, envGitTimeout + " is set"},
		{"issuer without public URL", map[string]string{envGitGrantIssuer: "https://straza.example"}, envGitGrantIssuer + " is set"},
		{"public URL without issuer", map[string]string{envPublicURL: "https://ghapp.example"}, "takes Straza grants only"},
		{"tickets true", withIssuer(map[string]string{envPublicURL: "https://ghapp.example", envGitTickets: "true"}), "asks for git tickets"},
		{"tickets true, git off", map[string]string{envGitTickets: "true"}, "asks for git tickets"},
		{"tickets maybe", map[string]string{envGitTickets: "off"}, "can only be false or unset"},
		{"plain http public URL", withIssuer(map[string]string{envPublicURL: "http://ghapp.example"}), "must use https"},
		{"public URL with a query", withIssuer(map[string]string{envPublicURL: "https://ghapp.example/?x=1"}), "plain base URL"},
		{"public URL with a password", withIssuer(map[string]string{envPublicURL: "https://u:secret-pass@ghapp.example"}), "plain base URL"},
		{"public URL not a URL", withIssuer(map[string]string{envPublicURL: "ghapp.example"}), "https URL"},
		{"plain http upstream", withIssuer(map[string]string{envPublicURL: "https://g.example", envGitUpstream: "http://github.com"}), "must use https"},
		{"max push zero", withIssuer(map[string]string{envPublicURL: "https://g.example", envGitMaxPushBytes: "0"}), envGitMaxPushBytes},
		{"max push words", withIssuer(map[string]string{envPublicURL: "https://g.example", envGitMaxPushBytes: "2GiB"}), envGitMaxPushBytes},
		{"timeout not a duration", withIssuer(map[string]string{envPublicURL: "https://g.example", envGitTimeout: "60"}), envGitTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setGitEnv(t, tc.env)
			_, err := configFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-pass") {
				t.Errorf("error %q quotes a secret", err)
			}
		})
	}
}
