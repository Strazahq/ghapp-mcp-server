package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The demo's roles: the role covers repos where team is payments, with its
// tools only, and sends the listed id.
func TestDecide(t *testing.T) {
	rf, err := readRoles(filepath.Join("..", "roles.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := &gate{cfg: config{agent: "joe-macos-agent", roles: rf}, objects: map[string]listed{
		"Strazahq/pay-api": {Value: "Strazahq/pay-api", ID: "812345678", Attributes: map[string]any{"team": "payments"}},
		"Strazahq/pay-web": {Value: "Strazahq/pay-web", ID: "812345679", Attributes: map[string]any{"team": []any{"web", "payments"}}},
		"Strazahq/straza":  {Value: "Strazahq/straza", ID: "812345690", Attributes: map[string]any{"team": "platform"}},
	}}
	for _, tc := range []struct {
		tool, repo string
		reachAll   bool
		want       decision
	}{
		{"pr_create", "Strazahq/pay-api", false, decision{allowed: true, role: "ghapp-write-payments", id: "812345678"}},
		{"pr_create", "Strazahq/pay-web", false, decision{allowed: true, role: "ghapp-write-payments", id: "812345679"}},
		{"pr_create", "Strazahq/straza", false, decision{}},
		{"pr_create", "strazahq/pay-api", false, decision{}}, // byte for byte
		{"pr_create", "Strazahq/unlisted", false, decision{}},
		{"pr_merge", "Strazahq/pay-api", false, decision{}}, // a tool the role does not hold
		{"pr_create", "Strazahq/pay-api", true, decision{}}, // reach all needs a role on every repo
	} {
		if got := g.decide(tc.tool, tc.repo, tc.reachAll); got != tc.want {
			t.Errorf("decide(%s, %s, %v) = %+v, want %+v", tc.tool, tc.repo, tc.reachAll, got, tc.want)
		}
	}
	if got := g.covered("pr_create"); len(got) != 2 || got[0] != "Strazahq/pay-api" || got[1] != "Strazahq/pay-web" {
		t.Errorf("covered = %v", got)
	}
	if g.decide("pr_create", "Strazahq/pay-api", false).allowed != true {
		t.Error("the role does not cover pay-api")
	}
	g.cfg.agent = "someone-else"
	if g.decide("pr_create", "Strazahq/pay-api", false).allowed {
		t.Error("an agent without roles was allowed")
	}
}

// A role takes exactly one form of covers.
func TestCoversTakesOneForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roles.json")
	for _, body := range []string{
		`{"roles":{"r":{"tools":["x"],"covers":{}}}}`,
		`{"roles":{"r":{"tools":["x"],"covers":{"every":true,"objects":["a/b"]}}}}`,
		`{"roles":{"r":{"tools":["x"],"covers":{"objects":["a/b"],"where":{"team":["x"]}}}}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readRoles(path); err == nil {
			t.Errorf("readRoles(%s) took it", body)
		}
	}
}
