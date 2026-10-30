package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strazahq/ghapp-mcp-server/internal/github"
)

// repo_list is the listing Straza reads every few minutes: every repo the
// App's installations reach, with GitHub's id and the configured custom
// properties as attributes.

const (
	// permCustomProperties is the App permission that reads an
	// organization's custom properties: the organization permission "Custom
	// properties", per GitHub's docs for the schema and values routes, and
	// seen working against a real App.
	permCustomProperties = "organization_custom_properties"

	maxAttributes      = 16  // attributes per object Straza accepts
	maxAttributeBytes  = 128 // bytes per attribute value
	maxAttributeValues = 16  // strings in a list value
	maxTitleBytes      = 256
	maxPropertyPages   = 1000 // 100,000 repos, Straza's limit, at 100 a page

	// propertyCacheTTL is how long one organization's property values are
	// reused across pages of a listing.
	propertyCacheTTL = 60 * time.Second
	// schemaReadTimeout bounds reading the property schemas when the server
	// is built.
	schemaReadTimeout = 5 * time.Second

	setByOrg    = "org"
	setByObject = "object"
)

type repoListInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"the nextCursor of the previous page. Leave it out for the first page"`
}

type listedObject struct {
	Value      string         `json:"value" jsonschema:"owner/name exactly as GitHub returns it"`
	ID         string         `json:"id" jsonschema:"GitHub's repository id, which never changes on a rename and is never reused"`
	Title      string         `json:"title"`
	Attributes map[string]any `json:"attributes" jsonschema:"custom property values: a string, or a list of strings for a multi-select property"`
}

type repoListOutput struct {
	Objects    []listedObject `json:"objects"`
	NextCursor string         `json:"nextCursor,omitempty" jsonschema:"pass it as cursor for the next page. Absent on the last page"`
}

// repoLister lists the repos and keeps each organization's property values
// for a minute.
type repoLister struct {
	gh     github.API
	tokens *TokenCache // the listing's tokens, reused like every other
	attrs  []string    // the custom properties reported as attributes
	now    func() time.Time
	logf   func(format string, args ...any)

	mu    sync.Mutex
	cache map[string]propertyEntry // by organization login
}

type propertyEntry struct {
	at     time.Time
	byRepo map[int64]map[string][]string
}

func newRepoLister(gh github.API, tokens *TokenCache, attrs []string) *repoLister {
	l := &repoLister{gh: gh, tokens: tokens, now: time.Now, logf: log.Printf, cache: map[string]propertyEntry{}}
	for _, a := range attrs {
		if slices.Contains(l.attrs, a) {
			continue
		}
		if len(l.attrs) == maxAttributes {
			l.logf("repo_list: %s lists more than %d attributes, Straza's limit. Only the first %d are reported.", envAttributes, maxAttributes, maxAttributes)
			break
		}
		l.attrs = append(l.attrs, a)
	}
	return l
}

// registerRepoList registers repo_list. It reads every organization's
// property schema first, for setBy, bounded by schemaReadTimeout.
//
// repo_list carries ai.straza/lists-objects, and ai.straza/object with reach
// all, so every tool is marked. It is the listing itself, called with the
// listing token, not a tool a role grants on some repos.
func registerRepoList(server *mcp.Server, l *repoLister) {
	ctx, cancel := context.WithTimeout(context.Background(), schemaReadTimeout)
	defer cancel()
	mcp.AddTool(server, &mcp.Tool{
		Name:  "repo_list",
		Title: "List repositories",
		Description: "List every repository this GitHub App can reach, across all its installations, one page at a time. " +
			"Each object has value (owner/name exactly as GitHub spells it), id (GitHub's repository id), title (the name) " +
			"and attributes (the configured custom properties). Pass nextCursor back as cursor until it is absent.",
		// The listing reads only the App's own installations: names, ids and
		// the configured properties, so its world is closed.
		Annotations: &mcp.ToolAnnotations{
			Title:           "List repositories",
			ReadOnlyHint:    true,
			DestructiveHint: ptr(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptr(false),
		},
		Meta: mcp.Meta{
			metaObject:       listingObjectMark,
			metaListsObjects: listsObjectsMark{Type: objectTypeRepo, Attributes: l.setBy(ctx)},
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in repoListInput) (*mcp.CallToolResult, repoListOutput, error) {
		out, err := l.list(ctx, in.Cursor)
		if err != nil {
			return nil, repoListOutput{}, err
		}
		line := fmt.Sprintf("%d repo(s) on this page.", len(out.Objects))
		if out.NextCursor != "" {
			line += " Pass nextCursor as cursor for the next page."
		} else {
			line += " This is the last page."
		}
		return textResult(line), out, nil
	})
}

// listCursor is where the next page starts: an installation and a page of
// its repos. On the wire it is opaque, base64 of its JSON.
type listCursor struct {
	Installation int64 `json:"i"`
	Page         int   `json:"p"`
}

func (c listCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (listCursor, error) {
	bad := invalidInputf("cursor", "cursor is not one repo_list returned. Start again without a cursor.")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return listCursor{}, bad
	}
	var c listCursor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() || c.Installation <= 0 || c.Page < 1 || c.Page > maxPropertyPages {
		return listCursor{}, bad
	}
	return c, nil
}

// list returns one page: up to 100 repos of one installation. Installations
// are walked by id, so a cursor still works when the installation it names
// is gone: the listing goes on with the next one.
func (l *repoLister) list(ctx context.Context, cursor string) (repoListOutput, error) {
	at := listCursor{Page: 1}
	if cursor != "" {
		c, err := decodeCursor(cursor)
		if err != nil {
			return repoListOutput{}, err
		}
		at = c
	}
	insts, err := l.gh.Installations(ctx)
	if err != nil {
		return repoListOutput{}, fmt.Errorf("listing the App's installations: %w", err)
	}
	slices.SortFunc(insts, func(a, b github.Installation) int { return cmp.Compare(a.ID, b.ID) })
	i, _ := slices.BinarySearchFunc(insts, at.Installation, func(inst github.Installation, id int64) int { return cmp.Compare(inst.ID, id) })
	out := repoListOutput{Objects: []listedObject{}}
	if i == len(insts) {
		return out, nil
	}
	inst, page := insts[i], 1
	if inst.ID == at.Installation {
		page = at.Page
	}

	withProps := len(l.attrs) > 0 && inst.AccountType == "Organization"
	perms := github.Permissions{"metadata": "read"}
	if withProps {
		perms[permCustomProperties] = "read"
	}
	// No repository ids: the token reaches the whole installation, with
	// metadata and properties read only. It is reused for the next pages.
	tok, reused, err := l.tokens.TokenFor(ctx, callerOf(ctx), inst.ID, nil, perms)
	if err != nil {
		return repoListOutput{}, fmt.Errorf("minting a listing token for installation %d: %w", inst.ID, err)
	}
	noteCallToken(ctx, tok, reused)
	repos, more, err := l.gh.InstallationRepos(ctx, tok, page)
	if err != nil {
		return repoListOutput{}, fmt.Errorf("listing the repos of installation %d: %w", inst.ID, err)
	}

	// A failed property read fails the whole listing. Straza then keeps its
	// last good list. A listing without attributes would look complete, and
	// every role that matches on an attribute would silently stop covering
	// these repos until the next listing.
	var props map[int64]map[string][]string
	if withProps {
		if props, err = l.properties(ctx, tok, inst.Account); err != nil {
			return repoListOutput{}, err
		}
	}
	for _, r := range repos {
		out.Objects = append(out.Objects, listedObject{
			Value:      r.FullName,
			ID:         strconv.FormatInt(r.ID, 10),
			Title:      capBytes(r.Name, maxTitleBytes),
			Attributes: l.attributes(r, props[r.ID]),
		})
	}

	switch {
	case more:
		out.NextCursor = listCursor{Installation: inst.ID, Page: page + 1}.encode()
	case i+1 < len(insts):
		out.NextCursor = listCursor{Installation: insts[i+1].ID, Page: 1}.encode()
	}
	return out, nil
}

// properties returns the custom property values of every repo of org, by
// repo id: read in full, every page, and kept for propertyCacheTTL.
func (l *repoLister) properties(ctx context.Context, tok github.Token, org string) (map[int64]map[string][]string, error) {
	l.mu.Lock()
	e, ok := l.cache[org]
	l.mu.Unlock()
	if ok && l.now().Sub(e.at) < propertyCacheTTL {
		return e.byRepo, nil
	}
	byRepo := map[int64]map[string][]string{}
	for page := 1; ; page++ {
		vals, more, err := l.gh.PropertyValues(ctx, tok, org, page)
		if err != nil {
			return nil, fmt.Errorf("reading the custom properties of %s: %w", org, err)
		}
		for _, v := range vals {
			byRepo[v.RepoID] = v.Values
		}
		if !more {
			break
		}
		if page == maxPropertyPages {
			return nil, fmt.Errorf("reading the custom properties of %s: more than %d pages", org, maxPropertyPages)
		}
	}
	l.mu.Lock()
	l.cache[org] = propertyEntry{at: l.now(), byRepo: byRepo}
	l.mu.Unlock()
	return byRepo, nil
}

// attributes are the configured properties of one repo: a string for one
// value, a list for several. A value Straza would refuse, over 128 bytes or a
// list over 16 strings, is dropped and logged rather than failing the listing,
// so the repo appears without that attribute: no role covers it through that
// attribute, and every other attribute still holds.
func (l *repoLister) attributes(r github.Repo, values map[string][]string) map[string]any {
	out := map[string]any{}
	for _, name := range l.attrs {
		vs := values[name]
		switch {
		case len(vs) == 0:
			continue
		case len(vs) > maxAttributeValues:
			l.logf("repo_list: dropped attribute %q of %s (id %d): %d values, over Straza's limit of %d", name, r.FullName, r.ID, len(vs), maxAttributeValues)
			continue
		}
		if long := slices.IndexFunc(vs, func(v string) bool { return len(v) > maxAttributeBytes }); long >= 0 {
			l.logf("repo_list: dropped attribute %q of %s (id %d): a value of %d bytes, over Straza's limit of %d", name, r.FullName, r.ID, len(vs[long]), maxAttributeBytes)
			continue
		}
		if len(vs) == 1 {
			out[name] = vs[0]
		} else {
			out[name] = slices.Clone(vs)
		}
	}
	return out
}

// setBy reads every organization's property schema and says who can set
// each configured attribute: "org" when every organization that defines it
// lets only organization owners set it, else "object". A failed read, a
// disagreement between organizations, an unknown values_editable_by, or a
// property no organization defines, give "object": Straza then warns that
// repo admins may move their repo into a role, which is the safe mistake.
func (l *repoLister) setBy(ctx context.Context) []listedAttribute {
	out := []listedAttribute{}
	if len(l.attrs) == 0 {
		return out
	}
	found, ok := l.readSchemas(ctx)
	for _, name := range l.attrs {
		v := setByObject
		if ok && found[name] == setByOrg {
			v = setByOrg
		}
		out = append(out, listedAttribute{Name: name, SetBy: v})
	}
	return out
}

// readSchemas returns the setBy each organization's schema gives each
// configured property, merged: "object" wins over "org". ok is false when any
// read failed.
func (l *repoLister) readSchemas(ctx context.Context) (map[string]string, bool) {
	insts, err := l.gh.Installations(ctx)
	if err != nil {
		l.logf("repo_list: reading the installations for the property schemas: %v. Every attribute is reported as set by the object.", err)
		return nil, false
	}
	wanted := map[string]bool{}
	for _, a := range l.attrs {
		wanted[a] = true
	}
	found := map[string]string{}
	for _, inst := range insts {
		if inst.AccountType != "Organization" {
			continue
		}
		tok, _, err := l.tokens.TokenFor(ctx, callerOf(ctx), inst.ID, nil, github.Permissions{"metadata": "read", permCustomProperties: "read"})
		if err != nil {
			l.logf("repo_list: minting a token to read the property schema of %s: %v. Every attribute is reported as set by the object.", inst.Account, err)
			return nil, false
		}
		defs, err := l.gh.PropertySchema(ctx, tok, inst.Account)
		if err != nil {
			l.logf("repo_list: reading the property schema of %s: %v. Every attribute is reported as set by the object.", inst.Account, err)
			return nil, false
		}
		for _, d := range defs {
			if !wanted[d.Name] {
				continue
			}
			v := setByObject
			switch d.ValuesEditableBy {
			case "", "org_actors":
				v = setByOrg
			case "org_and_repo_actors":
			default:
				l.logf("repo_list: property %q of %s has values_editable_by %q, which ghapp does not know. It is reported as set by the object.", d.Name, inst.Account, d.ValuesEditableBy)
			}
			if prev, seen := found[d.Name]; seen && prev != v {
				v = setByObject
			}
			found[d.Name] = v
		}
	}
	return found, true
}
