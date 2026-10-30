package main

import (
	"context"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gateRequest is a server request as the SDK hands it to middleware, with
// the TokenInfo a verifier would put on it for c. A nil c means no token.
func gateRequest(c *Caller, tool string, withParams bool) *mcp.CallToolRequest {
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}
	if withParams {
		req.Params = &mcp.CallToolParamsRaw{Name: tool}
	}
	if c != nil {
		req.Extra.TokenInfo = &sdkauth.TokenInfo{
			UserID:     callerUserID(*c),
			Expiration: time.Now().Add(time.Minute),
			Extra:      map[string]any{callerExtraKey: *c},
		}
	}
	return req
}

func TestCallerGate(t *testing.T) {
	listing := &Caller{Kind: callerListing}
	gateway := &Caller{Kind: callerGateway}
	agent := &Caller{Kind: callerAgent, Client: "straza-agent-build", Subject: "sa"}

	tests := []struct {
		name       string
		caller     *Caller
		method     string
		tool       string
		withParams bool
		refused    bool
	}{
		{name: "listing on repo_list", caller: listing, method: "tools/call", tool: "repo_list", withParams: true},
		{name: "listing on pr_get", caller: listing, method: "tools/call", tool: "pr_get", withParams: true, refused: true},
		{name: "listing on a name that differs in case", caller: listing, method: "tools/call", tool: "Repo_List", withParams: true, refused: true},
		{name: "listing on tools/call without params", caller: listing, method: "tools/call", refused: true},
		{name: "listing on tools/list", caller: listing, method: "tools/list"},
		{name: "gateway on pr_get", caller: gateway, method: "tools/call", tool: "pr_get", withParams: true},
		{name: "agent on pr_get", caller: agent, method: "tools/call", tool: "pr_get", withParams: true},
		{name: "no token on pr_get", method: "tools/call", tool: "pr_get", withParams: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ran bool
			var seen Caller
			var seenOK bool
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				ran = true
				seen, seenOK = callerFromContext(ctx)
				return &mcp.CallToolResult{}, nil
			}
			res, err := callerGate(next)(context.Background(), tt.method, gateRequest(tt.caller, tt.tool, tt.withParams))
			if err != nil {
				t.Fatalf("gate: %v", err)
			}
			if tt.refused {
				if ran {
					t.Fatal("the handler ran")
				}
				r, ok := res.(*mcp.CallToolResult)
				if !ok || !r.IsError {
					t.Fatalf("result = %+v, want an error result", res)
				}
				if p, _ := r.Meta[errorMetaKey].(errorPayload); p.Code != CodeListingTokenOnly {
					t.Errorf("payload = %+v, want code %s", r.Meta[errorMetaKey], CodeListingTokenOnly)
				}
				return
			}
			if !ran {
				t.Fatal("the handler did not run")
			}
			if tt.caller == nil {
				if seenOK {
					t.Errorf("caller %+v in the context without a token", seen)
				}
				return
			}
			if !seenOK || seen != *tt.caller {
				t.Errorf("caller in the context = %+v (%v), want %+v", seen, seenOK, *tt.caller)
			}
		})
	}
}
