package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// botLogin is the App's bot, the actor of everything the App does.
const botLogin = "ghapp[bot]"

// The demo organization and its one installation of the App.
const (
	orgLogin = "Strazahq"
	instID   = 55500001
)

// appPerms are the permissions the App was granted on the installation. A
// token may ask for these or less, never more.
var appPerms = map[string]string{
	"metadata":                       "read",
	"contents":                       "write",
	"pull_requests":                  "write",
	"issues":                         "write",
	"organization_custom_properties": "read",
}

// installation is one installation of the App.
type installation struct {
	ID          int64
	Account     string
	AccountType string
}

// repo is one repository and what lives on it besides git.
type repo struct {
	ID            int64
	Owner, Name   string
	Team          string
	Installation  int64
	DefaultBranch string
	nextNumber    int
	issues        []*issue // issues and pull requests, which share numbers
	pushMu        sync.Mutex
}

func (r *repo) fullName() string { return r.Owner + "/" + r.Name }

// issue is an issue or, with Pull set, a pull request.
type issue struct {
	ID       int64
	Number   int
	Title    string
	Body     string
	State    string
	Author   string
	Labels   []string
	Comments []comment
	Created  time.Time
	Updated  time.Time
	Closed   time.Time

	Pull     bool
	Draft    bool
	Head     string
	Base     string
	Merged   bool
	MergedAt time.Time
	MergeSHA string
}

type comment struct {
	ID      int64
	Body    string
	Author  string
	Created time.Time
}

// token is an installation token: what it reaches and may do.
type token struct {
	Installation int64
	RepoIDs      []int64 // nil: every repo of the installation
	Perms        map[string]string
	Expires      time.Time
}

// fake is the fake GitHub. One mutex guards its state; git runs outside it.
type fake struct {
	appID   string
	pub     any // the App's public key
	gitRoot string
	gitBin  string
	backend string // git http-backend
	base    string // this server's URL, set once it listens
	now     func() time.Time

	audit   io.Writer
	auditMu sync.Mutex

	mu      sync.Mutex
	insts   []installation
	repos   []*repo
	tokens  map[string]*token
	nextID  int64
	handler http.Handler
}

func newFake(appID string, pub any, gitRoot, gitBin, backend string, audit io.Writer) (*fake, error) {
	f := &fake{
		appID:   appID,
		pub:     pub,
		gitRoot: gitRoot,
		gitBin:  gitBin,
		backend: backend,
		now:     time.Now,
		audit:   audit,
		insts:   []installation{{ID: instID, Account: orgLogin, AccountType: "Organization"}},
		tokens:  map[string]*token{},
		nextID:  3000000001,
	}
	for _, s := range seedRepos {
		f.repos = append(f.repos, &repo{ID: s.id, Owner: orgLogin, Name: s.name, Team: s.team, Installation: instID, DefaultBranch: "main", nextNumber: 1})
	}
	f.handler = f.routes()
	return f, nil
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m := gitPath.FindStringSubmatch(r.URL.Path); m != nil {
		f.serveGit(w, r, m[1], m[2], m[3])
		return
	}
	f.handler.ServeHTTP(w, r)
}

// newID is a fresh id for an issue or a comment. The caller holds mu.
func (f *fake) newID() int64 {
	f.nextID++
	return f.nextID
}

// repoByName finds a repo by owner/name, without regard to case, as GitHub
// resolves names. The caller holds mu.
func (f *fake) repoByName(owner, name string) *repo {
	for _, r := range f.repos {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

// repoByID finds a repo by id. The caller holds mu.
func (f *fake) repoByID(id int64) *repo {
	for _, r := range f.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// reaches reports whether tok reaches r: its installation, and its repo ids
// when it has some.
func (tok *token) reaches(r *repo) bool {
	return tok.Installation == r.Installation && (tok.RepoIDs == nil || slices.Contains(tok.RepoIDs, r.ID))
}

// has reports whether tok holds perm at level or above. Every token reads
// metadata, as on GitHub.
func (tok *token) has(perm, level string) bool {
	if perm == "metadata" && level == "read" {
		return true
	}
	return levelRank(tok.Perms[perm]) >= levelRank(level)
}

func levelRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// newTokenValue is a fresh token in GitHub's ghs_ form.
func newTokenValue() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 36)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[n.Int64()]
	}
	return "ghs_" + string(b)
}

// hashedToken is base64(sha256(token)), the hashed_token of GitHub's audit
// log.
func hashedToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// auditEvent is one line of the audit log, in the shape GitHub's audit log
// gives an event done with a GitHub App installation token.
type auditEvent struct {
	Timestamp      int64          `json:"@timestamp"`
	Action         string         `json:"action"`
	Actor          string         `json:"actor"`
	ActorIsBot     bool           `json:"actor_is_bot"`
	Org            string         `json:"org"`
	Repo           string         `json:"repo"`
	RepositoryID   int64          `json:"repository_id"`
	HashedToken    string         `json:"hashed_token"`
	AccessType     string         `json:"programmatic_access_type"`
	Transport      string         `json:"transport_protocol_name,omitempty"`
	RequestMethod  string         `json:"request_method,omitempty"`
	RequestPath    string         `json:"request_path,omitempty"`
	Refs           []refUpdate    `json:"refs,omitempty"`
	Details        map[string]any `json:"details,omitempty"`
	StandInComment string         `json:"_stand_in"`
}

// refUpdate is one ref a push changed.
type refUpdate struct {
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// record writes one audit line, to the audit file and to the log.
func (f *fake) record(ev auditEvent) {
	ev.Timestamp = f.now().UnixMilli()
	ev.Actor, ev.ActorIsBot, ev.Org = botLogin, true, orgLogin
	ev.AccessType = "GitHub App server-to-server token"
	ev.StandInComment = "fakegithub, a stand-in, not GitHub"
	b, err := json.Marshal(ev)
	if err != nil {
		log.Printf("audit: %v", err)
		return
	}
	f.auditMu.Lock()
	defer f.auditMu.Unlock()
	if _, err := f.audit.Write(append(b, '\n')); err != nil {
		log.Printf("audit: %v", err)
	}
	log.Printf("audit: %s %s by %s, hashed_token %s", ev.Action, ev.Repo, ev.Actor, ev.HashedToken)
}
