package main

import (
	"strings"
	"testing"
)

// setEnv clears every variable configFromEnv reads, then sets vars.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, name := range []string{
		envAppID, envPrivateKey, envPrivateKeyFile, envAPIURL, envReadOnly, envAttributes, envAgentFooter,
		envOIDCIssuer, envOIDCDiscoveryURL, envOIDCAudience, envOIDCClients, envOIDCClientClaim,
		envGatewaySecret, envListingToken,
	} {
		t.Setenv(name, vars[name])
	}
}

// base is the smallest valid environment. The key is not parsed here.
func base(extra map[string]string) map[string]string {
	v := map[string]string{envAppID: "12345", envPrivateKey: "-----BEGIN RSA PRIVATE KEY-----"}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

func TestConfigFromEnvRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no app id", map[string]string{envPrivateKey: "pem"}, envAppID + " is not set"},
		{"no key", map[string]string{envAppID: "1"}, "private key is not set"},
		{"two key sources", base(map[string]string{envPrivateKeyFile: "/k.pem"}), "not both"},
		{"issuer without audience", base(map[string]string{envOIDCIssuer: "https://idp", envOIDCClients: "a"}), "set both"},
		{"issuer without clients", base(map[string]string{envOIDCIssuer: "https://idp", envOIDCAudience: "ghapp"}), envOIDCClients + " is empty"},
		{"discovery URL without issuer", base(map[string]string{envOIDCDiscoveryURL: "https://idp/doc"}), envOIDCIssuer + " is not"},
		{"one secret for both", base(map[string]string{envGatewaySecret: "s", envListingToken: " s\n"}), "must be different secrets"},
		{"too many attributes", base(map[string]string{envAttributes: strings.Repeat("p,", maxAttributes) + "last"}), "at most 16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			_, err := configFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestConfigFromEnvDefaultsAndTrimming(t *testing.T) {
	setEnv(t, base(map[string]string{
		envGatewaySecret: "gateway-secret\n",
		envListingToken:  "  listing-token\n",
		envAttributes:    " team, ,env ",
		envAPIURL:        "https://github.example.com/api/v3/",
	}))
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.GatewaySecret != "gateway-secret" || cfg.ListingToken != "listing-token" {
		t.Errorf("secrets are not trimmed")
	}
	if strings.Join(cfg.Attributes, ",") != "team,env" {
		t.Errorf("attributes = %q", cfg.Attributes)
	}
	if cfg.GitHub.BaseURL != "https://github.example.com/api/v3" {
		t.Errorf("base URL = %q", cfg.GitHub.BaseURL)
	}
	if cfg.OIDCClientClaim != defaultOIDCClientClaim || cfg.ReadOnly {
		t.Errorf("defaults: claim %q, read-only %v", cfg.OIDCClientClaim, cfg.ReadOnly)
	}
	if !cfg.CallerAuth() {
		t.Errorf("a gateway secret should turn caller authentication on")
	}
}

// Only the exact value "true" turns read-only mode on. Anything else serves
// the write tools, as unset does.
func TestConfigFromEnvReadOnly(t *testing.T) {
	for value, want := range map[string]bool{
		"true":   true,
		"":       false,
		"false":  false,
		"TRUE":   false,
		"True":   false,
		"1":      false,
		"yes":    false,
		" true":  false,
		"true\n": false,
	} {
		t.Run(value, func(t *testing.T) {
			setEnv(t, base(map[string]string{envReadOnly: value}))
			cfg, err := configFromEnv()
			if err != nil {
				t.Fatalf("configFromEnv: %v", err)
			}
			if cfg.ReadOnly != want {
				t.Errorf("%s=%q: read-only %v, want %v", envReadOnly, value, cfg.ReadOnly, want)
			}
		})
	}
}

// The agent footer is on by default. Only the exact value "false" turns it
// off.
func TestConfigFromEnvAgentFooter(t *testing.T) {
	for value, off := range map[string]bool{
		"false":  true,
		"":       false,
		"true":   false,
		"FALSE":  false,
		"0":      false,
		"no":     false,
		"false ": false,
	} {
		t.Run(value, func(t *testing.T) {
			setEnv(t, base(map[string]string{envAgentFooter: value}))
			cfg, err := configFromEnv()
			if err != nil {
				t.Fatalf("configFromEnv: %v", err)
			}
			if cfg.NoAgentFooter != off {
				t.Errorf("%s=%q: footer off %v, want %v", envAgentFooter, value, cfg.NoAgentFooter, off)
			}
		})
	}
}
