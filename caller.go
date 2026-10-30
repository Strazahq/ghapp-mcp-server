package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Caller kinds: who called, as caller authentication found it.
const (
	// callerListing is Straza listing repositories with the listing token.
	// It opens repo_list and nothing else.
	callerListing = "listing"
	// callerGateway is a call with the static gateway secret. The secret
	// does not say which agent is behind the call.
	callerGateway = "gateway"
	// callerAgent is a call with an identity provider token issued to one
	// of the listed clients, each an agent's client at the provider.
	callerAgent = "agent"
	// callerGrant is a git request with a Straza grant: Subject is the
	// grant's sub, the user, and Client its act, the agent's session.
	callerGrant = "grant"
)

// listingTokenTool is the one tool the listing token opens.
const listingTokenTool = "repo_list"

// listingTokenRefusal is the sentence for the listing token on any other tool.
const listingTokenRefusal = "The listing token opens repo_list only. Call this tool through Straza."

// Caller is who called, as caller authentication established it. It holds
// no credential, so it is safe to log.
type Caller struct {
	// Kind is callerListing, callerGateway, callerAgent or callerGrant.
	Kind string
	// Client is the identity provider client id for callerAgent, and the
	// grant's act for callerGrant.
	Client string
	// Subject is the token's sub for callerAgent, and the grant's sub for
	// callerGrant.
	Subject string
}

type callerContextKey struct{}

// withCaller returns ctx carrying c.
func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, c)
}

// callerFromContext returns the caller of the request ctx belongs to. It
// reports false when caller authentication is off, as over stdio or on a
// loopback bind without it.
func callerFromContext(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerContextKey{}).(Caller)
	return c, ok
}

// callerFromRequest reads the caller the bearer verifier put on the request.
func callerFromRequest(req mcp.Request) (Caller, bool) {
	extra := req.GetExtra()
	if extra == nil || extra.TokenInfo == nil {
		return Caller{}, false
	}
	c, ok := extra.TokenInfo.Extra[callerExtraKey].(Caller)
	return c, ok
}

// callerGate is the receiving middleware installed with caller
// authentication. It puts the caller into the context of every method, and
// answers a tools/call by the listing token for any tool other than
// repo_list with the listing-token-only refusal, before any other
// middleware or the tool's handler runs. Other methods and other callers
// pass.
func callerGate(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		c, ok := callerFromRequest(req)
		if !ok {
			return next(ctx, method, req)
		}
		ctx = withCaller(ctx, c)
		if c.Kind == callerListing && method == "tools/call" && calledToolName(req) != listingTokenTool {
			return refusal(CodeListingTokenOnly, listingTokenRefusal), nil
		}
		return next(ctx, method, req)
	}
}

// calledToolName is the tool a tools/call names, or "" when it names none.
func calledToolName(req mcp.Request) string {
	call, ok := req.(*mcp.CallToolRequest)
	if !ok || call.Params == nil {
		return ""
	}
	return call.Params.Name
}
