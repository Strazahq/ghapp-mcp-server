package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The command section of a push, as git's pack protocol sends it to
// receive-pack [doc git gitprotocol-pack, "Reference Update Request and
// Packfile Transfer"]:
//
//	update-requests = *shallow ( command-list | push-cert )
//	shallow         = PKT-LINE("shallow" SP obj-id)
//	command-list    = PKT-LINE(command NUL capability-list) *PKT-LINE(command) flush-pkt
//	command         = old-id SP new-id SP name
//	push-options    = *PKT-LINE(push-option) flush-pkt, when the push-options capability was asked for
//
// ghapp reads this section before anything goes to GitHub, and refuses what
// it does not allow: deletes, refs outside refs/heads/, and push options.
// The pack that follows is never read, only streamed on.

const (
	pktMaxLen   = 65520   // git's LARGE_PACKET_MAX, the 4 length bytes included
	pushHeadMax = 1 << 20 // bytes of the command section ghapp reads
	pushMaxRefs = 1000
	branchRefs  = "refs/heads/"
	// sidebandData is the most data one side-band packet carries: 1000
	// bytes for side-band, minus the length and the band byte, so it fits
	// side-band-64k too.
	sidebandData = 1000 - 5
)

// Errors of the command section. Their text goes to git, which prints it.
var (
	errPushHeadTooLarge = fmt.Errorf("the ref updates of this push are larger than %d bytes, ghapp's limit", pushHeadMax)
	errPushTooManyRefs  = fmt.Errorf("this push updates more than %d refs, ghapp's limit. Push fewer refs at a time", pushMaxRefs)
	errPushSigned       = errors.New("signed pushes are not supported through ghapp")
)

// pushMalformed is a command section ghapp cannot read.
type pushMalformed struct{ why string }

func (e *pushMalformed) Error() string { return "the push request is not valid git protocol: " + e.why }

func malformed(format string, args ...any) error {
	return &pushMalformed{why: fmt.Sprintf(format, args...)}
}

// pushCommand is one ref update.
type pushCommand struct {
	Old, New, Ref string
}

func (c pushCommand) isDelete() bool { return isZeroID(c.New) }

// String is the command for a log line: "ref old new".
func (c pushCommand) String() string { return c.Ref + " " + c.Old + " " + c.New }

// pushRequest is the command section of a push.
type pushRequest struct {
	Commands []pushCommand
	Caps     map[string]bool // the capabilities on the first command, "agent=..." kept whole
	Options  []string        // push options
	// Head is every byte read, exactly as git sent it. It goes upstream
	// first, followed by the rest of the body.
	Head []byte
}

// pktReader reads pkt-lines and keeps every byte it read.
type pktReader struct {
	r    io.Reader
	head []byte
}

// next returns the payload of the next pkt-line, or flush for a flush-pkt.
func (p *pktReader) next() (payload []byte, flush bool, err error) {
	var hdr [4]byte
	if len(p.head)+len(hdr) > pushHeadMax {
		return nil, false, errPushHeadTooLarge
	}
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return nil, false, readErr(err)
	}
	p.head = append(p.head, hdr[:]...)
	n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	switch {
	case err != nil:
		return nil, false, malformed("a pkt-line length is not hex")
	case n == 0:
		return nil, true, nil
	case n <= 4 || n > pktMaxLen:
		// 0001 to 0003 are protocol v2 markers, and 0004 an empty line:
		// none belongs in a push's command section.
		return nil, false, malformed("a pkt-line of length %d", n)
	}
	size := int(n) - len(hdr)
	if len(p.head)+size > pushHeadMax {
		return nil, false, errPushHeadTooLarge
	}
	start := len(p.head)
	p.head = append(p.head, make([]byte, size)...)
	if _, err := io.ReadFull(p.r, p.head[start:]); err != nil {
		return nil, false, readErr(err)
	}
	return p.head[start:], false, nil
}

// readErr keeps a body error, such as the size cap or a timeout, and calls
// a short body malformed.
func readErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed("the request ends inside the ref updates")
	}
	return err
}

// readPushRequest reads the command section of a push from r, and the push
// options when the client asked for them. It reads nothing beyond them.
func readPushRequest(r io.Reader) (pushRequest, error) {
	pr := &pktReader{r: r}
	req := pushRequest{Caps: map[string]bool{}}
	for {
		line, flush, err := pr.next()
		if err != nil {
			return pushRequest{}, err
		}
		if flush {
			break
		}
		s := strings.TrimSuffix(string(line), "\n")
		if len(req.Commands) == 0 {
			if oid, ok := strings.CutPrefix(s, "shallow "); ok {
				if !isObjectID(oid) {
					return pushRequest{}, malformed("a shallow line without an object id")
				}
				continue
			}
			if strings.HasPrefix(s, "push-cert") {
				return pushRequest{}, errPushSigned
			}
			var caps string
			s, caps, _ = strings.Cut(s, "\x00")
			for _, c := range strings.Fields(caps) {
				// git takes a capability with a value by its name as well,
				// such as push-options=1 for push-options, so both count.
				name, _, _ := strings.Cut(c, "=")
				req.Caps[c], req.Caps[name] = true, true
			}
		}
		if len(req.Commands) == pushMaxRefs {
			return pushRequest{}, errPushTooManyRefs
		}
		cmd, err := parseCommand(s)
		if err != nil {
			return pushRequest{}, err
		}
		req.Commands = append(req.Commands, cmd)
	}
	if req.Caps["push-options"] && len(req.Commands) > 0 {
		for {
			line, flush, err := pr.next()
			if err != nil {
				return pushRequest{}, err
			}
			if flush {
				break
			}
			opt := strings.TrimSuffix(string(line), "\n")
			if !printable(opt) {
				return pushRequest{}, malformed("a push option with control characters")
			}
			req.Options = append(req.Options, opt)
		}
	}
	req.Head = pr.head
	return req, nil
}

// parseCommand reads "old-id SP new-id SP name".
func parseCommand(s string) (pushCommand, error) {
	f := strings.Split(s, " ")
	if len(f) != 3 || !isObjectID(f[0]) || !isObjectID(f[1]) || len(f[0]) != len(f[1]) {
		return pushCommand{}, malformed("a ref update is not \"old-id new-id ref\"")
	}
	if f[2] == "" || !printable(f[2]) {
		return pushCommand{}, malformed("a ref name is empty or holds control characters")
	}
	if isZeroID(f[0]) && isZeroID(f[1]) {
		return pushCommand{}, malformed("a ref update from zero to zero")
	}
	return pushCommand{Old: f[0], New: f[1], Ref: f[2]}, nil
}

// isObjectID reports a SHA-1 or SHA-256 object id in lower-case hex, as git
// sends them.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func isZeroID(s string) bool { return strings.Trim(s, "0") == "" }

// printable reports valid UTF-8 without control characters or the Unicode
// line and paragraph separators, any of which could break a log line or a
// status line.
func printable(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}) < 0
}

// Reasons a ref update is refused, as git prints them after the ref.
const (
	reasonDelete       = "ghapp does not delete refs"
	reasonTag          = "ghapp does not push tags, only branches under refs/heads/"
	reasonNotBranch    = "ghapp pushes only branches under refs/heads/"
	reasonBranchName   = "not a valid branch name"
	reasonPushOptions  = "ghapp does not pass push options on"
	reasonOtherRefused = "not pushed, because another ref in this push was refused"
)

// refusals returns a reason for every command when ghapp refuses the push,
// or nil when it may go upstream. A refused push goes nowhere, so the refs
// that were not refused themselves get reasonOtherRefused.
func (p pushRequest) refusals() []string {
	reasons := make([]string, len(p.Commands))
	refused := false
	for i, c := range p.Commands {
		reasons[i] = c.refusal()
		if len(p.Options) > 0 && reasons[i] == "" {
			reasons[i] = reasonPushOptions
		}
		refused = refused || reasons[i] != ""
	}
	if !refused {
		return nil
	}
	for i := range reasons {
		if reasons[i] == "" {
			reasons[i] = reasonOtherRefused
		}
	}
	return reasons
}

// refusal is why ghapp refuses this one update, or "".
func (c pushCommand) refusal() string {
	switch {
	case c.isDelete():
		return reasonDelete
	case strings.HasPrefix(c.Ref, "refs/tags/"):
		return reasonTag
	case !strings.HasPrefix(c.Ref, branchRefs):
		return reasonNotBranch
	case !validBranch(strings.TrimPrefix(c.Ref, branchRefs)):
		return reasonBranchName
	}
	return ""
}

// validBranch is a cut of git check-ref-format for the part after
// refs/heads/. GitHub checks the rest.
func validBranch(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") ||
		strings.ContainsAny(name, "~^:?*[\\") || name == "@" {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// wantsReport reports whether the client asked for report-status, so that
// a refusal can name each ref.
func (p pushRequest) wantsReport() bool {
	return p.Caps["report-status"] || p.Caps["report-status-v2"]
}

// reportStatus is receive-pack's answer that refuses every command with its
// reason: "unpack ok", then "ng <ref> <reason>" per ref, as GitHub answers
// when a pre-receive hook declines. With side-band, the report travels in
// band 1 and a flush ends the answer, as receive-pack sends it. This
// report-status form is also valid report-status-v2.
func (p pushRequest) reportStatus(reasons []string) []byte {
	var report bytes.Buffer
	writePkt(&report, []byte("unpack ok\n"))
	for i, c := range p.Commands {
		writePkt(&report, []byte("ng "+c.Ref+" "+reasons[i]+"\n"))
	}
	report.WriteString("0000")
	if !p.Caps["side-band-64k"] && !p.Caps["side-band"] {
		return report.Bytes()
	}
	var out bytes.Buffer
	for data := report.Bytes(); len(data) > 0; {
		n := min(len(data), sidebandData)
		writePkt(&out, append([]byte{1}, data[:n]...))
		data = data[n:]
	}
	out.WriteString("0000")
	return out.Bytes()
}

// writePkt writes one pkt-line with payload.
func writePkt(b *bytes.Buffer, payload []byte) {
	fmt.Fprintf(b, "%04x", len(payload)+4)
	b.Write(payload)
}
