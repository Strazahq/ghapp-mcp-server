package main

import (
	"context"
	"strings"
)

// GitHub shows every pull request, issue and comment ghapp writes as the
// App's bot, the same for every agent. So what an agent writes ends with a
// line that names it, unless GHAPP_MCP_AGENT_FOOTER is false. Without a known
// agent there is no line: behind the gateway secret, over stdio, or for the
// listing token, ghapp does not know which agent called.

// withAgentFooter returns body ending with a blank line and the footer, when
// the caller is an agent with a known client and the footer is on, else body
// unchanged. An empty body becomes the footer alone.
func (t *tools) withAgentFooter(ctx context.Context, body string) string {
	if !t.footer {
		return body
	}
	c, ok := callerFromContext(ctx)
	if !ok || c.Kind != callerAgent {
		return body
	}
	// The client id goes in a code span, so it cannot mention anyone or
	// link anywhere, and a backtick or line break cannot end the span.
	client := strings.ReplaceAll(strings.TrimSpace(plainText(c.Client)), "`", "")
	if client == "" {
		return body
	}
	line := "Sent by agent `" + client + "` through " + serverName + "."
	body = strings.TrimRight(body, "\r\n")
	if body == "" {
		return line
	}
	return body + "\n\n" + line
}
