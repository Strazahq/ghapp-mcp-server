package main

import "testing"

// A grant request names the URL git was given. The stand-in reads owner and
// name from it only under the demo's upstream, as Straza does with an app's
// straza.git.upstream.
func TestRepoOf(t *testing.T) {
	for _, tc := range []struct{ upstream, want string }{
		{"https://github.com/Strazahq/pay-api.git", "Strazahq/pay-api"},
		{"https://github.com/Strazahq/pay-api", "Strazahq/pay-api"},
		{"https://github.com/Strazahq/pay-api.git/info/refs", "Strazahq/pay-api"},
		{"https://github.com/Strazahq/.git", ""},
		{"https://github.com/Strazahq", ""},
		{"https://github.com//pay-api", ""},
		{"https://gitlab.com/Strazahq/pay-api.git", ""},
		{"https://github.company.example/Strazahq/pay-api.git", ""},
		{"", ""},
	} {
		if got := repoOf(tc.upstream); got != tc.want {
			t.Errorf("repoOf(%q) = %q, want %q", tc.upstream, got, tc.want)
		}
	}
}
