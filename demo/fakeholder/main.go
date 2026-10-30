// Command fakeholder is a stand-in, not Straza's holder, for the demo in
// demo/run.sh.
//
// Straza's holder, `straza service`, runs on the agent's machine under its
// own OS account and alone holds Straza's credential. Its git reaches it
// through a remote helper. The stand-in takes git's smart HTTP instead,
// through one git setting, url.<holder>/git/.insteadOf https://github.com/.
// For each git request it asks Straza (fakestraza grants) for a grant, with
// the body of Straza's POST /v1/grants: a fetch grant for upload-pack and its
// info/refs, a push grant without refs for receive-pack's info/refs and a
// push without commands, and a push grant naming the ref updates it reads
// before the pack for a push with commands. It sends the request on to the
// grant's aud and repo with the grant and a DPoP proof (RFC 9449) signed by
// its own Ed25519 key, made at start. A refusal from Straza goes back to git
// as a plain line, which git prints.
//
//	fakeholder -listen 127.0.0.1:PORT -straza http://127.0.0.1:PORT
package main

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const standIn = "fakeholder (stand-in, not Straza's holder)"

var b64 = base64.RawURLEncoding

// The headers that pass between git and ghapp, as ghapp's endpoint passes them.
var (
	requestHeaders  = []string{"Git-Protocol", "Content-Type", "Content-Encoding", "Accept", "User-Agent"}
	responseHeaders = []string{"Content-Type", "Content-Encoding", "Cache-Control", "Expires", "Pragma"}
)

// gitHost is the host the agent's git URLs name. The one git setting sends
// them here, and the holder names each repo to Straza by its URL there.
const gitHost = "https://github.com/"

type holder struct {
	straza string
	key    ed25519.PrivateKey
	jkt    string
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(standIn + ": ")
	listen := flag.String("listen", "", "host:port to serve git on")
	straza := flag.String("straza", "", "fakestraza's grant service, such as http://127.0.0.1:9000")
	flag.Parse()
	if *listen == "" || *straza == "" {
		flag.Usage()
		log.Fatal("-listen and -straza are required")
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: pub}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		log.Fatal(err)
	}
	h := &holder{straza: strings.TrimRight(*straza, "/"), key: key, jkt: b64.EncodeToString(thumb)}
	log.Printf("serving git at http://%s/git/ with key %s", *listen, h.jkt)
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// route is what git asked for: the repo, the service, and info/refs or not.
type route struct {
	repo, service string
	infoRefs      bool
}

// path is the route's path at a git server, under repo.
func (rt route) path(repo string) string {
	tail := rt.service
	if rt.infoRefs {
		tail = "info/refs"
	}
	return "/git/" + repo + ".git/" + tail
}

func parseRoute(r *http.Request) (route, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/git/")
	segs := strings.Split(rest, "/")
	if !ok || len(segs) < 3 {
		return route{}, false
	}
	rt := route{repo: segs[0] + "/" + strings.TrimSuffix(segs[1], ".git")}
	switch tail := strings.Join(segs[2:], "/"); tail {
	case "info/refs":
		rt.infoRefs, rt.service = true, r.URL.Query().Get("service")
	case "git-upload-pack", "git-receive-pack":
		rt.service = tail
	default:
		return route{}, false
	}
	return rt, rt.service == "git-upload-pack" || rt.service == "git-receive-pack"
}

func (h *holder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, ok := parseRoute(r)
	if !ok {
		plain(w, http.StatusNotFound, "the holder serves git's smart HTTP at /git/{owner}/{name}.git/")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<30))
	if err != nil {
		plain(w, http.StatusBadRequest, "the holder could not read the request")
		return
	}
	req := grantRequest{Upstream: gitHost + rt.repo + ".git", JKT: h.jkt, Op: "fetch"}
	if rt.service == "git-receive-pack" {
		req.Op = "push"
	}
	if rt.service == "git-receive-pack" && !rt.infoRefs {
		if r.Header.Get("Content-Encoding") != "" {
			plain(w, http.StatusUnsupportedMediaType, "the holder reads a plain push only")
			return
		}
		cmds, err := commands(body)
		if err != nil {
			plain(w, http.StatusBadRequest, "the holder could not read the push: "+err.Error())
			return
		}
		req.Refs = cmds // none for git's probe, and then a grant without refs
	}
	a, status := h.ask(req)
	if a.Grant == "" {
		log.Printf("Straza refused %s on %s: %s", req.Op, rt.repo, a.Error)
		plain(w, status, a.Error)
		return
	}
	log.Printf("Straza granted %s on %s%s, for %s until %s", req.Op, a.Repo, refsText(req.Refs), a.Aud, a.Exp)

	// Git goes to the server and the repo Straza named, as Straza's holder
	// sends it.
	target := a.Aud + rt.path(a.Repo)
	uri := target
	if r.URL.RawQuery != "" {
		uri += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, uri, bytes.NewReader(body))
	if err != nil {
		plain(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, name := range requestHeaders {
		if v := r.Header.Values(name); len(v) > 0 {
			out.Header[http.CanonicalHeaderKey(name)] = slices.Clone(v)
		}
	}
	proof, err := h.proof(r.Method, target, a.Grant)
	if err != nil {
		plain(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Header.Set("Authorization", "DPoP "+a.Grant)
	out.Header.Set("DPoP", proof)
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		plain(w, http.StatusBadGateway, "the holder could not reach ghapp")
		return
	}
	defer resp.Body.Close()
	for _, name := range append([]string{"Content-Length"}, responseHeaders...) {
		if v := resp.Header.Values(name); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(name)] = slices.Clone(v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// grantRequest is the body of Straza's POST /v1/grants, with the key's
// thumbprint in jkt for the stand-in, which has no session token to take it
// from.
type grantRequest struct {
	Upstream string     `json:"upstream"`
	Op       string     `json:"op"`
	Refs     []grantRef `json:"refs,omitempty"`
	JKT      string     `json:"jkt"`
}

type grantRef struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// grantAnswer is Straza's answer to POST /v1/grants, or its error.
type grantAnswer struct {
	Grant string `json:"grant"`
	Aud   string `json:"aud"`
	Repo  string `json:"repo"`
	Srv   string `json:"srv"`
	Exp   string `json:"exp"`
	Error string `json:"error"`
}

// ask asks Straza for a grant.
func (h *holder) ask(req grantRequest) (grantAnswer, int) {
	b, _ := json.Marshal(req)
	resp, err := http.Post(h.straza+"/v1/grants", "application/json", bytes.NewReader(b))
	if err != nil {
		return grantAnswer{Error: "the holder could not reach Straza"}, http.StatusBadGateway
	}
	defer resp.Body.Close()
	var a grantAnswer
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return grantAnswer{Error: "Straza answered " + resp.Status}, http.StatusBadGateway
	}
	if resp.StatusCode != http.StatusOK {
		a.Grant = ""
	}
	return a, resp.StatusCode
}

// proof is a DPoP proof for one request with grant.
func (h *holder) proof(method, htu, grant string) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	ath := sha256.Sum256([]byte(grant))
	claims, _ := json.Marshal(map[string]any{
		"jti": b64.EncodeToString(jti), "htm": method, "htu": htu,
		"iat": time.Now().Unix(), "ath": b64.EncodeToString(ath[:]),
	})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: h.key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(claims)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

// commands reads the ref updates at the start of a receive-pack request:
// pkt-lines "old new ref", the first with capabilities after a NUL, up to a
// flush. A push with none, git's probe, has none.
func commands(body []byte) ([]grantRef, error) {
	refs := []grantRef{}
	for len(body) >= 4 {
		n, err := strconv.ParseUint(string(body[:4]), 16, 16)
		if err != nil {
			return nil, errors.New("not a pkt-line")
		}
		if n == 0 {
			return refs, nil
		}
		if n < 4 || int(n) > len(body) {
			return nil, errors.New("a pkt-line is cut short")
		}
		line := string(body[4:n])
		body = body[n:]
		line, _, _ = strings.Cut(line, "\x00")
		f := strings.Fields(line)
		if len(f) != 3 || f[0] == "shallow" {
			continue
		}
		refs = append(refs, grantRef{Old: f[0], New: f[1], Ref: f[2]})
	}
	return refs, nil
}

func refsText(refs []grantRef) string {
	var parts []string
	for _, r := range refs {
		parts = append(parts, fmt.Sprintf("%s %s..%s", r.Ref, short(r.Old), short(r.New)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, ", ")
}

func short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// plain answers with one line, which git prints as "remote: ...".
func plain(w http.ResponseWriter, status int, line string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, line+"\n")
}
