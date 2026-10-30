// Command fakestraza is a stand-in, not Straza, for the demo in demo/run.sh.
//
// It runs as `fakestraza mcp ghapp`, as `straza mcp ghapp` serves an agent's
// own Straza session over stdio. It does the part of Straza that ghapp's
// design leans on, and nothing else:
//
//   - It lists ghapp's repos with ghapp's repo_list and the listing token,
//     for their ids and their team.
//   - It offers the agent the ghapp tools its roles hold, from roles.json.
//   - On each call it checks that one of the agent's roles holds the tool and
//     covers the repo, and refuses otherwise in Straza's sentence, so ghapp
//     never sees the call.
//   - It forwards an allowed call to ghapp over HTTP with the gateway secret,
//     with the repo id from the listing in params._meta["ai.straza/object-id"].
//
// `fakestraza list ghapp` prints the listing and exits. `fakestraza grants
// ghapp` issues git grants to the demo's holder (grants.go).
//
// Settings, from the environment: FAKESTRAZA_GHAPP_URL (ghapp's /mcp),
// FAKESTRAZA_GATEWAY_SECRET, FAKESTRAZA_LISTING_TOKEN, FAKESTRAZA_ROLES (the
// roles file), FAKESTRAZA_AGENT (default joe-macos-agent) and
// FAKESTRAZA_AUDIT (a file for its audit lines, optional).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// standIn names this program wherever it speaks.
const standIn = "fakestraza (stand-in, not Straza)"

// metaObject and metaObjectID are Straza's object-scope keys.
const (
	metaObject   = "ai.straza/object"
	metaObjectID = "ai.straza/object-id"
)

const usage = `fakestraza is a stand-in, not Straza, for the ghapp-mcp-server demo.

Usage:
  fakestraza mcp ghapp      serve ghapp's tools over stdio, as straza mcp ghapp does
  fakestraza list ghapp     print ghapp's repo listing and exit
  fakestraza grants ghapp   issue git grants to the demo's holder over HTTP
`

func main() {
	log.SetFlags(0)
	log.SetPrefix(standIn + ": ")
	args := os.Args[1:]
	if len(args) != 2 || args[1] != "ghapp" || (args[0] != "mcp" && args[0] != "list" && args[0] != "grants") {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := configFromEnv()
	if err == nil {
		switch args[0] {
		case "list":
			err = printListing(ctx, cfg)
		case "grants":
			err = serveGrants(ctx, cfg)
		default:
			err = serve(ctx, cfg)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
}

type config struct {
	ghappURL      string
	gatewaySecret string
	listingToken  string
	agent         string
	roles         rolesFile
	auditPath     string
}

func configFromEnv() (config, error) {
	c := config{
		ghappURL:      os.Getenv("FAKESTRAZA_GHAPP_URL"),
		gatewaySecret: strings.TrimSpace(os.Getenv("FAKESTRAZA_GATEWAY_SECRET")),
		listingToken:  strings.TrimSpace(os.Getenv("FAKESTRAZA_LISTING_TOKEN")),
		agent:         os.Getenv("FAKESTRAZA_AGENT"),
		auditPath:     os.Getenv("FAKESTRAZA_AUDIT"),
	}
	if c.agent == "" {
		c.agent = "joe-macos-agent"
	}
	if c.ghappURL == "" || c.gatewaySecret == "" || c.listingToken == "" {
		return config{}, errors.New("set FAKESTRAZA_GHAPP_URL, FAKESTRAZA_GATEWAY_SECRET and FAKESTRAZA_LISTING_TOKEN")
	}
	path := os.Getenv("FAKESTRAZA_ROLES")
	if path == "" {
		return config{}, errors.New("set FAKESTRAZA_ROLES to the roles file, such as demo/roles.json")
	}
	var err error
	c.roles, err = readRoles(path)
	return c, err
}

// --- roles ---

// rolesFile is the stand-in's roles: which roles each agent holds, and what
// each role holds. Not Straza's own format.
type rolesFile struct {
	Server string              `json:"server"`
	Agents map[string][]string `json:"agents"`
	Roles  map[string]role     `json:"roles"`
}

// role binds tools to the repos its field covers names.
type role struct {
	Tools  []string `json:"tools"`
	Covers covers   `json:"covers"`
}

// covers is one of three forms: every repo, named repos, or repos whose
// listed attributes match.
type covers struct {
	Every   bool                `json:"every,omitempty"`
	Objects []string            `json:"objects,omitempty"`
	Where   map[string][]string `json:"where,omitempty"`
}

func (o covers) String() string {
	switch {
	case o.Every:
		return "every repo"
	case len(o.Objects) > 0:
		return "objects " + strings.Join(o.Objects, ", ")
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(o.Where)) {
		parts = append(parts, k+" is "+strings.Join(o.Where[k], " or "))
	}
	return "repos where " + strings.Join(parts, " and ")
}

func readRoles(path string) (rolesFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return rolesFile{}, err
	}
	var rf rolesFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if err := dec.Decode(&rf); err != nil {
		return rolesFile{}, fmt.Errorf("%s: %w", path, err)
	}
	for name, r := range rf.Roles {
		forms := 0
		if r.Covers.Every {
			forms++
		}
		if len(r.Covers.Objects) > 0 {
			forms++
		}
		if len(r.Covers.Where) > 0 {
			forms++
		}
		if forms != 1 {
			return rolesFile{}, fmt.Errorf("%s: role %s: covers takes exactly one of every, objects and where", path, name)
		}
	}
	return rf, nil
}

// --- talking to ghapp ---

// bearer adds the stand-in's credential to every request to ghapp.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// connect opens an MCP session to ghapp with token as the bearer.
func connect(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fakestraza", Title: standIn, Version: "demo"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
}

// listed is one repo of ghapp's repo_list.
type listed struct {
	Value      string         `json:"value"`
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Attributes map[string]any `json:"attributes"`
}

// listing reads every page of repo_list with the listing token.
func listing(ctx context.Context, cfg config) ([]listed, int, error) {
	cs, err := connect(ctx, cfg.ghappURL, cfg.listingToken)
	if err != nil {
		return nil, 0, fmt.Errorf("connecting to ghapp with the listing token: %w", err)
	}
	defer cs.Close()
	var all []listed
	cursor := ""
	for page := 1; ; page++ {
		args := map[string]any{}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "repo_list", Arguments: args})
		if err != nil {
			return nil, page, fmt.Errorf("repo_list: %w", err)
		}
		if res.IsError {
			return nil, page, fmt.Errorf("repo_list refused: %s", resultText(res))
		}
		var out struct {
			Objects    []listed `json:"objects"`
			NextCursor string   `json:"nextCursor"`
		}
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, page, fmt.Errorf("repo_list answered: %w", err)
		}
		all = append(all, out.Objects...)
		if out.NextCursor == "" {
			return all, page, nil
		}
		cursor = out.NextCursor
	}
}

func printListing(ctx context.Context, cfg config) error {
	objs, pages, err := listing(ctx, cfg)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"source": standIn, "pages": pages, "objects": objs})
}

// --- serving the agent ---

// gate is the stand-in's decision on each call.
type gate struct {
	cfg     config
	objects map[string]listed // by value, byte for byte
	ghapp   *mcp.ClientSession
	auditMu sync.Mutex
}

// held are the agent's roles, by name.
func (g *gate) held() map[string]role {
	out := map[string]role{}
	for _, name := range g.cfg.roles.Agents[g.cfg.agent] {
		if r, ok := g.cfg.roles.Roles[name]; ok {
			out[name] = r
		}
	}
	return out
}

// decision is what the gate decided for one call.
type decision struct {
	allowed bool
	role    string
	id      string // the id to send, "" through a role on every repo
}

// decide checks that a held role holds tool and covers repo. reachAll marks
// a tool that may touch any repo, which only a role on every repo covers.
func (g *gate) decide(tool, repo string, reachAll bool) decision {
	held := g.held()
	for _, name := range slices.Sorted(maps.Keys(held)) {
		r := held[name]
		if !slices.Contains(r.Tools, tool) {
			continue
		}
		obj, listedRepo := g.objects[repo]
		switch {
		case r.Covers.Every:
			return decision{allowed: true, role: name}
		case reachAll:
			continue
		case len(r.Covers.Objects) > 0 && listedRepo && slices.Contains(r.Covers.Objects, repo):
			return decision{allowed: true, role: name, id: obj.ID}
		case len(r.Covers.Where) > 0 && listedRepo && matches(r.Covers.Where, obj.Attributes):
			return decision{allowed: true, role: name, id: obj.ID}
		}
	}
	return decision{}
}

// matches reports whether attrs match where: for every attribute, any one
// value in the role's list.
func matches(where map[string][]string, attrs map[string]any) bool {
	for name, want := range where {
		var got []string
		switch v := attrs[name].(type) {
		case string:
			got = []string{v}
		case []any:
			for _, s := range v {
				if s, ok := s.(string); ok {
					got = append(got, s)
				}
			}
		}
		if !slices.ContainsFunc(got, func(s string) bool { return slices.Contains(want, s) }) {
			return false
		}
	}
	return true
}

// covered lists up to ten repos the agent's roles cover with tool, as
// Straza's refusal does.
func (g *gate) covered(tool string) []string {
	var out []string
	for _, value := range slices.Sorted(maps.Keys(g.objects)) {
		if g.decide(tool, value, false).allowed {
			out = append(out, value)
		}
		if len(out) == 10 {
			break
		}
	}
	return out
}

// auditLine is one line of the stand-in's audit: a tool call, or with kind
// grant, a git grant for the holder.
type auditLine struct {
	Time       string `json:"time"`
	Source     string `json:"source"`
	Kind       string `json:"kind,omitempty"`
	Agent      string `json:"agent"`
	Tool       string `json:"tool"`
	Repo       string `json:"repo,omitempty"`
	Op         string `json:"op,omitempty"`
	Refs       string `json:"refs,omitempty"`
	Decision   string `json:"decision"`
	Role       string `json:"role,omitempty"`
	ObjectID   string `json:"object_id,omitempty"`
	GrantID    string `json:"grant_id,omitempty"`
	DecisionID string `json:"decision_id,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
}

func (g *gate) audit(l auditLine) {
	l.Time, l.Source, l.Agent = time.Now().UTC().Format(time.RFC3339Nano), standIn, g.cfg.agent
	switch {
	case l.Kind == "grant" && l.Decision == "allowed":
		log.Printf("granted %s %s %s on %s by role %s, %s %s", l.Agent, l.Tool, l.Op, l.Repo, l.Role, l.GrantID, l.Refs)
	case l.Decision == "allowed":
		log.Printf("allowed %s %s on %s by role %s, object-id %q, %s", l.Agent, l.Tool, l.Repo, l.Role, l.ObjectID, l.Outcome)
	default:
		log.Printf("refused %s %s on %s: no held role covers it", l.Agent, l.Tool, l.Repo)
	}
	if g.cfg.auditPath == "" {
		return
	}
	b, _ := json.Marshal(l)
	g.auditMu.Lock()
	defer g.auditMu.Unlock()
	f, err := os.OpenFile(g.cfg.auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("audit: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// handler is the stand-in's handler for one ghapp tool: decide, then refuse
// or forward.
func (g *gate) handler(t *mcp.Tool) mcp.ToolHandler {
	argument, reachAll := objectMark(t)
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo := ""
		if argument != "" {
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			repo, _ = args[argument].(string)
		}
		d := g.decide(t.Name, repo, reachAll)
		if argument != "" && repo == "" {
			d = decision{}
		}
		if !d.allowed {
			g.audit(auditLine{Tool: t.Name, Repo: repo, Decision: "refused"})
			res := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprintf("%s on %s was refused: none of your roles on %s covers that repo.", t.Name, repo, g.cfg.roles.Server)},
			}}
			if cov := g.covered(t.Name); len(cov) > 0 {
				res.Content = append(res.Content, &mcp.TextContent{Text: fmt.Sprintf("[%s] Your roles on %s cover: %s.", standIn, g.cfg.roles.Server, strings.Join(cov, ", "))})
			}
			return res, nil
		}
		params := &mcp.CallToolParams{Name: t.Name, Arguments: req.Params.Arguments}
		if d.id != "" {
			params.Meta = mcp.Meta{metaObjectID: d.id}
		}
		res, err := g.ghapp.CallTool(ctx, params)
		outcome := "ok"
		switch {
		case err != nil:
			outcome = "error: " + err.Error()
		case res.IsError:
			outcome = "refused by ghapp"
		}
		g.audit(auditLine{Tool: t.Name, Repo: repo, Decision: "allowed", Role: d.role, ObjectID: d.id, Outcome: outcome})
		return res, err
	}
}

// objectMark reads ghapp's ai.straza/object mark: the argument that names
// the repo, or reach all.
func objectMark(t *mcp.Tool) (argument string, reachAll bool) {
	m, _ := t.Meta[metaObject].(map[string]any)
	argument, _ = m["argument"].(string)
	reach, _ := m["reach"].(string)
	return argument, reach == "all" || (argument == "" && reach != "none")
}

func serve(ctx context.Context, cfg config) error {
	objs, _, err := listing(ctx, cfg)
	if err != nil {
		return err
	}
	g := &gate{cfg: cfg, objects: map[string]listed{}}
	for _, o := range objs {
		g.objects[o.Value] = o
	}
	g.ghapp, err = connect(ctx, cfg.ghappURL, cfg.gatewaySecret)
	if err != nil {
		return fmt.Errorf("connecting to ghapp with the gateway secret: %w", err)
	}
	defer g.ghapp.Close()

	var all, offered []*mcp.Tool
	for t, err := range g.ghapp.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("ghapp's tools/list: %w", err)
		}
		all = append(all, t)
	}
	held := g.held()
	for _, t := range all {
		for _, r := range held {
			if slices.Contains(r.Tools, t.Name) {
				offered = append(offered, t)
				break
			}
		}
	}
	var names []string
	for _, t := range offered {
		names = append(names, t.Name)
	}
	instructions := fmt.Sprintf("%s. Agent %s. ghapp serves %d tools; the roles %s hold %d of them: %s.",
		standIn, cfg.agent, len(all), strings.Join(cfg.roles.Agents[cfg.agent], ", "), len(offered), strings.Join(names, ", "))
	server := mcp.NewServer(&mcp.Implementation{Name: "fakestraza", Title: standIn, Version: "demo"},
		&mcp.ServerOptions{Instructions: instructions})
	for _, t := range offered {
		server.AddTool(&mcp.Tool{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: t.Annotations,
		}, g.handler(t))
	}
	log.Printf("serving %d of ghapp's %d tools for %s, with %d listed repos", len(offered), len(all), cfg.agent, len(objs))
	return server.Run(ctx, &mcp.StdioTransport{})
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}
