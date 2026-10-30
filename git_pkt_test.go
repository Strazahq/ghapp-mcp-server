package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

var (
	zeroID = strings.Repeat("0", 40)
	oidA   = strings.Repeat("a", 40)
	oidB   = strings.Repeat("b", 40)
	oidC   = strings.Repeat("c", 40)
)

const testCaps = "report-status side-band-64k quiet object-format=sha1 agent=git/2.47.3"

// pkt is one pkt-line holding s.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushHead is the command section of a push: the first command carries
// caps, then a flush.
func pushHead(caps string, cmds ...string) string {
	var b strings.Builder
	for i, c := range cmds {
		if i == 0 && caps != "" {
			c += "\x00" + caps
		}
		b.WriteString(pkt(c + "\n"))
	}
	b.WriteString("0000")
	return b.String()
}

func cmd(old, new, ref string) string { return old + " " + new + " " + ref }

func TestReadPushRequestRefs(t *testing.T) {
	head := pushHead(testCaps,
		cmd(zeroID, oidA, "refs/heads/new"),
		cmd(oidA, oidB, "refs/heads/main"),
		cmd(oidB, oidC, "refs/heads/feature/x"))
	pack := "PACK\x00\x00\x00\x02rest of the pack"
	r := strings.NewReader(head + pack)
	req, err := readPushRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	want := []pushCommand{
		{Old: zeroID, New: oidA, Ref: "refs/heads/new"},
		{Old: oidA, New: oidB, Ref: "refs/heads/main"},
		{Old: oidB, New: oidC, Ref: "refs/heads/feature/x"},
	}
	if !reflect.DeepEqual(req.Commands, want) {
		t.Errorf("commands = %+v", req.Commands)
	}
	for _, c := range strings.Fields(testCaps) {
		if !req.Caps[c] {
			t.Errorf("capability %s missing from %v", c, req.Caps)
		}
	}
	if string(req.Head) != head {
		t.Errorf("head = %q, want exactly the bytes of the command section", req.Head)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != pack {
		t.Errorf("read past the command section: left %q", rest)
	}
	if reasons := req.refusals(); reasons != nil {
		t.Errorf("refusals = %v, want none", reasons)
	}
}

func TestReadPushRequestForms(t *testing.T) {
	sha256Old, sha256New := strings.Repeat("1", 64), strings.Repeat("2", 64)
	for _, tc := range []struct {
		name string
		body string
		refs int
		opts []string
	}{
		{"the probe git sends before a large push", "0000", 0, nil},
		{"shallow lines first", pkt("shallow "+oidC) + pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main")), 1, nil},
		{"no line feeds", pkt(cmd(oidA, oidB, "refs/heads/main")+"\x00report-status") + "0000", 1, nil},
		{"no capabilities", pkt(cmd(oidA, oidB, "refs/heads/main")) + "0000", 1, nil},
		{"sha256 ids", pushHead(testCaps, cmd(sha256Old, sha256New, "refs/heads/main")), 1, nil},
		{"push-options asked, none sent", pushHead("report-status push-options", cmd(oidA, oidB, "refs/heads/main")) + "0000", 1, nil},
		{"push options", pushHead("report-status push-options", cmd(oidA, oidB, "refs/heads/main")) + pkt("ci.skip\n") + pkt("merge_request.create") + "0000", 1, []string{"ci.skip", "merge_request.create"}},
		{"push-options with a value", pushHead("report-status push-options=1", cmd(oidA, oidB, "refs/heads/main")) + pkt("ci.skip") + "0000", 1, []string{"ci.skip"}},
		{"upper-case pkt length", "006D" + cmd(oidA, oidB, "refs/heads/maintenance") + "\n" + "0000", 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := readPushRequest(strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if len(req.Commands) != tc.refs || !reflect.DeepEqual(req.Options, tc.opts) || string(req.Head) != tc.body {
				t.Errorf("got %d refs, options %q, head %q", len(req.Commands), req.Options, req.Head)
			}
		})
	}
}

func TestPushRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"a delete", pushHead(testCaps, cmd(oidA, zeroID, "refs/heads/old")), []string{reasonDelete}},
		{"a tag", pushHead(testCaps, cmd(zeroID, oidA, "refs/tags/v1.0")), []string{reasonTag}},
		{"a tag delete", pushHead(testCaps, cmd(oidA, zeroID, "refs/tags/v1.0")), []string{reasonDelete}},
		{"a note", pushHead(testCaps, cmd(zeroID, oidA, "refs/notes/commits")), []string{reasonNotBranch}},
		{"a pull request head", pushHead(testCaps, cmd(oidA, oidB, "refs/pull/1/head")), []string{reasonNotBranch}},
		{"HEAD", pushHead(testCaps, cmd(oidA, oidB, "HEAD")), []string{reasonNotBranch}},
		{"refs/heads itself", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/")), []string{reasonBranchName}},
		{"a dot-dot branch", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/a..b")), []string{reasonBranchName}},
		{"a .lock branch", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/x.lock")), []string{reasonBranchName}},
		{"a hidden part", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/x/.y")), []string{reasonBranchName}},
		{"a colon", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/a:b")), []string{reasonBranchName}},
		{"one tag among branches", pushHead(testCaps,
			cmd(oidA, oidB, "refs/heads/main"),
			cmd(zeroID, oidC, "refs/tags/v2"),
			cmd(oidB, oidC, "refs/heads/dev")),
			[]string{reasonOtherRefused, reasonTag, reasonOtherRefused}},
		{"a push option", pushHead("report-status push-options", cmd(oidA, oidB, "refs/heads/main")) + pkt("ci.skip") + "0000",
			[]string{reasonPushOptions}},
		{"a push option, the capability with a value", pushHead("report-status push-options=1", cmd(oidA, oidB, "refs/heads/main")) + pkt("ci.skip") + "0000",
			[]string{reasonPushOptions}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := readPushRequest(strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := req.refusals(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("refusals = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadPushRequestMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want error // nil: any *pushMalformed
	}{
		{"empty body", "", nil},
		{"cut in a length", "00", nil},
		{"cut in a line", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main"))[:30], nil},
		{"no flush", pkt(cmd(oidA, oidB, "refs/heads/main")), nil},
		{"length not hex", "zzzz", nil},
		{"length with a sign", "+0a1", nil},
		{"a delimiter packet", "0001", nil},
		{"an empty packet", "0004", nil},
		{"a length over the maximum", "fff1", nil},
		{"two fields", pushHead("", oidA+" refs/heads/main"), nil},
		{"four fields", pushHead("", cmd(oidA, oidB, "refs/heads/main")+" x"), nil},
		{"upper-case ids", pushHead("", cmd(strings.ToUpper(oidA), oidB, "refs/heads/main")), nil},
		{"short id", pushHead("", cmd(oidA[:39], oidB, "refs/heads/main")), nil},
		{"mixed id lengths", pushHead("", cmd(oidA, strings.Repeat("b", 64), "refs/heads/main")), nil},
		{"zero to zero", pushHead("", cmd(zeroID, zeroID, "refs/heads/main")), nil},
		{"a NUL on a later line", pushHead(testCaps, cmd(oidA, oidB, "refs/heads/main"), cmd(oidA, oidB, "refs/heads/x")+"\x00more"), nil},
		{"a control character in a ref", pushHead("", cmd(oidA, oidB, "refs/heads/a\x1bb")), nil},
		{"a line separator in a ref", pushHead("", cmd(oidA, oidB, "refs/heads/a\u2028b")), nil},
		{"invalid UTF-8 in a ref", pushHead("", cmd(oidA, oidB, "refs/heads/a\xffb")), nil},
		{"a bad shallow line", pkt("shallow nope") + "0000", nil},
		{"a signed push", pkt("push-cert\x00report-status") + pkt("certificate version 0.1\n") + "0000", errPushSigned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readPushRequest(strings.NewReader(tc.body))
			var bad *pushMalformed
			switch {
			case err == nil:
				t.Fatal("parsed")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("error = %v, want %v", err, tc.want)
			case tc.want == nil && !errors.As(err, &bad):
				t.Errorf("error = %v, want a malformed request", err)
			}
		})
	}
}

// The caps: 1000 refs pass and 1001 do not, and a command section over
// 1 MiB is refused without reading past the cap.
func TestReadPushRequestCaps(t *testing.T) {
	refs := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = cmd(oidA, oidB, "refs/heads/b"+strconv.Itoa(i))
		}
		return out
	}
	req, err := readPushRequest(strings.NewReader(pushHead(testCaps, refs(1000)...)))
	if err != nil || len(req.Commands) != 1000 {
		t.Fatalf("1000 refs: %d commands, %v", len(req.Commands), err)
	}
	if _, err := readPushRequest(strings.NewReader(pushHead(testCaps, refs(1001)...))); !errors.Is(err, errPushTooManyRefs) {
		t.Errorf("1001 refs: error = %v, want %v", err, errPushTooManyRefs)
	}

	// 20 lines of 64 KiB each, valid branch names, fewer than 1000 refs.
	long := make([]string, 20)
	for i := range long {
		name := "refs/heads/" + strconv.Itoa(i) + strings.Repeat("x", pktMaxLen-4-len("refs/heads/")-100)
		long[i] = cmd(oidA, oidB, name)
	}
	r := &countReader{r: strings.NewReader(pushHead("", long...))}
	if _, err := readPushRequest(r); !errors.Is(err, errPushHeadTooLarge) {
		t.Errorf("a 1.3 MiB command section: error = %v, want %v", err, errPushHeadTooLarge)
	}
	if r.n > pushHeadMax {
		t.Errorf("read %d bytes, past the %d byte cap", r.n, pushHeadMax)
	}
}

type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// readPkts splits pkt-lines, a flush as "".
func readPkts(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for len(b) > 0 {
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		if err != nil {
			t.Fatalf("bad pkt-line at %q", b)
		}
		if n == 0 {
			out = append(out, "")
			b = b[4:]
			continue
		}
		out = append(out, string(b[4:n]))
		b = b[n:]
	}
	return out
}

func TestReportStatus(t *testing.T) {
	cmds := []string{cmd(oidA, oidB, "refs/heads/main"), cmd(zeroID, oidC, "refs/tags/v1")}
	wantReport := []string{"unpack ok\n", "ng refs/heads/main " + reasonOtherRefused + "\n", "ng refs/tags/v1 " + reasonTag + "\n", ""}

	plain, _ := readPushRequest(strings.NewReader(pushHead("report-status", cmds...)))
	if got := readPkts(t, plain.reportStatus(plain.refusals())); !reflect.DeepEqual(got, wantReport) {
		t.Errorf("report = %q, want %q", got, wantReport)
	}

	for _, caps := range []string{"report-status side-band-64k", "report-status-v2 side-band"} {
		sb, _ := readPushRequest(strings.NewReader(pushHead(caps, cmds...)))
		pkts := readPkts(t, sb.reportStatus(sb.refusals()))
		if pkts[len(pkts)-1] != "" {
			t.Fatalf("%s: no closing flush in %q", caps, pkts)
		}
		var band1 bytes.Buffer
		for _, p := range pkts[:len(pkts)-1] {
			if len(p) > 1000-4 || p[0] != 1 {
				t.Fatalf("%s: packet %q is not band 1 within 1000 bytes", caps, p)
			}
			band1.WriteString(p[1:])
		}
		if got := readPkts(t, band1.Bytes()); !reflect.DeepEqual(got, wantReport) {
			t.Errorf("%s: report in band 1 = %q, want %q", caps, got, wantReport)
		}
	}

	// A long report spans several side-band packets.
	var many []string
	for i := range 40 {
		many = append(many, cmd(oidA, zeroID, "refs/heads/branch-with-a-long-name-"+strconv.Itoa(i)))
	}
	sb, _ := readPushRequest(strings.NewReader(pushHead("report-status side-band", many...)))
	if pkts := readPkts(t, sb.reportStatus(sb.refusals())); len(pkts) < 4 {
		t.Errorf("40 refs fit in %d packets", len(pkts))
	}
}
