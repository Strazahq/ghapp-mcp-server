package main

import "testing"

// ghapp's log mixes JSON lines from its audit logger with Go's text lines,
// and the client reads both the same way.
func TestParseLogLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		msg  string
		want map[string]string
	}{
		{
			`{"time":"2026-10-18T08:35:44Z","level":"INFO","msg":"token minted","caller":{"kind":"gateway"},"repo_ids":[812345678],"perms":{"contents":"write"},"token_hash":"5np7b/Bc6=="}`,
			"token minted",
			map[string]string{"caller.kind": "gateway", "repo_ids": "[812345678]", "perms": `{"contents":"write"}`, "token_hash": "5np7b/Bc6=="},
		},
		{
			`2026/10/18 08:35:44 INFO token minted caller.kind=gateway installation=55500001 repo_ids=[812345678] perms=map[contents:write] token_hash="5np7b/Bc6=="`,
			"token minted",
			map[string]string{"caller.kind": "gateway", "repo_ids": "[812345678]", "perms": "map[contents:write]", "token_hash": "5np7b/Bc6=="},
		},
		{
			`2026/10/18 08:35:44 INFO git op=push sub="" refs="refs/heads/x 00 11" reason="the grant was used"`,
			"git",
			map[string]string{"op": "push", "sub": "", "refs": "refs/heads/x 00 11", "reason": "the grant was used"},
		},
	} {
		e, ok := parseLogLine(tc.line)
		if !ok || e.msg != tc.msg {
			t.Errorf("parseLogLine(%s) = %q, %v", tc.line, e.msg, ok)
			continue
		}
		for k, v := range tc.want {
			if got, ok := e.attrs[k]; !ok || got != v {
				t.Errorf("%s: %s = %q, want %q", tc.msg, k, got, v)
			}
		}
	}
	if got := permsText(`{"pull_requests":"write","contents":"write"}`); got != "contents: write, pull_requests: write" {
		t.Errorf("permsText(JSON) = %q", got)
	}
	if got := permsText("map[contents:write pull_requests:write]"); got != "contents: write, pull_requests: write" {
		t.Errorf("permsText(Go) = %q", got)
	}
}
