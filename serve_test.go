package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveBindAddrWithoutCallerAuth(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty host binds loopback", ":3001", "127.0.0.1:3001", false},
		{"bare port binds loopback", "3001", "127.0.0.1:3001", false},
		{"loopback ipv4", "127.0.0.1:3001", "127.0.0.1:3001", false},
		{"another loopback ipv4", "127.0.0.2:3001", "127.0.0.2:3001", false},
		{"localhost", "localhost:3001", "localhost:3001", false},
		{"loopback ipv6", "[::1]:3001", "[::1]:3001", false},
		{"all interfaces ipv4 refused", "0.0.0.0:3001", "", true},
		{"all interfaces ipv6 refused", "[::]:3001", "", true},
		{"lan address refused", "192.168.1.10:3001", "", true},
		{"host name refused", "ghapp.internal:3001", "", true},
		{"empty refused", "", "", true},
		{"missing port refused", "127.0.0.1:", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBindAddr(tt.in, false)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveBindAddr(%q, false) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBindAddr(%q, false): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("resolveBindAddr(%q, false) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveBindAddrNamesTheFix(t *testing.T) {
	_, err := resolveBindAddr("0.0.0.0:3001", false)
	if err == nil {
		t.Fatal("a non-loopback bind without caller auth was allowed")
	}
	for _, want := range []string{envOIDCIssuer, envGatewaySecret, "127.0.0.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// With caller authentication any address binds, and an empty host still
// binds loopback: a public bind is always asked for by name.
func TestResolveBindAddrWithCallerAuth(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0:3001":     "0.0.0.0:3001",
		"[::]:3001":        "[::]:3001",
		"192.168.1.10:443": "192.168.1.10:443",
		":3001":            "127.0.0.1:3001",
		"3001":             "127.0.0.1:3001",
	} {
		got, err := resolveBindAddr(in, true)
		if err != nil {
			t.Fatalf("resolveBindAddr(%q, true): %v", in, err)
		}
		if got != want {
			t.Errorf("resolveBindAddr(%q, true) = %q, want %q", in, got, want)
		}
	}
}

// serveHTTP itself refuses before it listens or touches GitHub, so a nil
// API is never used. --dev opens loopback only, and nothing else gets past
// it.
func TestServeHTTPRefusals(t *testing.T) {
	tests := []struct {
		name string
		addr string
		cfg  Config
		want []string
	}{
		{
			name: "loopback without caller auth",
			addr: "127.0.0.1:0",
			want: []string{"only behind Straza", envGatewaySecret, envOIDCIssuer, "--dev"},
		},
		{
			name: "non-loopback without caller auth",
			addr: "0.0.0.0:0",
			want: []string{"only behind Straza", "--dev"},
		},
		{
			// --dev never opens a non-loopback address.
			name: "non-loopback without caller auth, --dev",
			addr: "0.0.0.0:0",
			cfg:  Config{Dev: true},
			want: []string{"not loopback", envGatewaySecret},
		},
		{
			name: "non-loopback with only the listing token",
			addr: "0.0.0.0:0",
			cfg:  Config{ListingToken: "listing-for-tests"},
			want: []string{envListingToken, envGatewaySecret, envOIDCIssuer},
		},
		{
			// Loopback with only the listing token: refused, so the token
			// never opens every tool and tools are never locked away.
			name: "loopback with only the listing token",
			addr: "127.0.0.1:0",
			cfg:  Config{ListingToken: "listing-for-tests"},
			want: []string{envListingToken, "unset", "--dev"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := serveHTTP(tt.addr, nil, tt.cfg)
			if err == nil {
				t.Fatal("serveHTTP started")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "listing-for-tests") {
				t.Errorf("error %q echoes the listing token", err)
			}
		})
	}
}

func TestCallerState(t *testing.T) {
	if got := callerState(Config{}); !strings.Contains(got, "loopback only") || !strings.Contains(got, "--dev") {
		t.Errorf("callerState without auth = %q", got)
	}
	cfg := Config{
		OIDCIssuer:      "https://idp.example/realms/straza",
		OIDCAudience:    "ghapp-mcp",
		OIDCClients:     []string{"a", "b"},
		OIDCClientClaim: "azp",
		GatewaySecret:   "gateway-secret-value",
		ListingToken:    "listing-token-value",
	}
	got := callerState(cfg)
	for _, want := range []string{cfg.OIDCIssuer, "ghapp-mcp", "2 client(s)", "azp", "gateway secret", "listing token on repo_list only"} {
		if !strings.Contains(got, want) {
			t.Errorf("callerState = %q, missing %q", got, want)
		}
	}
	for _, secret := range []string{cfg.GatewaySecret, cfg.ListingToken} {
		if strings.Contains(got, secret) {
			t.Errorf("callerState = %q holds a secret", got)
		}
	}
}

// Loopback without caller auth serves /mcp and /mcp/ with no token, and
// installs no caller gate.
func TestHTTPHandlerWithoutCallerAuth(t *testing.T) {
	rec := &authTestRecorder{}
	srv := httptest.NewServer(mcpHTTPHandler(newAuthTestServer(rec), Config{}, nil))
	defer srv.Close()

	for _, path := range []string{"/mcp", "/mcp/"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(authTestInitialize))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200: %s", path, resp.StatusCode, b)
		}
		if !strings.Contains(string(b), serverName) {
			t.Errorf("initialize at %s does not name %q: %s", path, serverName, b)
		}
	}

	e := &authTestEnv{rec: rec, url: srv.URL + "/mcp"}
	cs := e.mustConnect(t, "")
	if res := callAuthTestTool(t, cs, "pr_get", map[string]any{"repo": "Strazahq/pay-api"}); res.IsError {
		t.Fatalf("pr_get without caller auth: %s", authTestText(res))
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0].known {
		t.Errorf("calls = %+v, want one pr_get with no caller in the context", calls)
	}
}

func TestHTTPUnknownPath404(t *testing.T) {
	srv := httptest.NewServer(mcpHTTPHandler(newAuthTestServer(&authTestRecorder{}), Config{}, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope status = %d, want 404", resp.StatusCode)
	}
}

// stdio runs only with --dev: Straza reaches ghapp over --http.
func TestStdioNeedsDev(t *testing.T) {
	for _, name := range []string{envGatewaySecret, envOIDCIssuer, envListingToken, envPublicURL, envPrivateKeyFile} {
		t.Setenv(name, "")
	}
	t.Setenv(envAppID, "1")
	t.Setenv(envPrivateKey, "not read before the refusal")
	err := run("", false)
	if err != errStdioNeedsDev {
		t.Fatalf("run without --http or --dev = %v, want errStdioNeedsDev", err)
	}
	for _, want := range []string{"only behind Straza", "--http", "--dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
}
