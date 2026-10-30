package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Straza grants at ghapp's git endpoint. Straza's holder on the agent's
// machine takes the agent's plain git and sends each request on to /git with
//
//	Authorization: DPoP <grant>
//	DPoP: <proof>
//
// The grant is a JWT Straza signs for one git operation on one repo, and for
// a push, for exactly the ref updates it names. The proof is a DPoP proof
// (RFC 9449) the holder signs for this one request with its own key, the key
// the grant names in cnf.jkt. A grant copied off the machine opens nothing
// without that key.
//
// The claims and the checks follow Straza's grant spec, spec/grant
// v1alpha1, and testdata/spec-grant replays its examples. Two tolerances stay: typ is compared as a media type, which the
// spec allows, and obj.id may come as a JSON number as well as a string.
// ghapp checks no proof nonce: Straza's nonces are its own HMAC, which ghapp
// cannot check, so an iat window and single-use jtis keep a proof fresh.
// The git endpoint needs GHAPP_PUBLIC_URL and GHAPP_GIT_GRANT_ISSUER.

const (
	authSchemeDPoP = "DPoP"
	grantType      = "straza-grant+jwt"
	proofType      = "dpop+jwt"
	grantObjType   = "repo"

	grantMaxTTL    = 5 * time.Minute
	grantClockSkew = time.Minute // between Straza, the holder and ghapp
	proofMaxAge    = time.Minute // a proof is made just before its request
	grantMaxLen    = 8 << 10     // bytes
	proofMaxLen    = 4 << 10     // bytes
	grantFieldMax  = 512         // bytes of a string claim
	jktLen         = 43          // a SHA-256 thumbprint in base64url

	// strazaJWKSPath is where Straza publishes its signing keys, under its
	// public URL (spec/session-token).
	strazaJWKSPath = "/.well-known/straza/jwks.json"
)

// The reasons a grant or a proof is refused. Their text goes to git, which
// prints it, so it says what happened and never quotes the grant.
var (
	errGrantMalformed = errors.New("the Authorization header holds no Straza grant ghapp can read")
	errGrantSignature = errors.New("the grant is not signed by a key Straza publishes")
	errGrantKeys      = errors.New("ghapp could not read Straza's signing keys. Try again")
	errGrantFields    = errors.New("the grant is signed but does not hold valid claims")
	errGrantNotOurs   = errors.New("the grant is not for this ghapp")
	errGrantExpired   = errors.New("the grant expired. Run git again, and Straza's holder asks for a new one")
	errGrantUsed      = errors.New("the grant was used: a grant opens one git operation. Run git again, and Straza's holder asks for a new one")
	errProofMissing   = errors.New("a Straza grant needs one DPoP proof header")
	errProofInvalid   = errors.New("the DPoP proof is not valid")
	errProofUsed      = errors.New("the DPoP proof was used: a proof opens one request")
)

// proofError is errProofInvalid with a reason for ghapp's own log.
type proofError struct{ reason string }

func (e *proofError) Error() string        { return errProofInvalid.Error() }
func (e *proofError) Is(target error) bool { return target == errProofInvalid }

// grantClaims is what a grant says.
type grantClaims struct {
	Iss  string          `json:"iss"`
	Aud  string          `json:"aud"` // one string, ghapp's public URL exactly
	Sub  string          `json:"sub"` // the user id
	Act  json.RawMessage `json:"act"` // grantActor, kept as sent for the token cache key
	Iat  unixTime        `json:"iat"`
	Exp  unixTime        `json:"exp"`
	Jti  string          `json:"jti"`
	Cnf  grantCnf        `json:"cnf"`
	Srv  string          `json:"srv"` // the name of ghapp's app in Straza
	Obj  grantObject     `json:"obj"`
	Op   string          `json:"op"` // gitOpFetch or gitOpPush
	Refs []grantRef      `json:"refs"`
	Dec  string          `json:"dec"` // the id of Straza's audit record of the grant
	Apr  string          `json:"apr"` // the approval id, when one ran
}

// grantActor is what act holds: the session id, the session's harness as
// "<name>/<version>" when it names one, and the subject's user type.
type grantActor struct {
	Ses  string  `json:"ses"`
	Hrn  *string `json:"hrn"`
	Kind string  `json:"kind"`
}

// unixTime is a time claim in whole seconds since the epoch. spec/grant
// pins iat and exp as integers, so a fraction, a string or null is refused.
type unixTime int64

func (u *unixTime) UnmarshalJSON(b []byte) error {
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n <= 0 {
		return errGrantFields
	}
	*u = unixTime(n)
	return nil
}

func (u unixTime) asTime() time.Time { return time.Unix(int64(u), 0) }

type grantCnf struct {
	JKT string `json:"jkt"`
}

type grantObject struct {
	Type  string   `json:"type"`
	Value string   `json:"value"` // owner/name
	ID    objectID `json:"id"`    // GitHub's repository id
}

// grantRef is one ref update a push grant allows.
type grantRef struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// objectID is a repository id sent as a JSON string, as Straza sends object
// ids, or as a number.
type objectID int64

func (o *objectID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if unq, err := strconv.Unquote(s); err == nil {
		s = unq
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return errGrantFields
	}
	*o = objectID(n)
	return nil
}

// expires is when the grant stops working.
func (c grantClaims) expires() time.Time { return c.Exp.asTime().UTC() }

// actorValid reports whether act is an object with a session id, a user
// type spec/grant names and, when present, a harness. Other members are
// allowed and stay in the token cache key.
func (c grantClaims) actorValid() bool {
	var a grantActor
	if json.Unmarshal(c.Act, &a) != nil {
		return false
	}
	switch a.Kind {
	case "human", "agent", "service":
	default:
		return false
	}
	return a.Ses != "" && (a.Hrn == nil || *a.Hrn != "")
}

// actor is the canonical form of act: the JSON object with its keys in
// order and its numbers as sent, or "" when act is not a JSON object with
// at least one member, which check refuses.
func (c grantClaims) actor() string {
	dec := json.NewDecoder(bytes.NewReader(c.Act))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m) // map keys come out sorted
	return string(b)
}

// grantVerifier checks grants against Straza's published keys.
type grantVerifier struct {
	issuer   string // Straza's public URL, the grants' iss
	audience string // ghapp's public URL, the grants' aud
	keys     *keySet
}

func newGrantVerifier(issuer, jwksURL, audience string, log *slog.Logger) *grantVerifier {
	return &grantVerifier{issuer: issuer, audience: audience, keys: newKeySet(jwksURL, log)}
}

// verify checks the grant's form, its signature with Straza's key named by
// kid, and then every claim against now.
func (v *grantVerifier) verify(ctx context.Context, raw string, now time.Time) (grantClaims, error) {
	if len(raw) > grantMaxLen {
		return grantClaims{}, errGrantMalformed
	}
	jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(jws.Signatures) != 1 {
		return grantClaims{}, errGrantMalformed
	}
	h := jws.Signatures[0].Protected
	if !mediaType(h, grantType) || h.KeyID == "" {
		return grantClaims{}, errGrantMalformed
	}
	key, err := v.keys.key(ctx, h.KeyID)
	if err != nil {
		return grantClaims{}, err
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return grantClaims{}, errGrantSignature
	}
	var c grantClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return grantClaims{}, errGrantFields
	}
	if err := c.check(v.issuer, v.audience, now); err != nil {
		return grantClaims{}, err
	}
	return c, nil
}

// check validates every claim ghapp reads, in the forms spec/grant pins.
func (c grantClaims) check(issuer, audience string, now time.Time) error {
	switch {
	case c.Iat == 0, c.Exp == 0,
		!loggable(c.Sub), c.Sub == "", !loggable(c.Jti), c.Jti == "",
		!loggable(c.Srv), c.Srv == "", !loggable(c.Dec), c.Dec == "", !loggable(c.Apr),
		// act keys the token cache per agent session, so it must be there.
		len(c.Act) > grantFieldMax, !c.actorValid(),
		len(c.Cnf.JKT) != jktLen, !isBase64URL(c.Cnf.JKT),
		c.Op != gitOpFetch && c.Op != gitOpPush,
		c.Obj.Type != grantObjType, checkRepoName(c.Obj.Value) != nil, c.Obj.ID <= 0,
		// refs come only on a push that names updates, never as an empty list.
		c.Op != gitOpPush && c.Refs != nil, c.Refs != nil && len(c.Refs) == 0, len(c.Refs) > pushMaxRefs:
		return errGrantFields
	case c.Iss != issuer, c.Aud != audience:
		return errGrantNotOurs
	}
	for _, r := range c.Refs {
		if !loggable(r.Ref) || !strings.HasPrefix(r.Ref, "refs/") || len(r.Ref) == len("refs/") ||
			!isObjectID(r.Old) || !isObjectID(r.New) || len(r.Old) != len(r.New) {
			return errGrantFields
		}
	}
	iat, exp := c.Iat.asTime(), c.Exp.asTime()
	switch {
	case !exp.After(iat), exp.Sub(iat) > grantMaxTTL, iat.After(now.Add(grantClockSkew)):
		return errGrantFields
	case !now.Before(exp.Add(grantClockSkew)):
		return errGrantExpired
	}
	return nil
}

// allows reports why the grant does not open this route, or "". A grant
// opens only its own service: a fetch grant upload-pack, a push grant
// receive-pack, so ghapp mints the permission of the tool Straza decided.
// info/refs takes a grant without refs, since a push grant with refs opens
// only the POST of its updates (spec/grant, The git binding).
func (c grantClaims) allows(rt gitRoute) string {
	if c.Op != rt.op() || rt.InfoRefs && len(c.Refs) > 0 {
		return fmt.Sprintf("the grant is for %s, not for %s", c.Op, rt.step())
	}
	return ""
}

// proofClaims is what a DPoP proof says (RFC 9449 section 4.2).
type proofClaims struct {
	Jti   string           `json:"jti"`
	Htm   string           `json:"htm"`
	Htu   string           `json:"htu"`
	Iat   *jwt.NumericDate `json:"iat"`
	Ath   string           `json:"ath"`
	Nonce string           `json:"nonce"`
}

// checkedProof is a proof that passed, with what the endpoint spends and logs.
type checkedProof struct {
	jti string
	jkt string // the proof key's thumbprint, equal to the grant's cnf.jkt
	iat time.Time
}

// verifyProof checks a DPoP proof for one request (RFC 9449 section 4.3):
// typ dpop+jwt, EdDSA with the Ed25519 public key in its jwk header, the
// signature, the method, the URL without query, an iat within the window,
// ath bound to the grant, and the key's thumbprint equal to the grant's
// cnf.jkt. A nonce, when present, is not checked.
func verifyProof(proof, method, wantURL, grant, jkt string, now time.Time) (checkedProof, error) {
	if len(proof) > proofMaxLen {
		return checkedProof{}, &proofError{"the proof is too long"}
	}
	jws, err := jose.ParseSignedCompact(proof, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil || len(jws.Signatures) != 1 {
		return checkedProof{}, &proofError{"the proof is not a compact JWS with EdDSA"}
	}
	h := jws.Signatures[0].Protected
	if !mediaType(h, proofType) {
		return checkedProof{}, &proofError{"typ is not " + proofType}
	}
	jwk := h.JSONWebKey
	if jwk == nil || !jwk.Valid() || !jwk.IsPublic() {
		return checkedProof{}, &proofError{"no public jwk in the header"}
	}
	pub, ok := jwk.Key.(ed25519.PublicKey)
	if !ok {
		return checkedProof{}, &proofError{"the jwk is not an Ed25519 key"}
	}
	payload, err := jws.Verify(pub)
	if err != nil {
		return checkedProof{}, &proofError{"the signature does not match the jwk"}
	}
	var c proofClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return checkedProof{}, &proofError{"the claims are not valid JSON"}
	}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return checkedProof{}, &proofError{"no thumbprint for the jwk"}
	}
	ath := sha256.Sum256([]byte(grant))
	switch {
	case c.Jti == "" || !loggable(c.Jti):
		return checkedProof{}, &proofError{"no valid jti"}
	case c.Htm != method:
		return checkedProof{}, &proofError{"htm is not " + method}
	case !sameURL(c.Htu, wantURL):
		return checkedProof{}, &proofError{"htu is not " + wantURL}
	case c.Iat == nil:
		return checkedProof{}, &proofError{"no iat"}
	case c.Iat.Time().After(now.Add(grantClockSkew)):
		return checkedProof{}, &proofError{"iat is in the future"}
	case c.Iat.Time().Before(now.Add(-proofMaxAge - grantClockSkew)):
		return checkedProof{}, &proofError{"iat is too old"}
	case c.Ath != b64url.EncodeToString(ath[:]):
		return checkedProof{}, &proofError{"ath does not match the grant"}
	case b64url.EncodeToString(thumb) != jkt:
		return checkedProof{}, &proofError{"the proof key is not the key the grant names"}
	}
	return checkedProof{jti: c.Jti, jkt: jkt, iat: c.Iat.Time()}, nil
}

// spentUntil is how long a proof's jti is remembered: as long as its iat
// passes the window.
func (p checkedProof) spentUntil() time.Time {
	return p.iat.Add(proofMaxAge + 2*grantClockSkew)
}

// mediaType reports whether the header's typ is want, compared as a media
// type: without case, with or without "application/" (RFC 8725 3.11).
func mediaType(h jose.Header, want string) bool {
	typ, _ := h.ExtraHeaders[jose.HeaderType].(string)
	typ = strings.ToLower(typ)
	return typ == want || typ == "application/"+want
}

// sameURL compares a proof's htu with the request's URL without query and
// fragment, after RFC 3986's syntax normalization: scheme and host without
// case, and a default port the same as none.
func sameURL(htu, want string) bool {
	a, err1 := url.Parse(htu)
	b, err2 := url.Parse(want)
	if err1 != nil || err2 != nil || a.Opaque != "" || a.User != nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		hostPort(a) == hostPort(b) &&
		a.EscapedPath() == b.EscapedPath()
}

func hostPort(u *url.URL) string {
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" || (port == "443" && strings.EqualFold(u.Scheme, "https")) || (port == "80" && strings.EqualFold(u.Scheme, "http")) {
		return host
	}
	return host + ":" + port
}

// loggable reports whether a claim may go into a log line as it is.
func loggable(s string) bool {
	return len(s) <= grantFieldMax && strings.IndexFunc(s, unicode.IsControl) < 0
}

func isBase64URL(s string) bool {
	_, err := b64url.DecodeString(s)
	return err == nil
}

// grantedRefusals compares a push's ref updates with the ones its grant
// names. They must be the same set, so a reason is returned for every
// command when they are not, and nil when they are. A command the grant
// does not name is refused as such, and the others as part of a push that
// was refused.
func grantedRefusals(cmds []pushCommand, granted []grantRef) []string {
	want := map[string]int{}
	for _, r := range granted {
		want[r.Ref+" "+strings.ToLower(r.Old)+" "+strings.ToLower(r.New)]++
	}
	reasons := make([]string, len(cmds))
	refused := len(cmds) != len(granted)
	for i, c := range cmds {
		k := c.Ref + " " + strings.ToLower(c.Old) + " " + strings.ToLower(c.New)
		if want[k] == 0 {
			reasons[i] = reasonNotGranted
			refused = true
			continue
		}
		want[k]--
	}
	if !refused {
		return nil
	}
	for i := range reasons {
		if reasons[i] == "" {
			reasons[i] = reasonGrantMismatch
		}
	}
	return reasons
}

const (
	reasonNotGranted    = "Straza's grant does not name this update"
	reasonGrantMismatch = "not pushed, because this push is not the one Straza granted"
	// reasonRefsNoCommands refuses a push without ref updates, git's probe,
	// that comes with a grant naming updates, which opens only their push.
	reasonRefsNoCommands = "Straza's grant names ref updates, so it opens only the push of those updates, and this push carries none"
)

// Straza's signing keys.
const (
	keySetMaxAge      = 10 * time.Minute // a key set is read again after this
	keySetMinInterval = 30 * time.Second // and at most this often for an unknown kid
	keySetMaxStale    = time.Hour        // keys kept while Straza cannot be read
	keySetTimeout     = 10 * time.Second
	keySetMaxBytes    = 1 << 20
)

// keySet is Straza's published key set, read when first needed and again
// when it is old or a grant names a kid it lacks. It keeps only Ed25519 keys
// for signing.
//
// A read runs on its own, not on the request that started it, so a client
// that hangs up cannot cut it short, and it holds no lock while it waits for
// Straza. Only a request that needs its answer waits for it: a kid the set
// lacks, or a set too old to serve. A known kid is served from the set
// while a read is due or under way.
type keySet struct {
	url    string
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	keys     map[string]ed25519.PublicKey
	fetched  time.Time     // the last read that worked
	tried    time.Time     // the last read started
	inflight chan struct{} // closed when the read under way ends, nil without one
}

func newKeySet(jwksURL string, log *slog.Logger) *keySet {
	return &keySet{
		url: jwksURL,
		client: &http.Client{
			Timeout: keySetTimeout,
			// The key set is read from the configured URL only.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: log,
		now: time.Now,
	}
}

// key returns the key kid names. It starts a read of the set when the set is
// older than keySetMaxAge or lacks kid, at most once every
// keySetMinInterval. When a read fails, the keys of the last good read serve
// for up to keySetMaxStale.
func (k *keySet) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	k.mu.Lock()
	now := k.now()
	pub, known := k.keys[kid]
	if known && !k.fetched.IsZero() && now.Sub(k.fetched) < keySetMaxAge {
		k.mu.Unlock()
		return pub, nil
	}
	wait := k.inflight
	if wait == nil && (k.tried.IsZero() || now.Sub(k.tried) >= keySetMinInterval) {
		k.tried = now
		wait = make(chan struct{})
		k.inflight = wait
		go k.refresh(wait)
	}
	if known && !k.fetched.IsZero() && now.Sub(k.fetched) < keySetMaxStale {
		// Old but still served, while the read runs.
		k.mu.Unlock()
		return pub, nil
	}
	k.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, errGrantKeys
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fetched.IsZero() || k.now().Sub(k.fetched) >= keySetMaxStale {
		return nil, errGrantKeys
	}
	if pub, ok := k.keys[kid]; ok {
		return pub, nil
	}
	return nil, errGrantSignature
}

// refresh reads the set with its own deadline, keeps it when the read
// works, and closes done.
func (k *keySet) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), keySetTimeout)
	defer cancel()
	keys, err := k.fetch(ctx)
	k.mu.Lock()
	if err != nil {
		k.log.Warn("reading Straza's signing keys failed", slog.String("url", k.url), slog.String("error", err.Error()))
	} else {
		k.keys, k.fetched = keys, k.now()
	}
	k.inflight = nil
	k.mu.Unlock()
	close(done)
}

// settle waits for a read under way to end. For tests.
func (k *keySet) settle() {
	k.mu.Lock()
	wait := k.inflight
	k.mu.Unlock()
	if wait != nil {
		<-wait
	}
}

// fetch reads the key set.
func (k *keySet) fetch(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, keySetMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > keySetMaxBytes {
		return nil, fmt.Errorf("the key set is larger than %d bytes", keySetMaxBytes)
	}
	// Each key alone, so a key of a type go-jose does not know is skipped
	// and does not fail the set.
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&set); err != nil {
		return nil, fmt.Errorf("the key set is not a JWKS: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, raw := range set.Keys {
		var jwk jose.JSONWebKey
		if jwk.UnmarshalJSON(raw) != nil {
			continue
		}
		pub, ok := jwk.Key.(ed25519.PublicKey)
		if !ok || jwk.KeyID == "" || (jwk.Use != "" && jwk.Use != "sig") || (jwk.Algorithm != "" && jwk.Algorithm != string(jose.EdDSA)) {
			continue
		}
		keys[jwk.KeyID] = pub
	}
	return keys, nil
}
