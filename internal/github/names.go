package github

import (
	"net/url"
	"strconv"
	"strings"
)

// maxNameLen bounds an owner, organization or repository name. GitHub's own
// limits are lower; this one only keeps paths and error texts short.
const maxNameLen = 100

// maxQuoted bounds how much of a refused argument an error text repeats.
const maxQuoted = 100

// RepoPath returns the REST path of the repository fullName,
// "/repos/{owner}/{name}", followed by segments. Each part is escaped with
// url.PathEscape, so a "/" inside a segment stays in that segment. It
// refuses, as ErrInvalidInput, a fullName that is not exactly owner/name and
// a segment that is empty, "." or "..".
func RepoPath(fullName string, segments ...string) (string, error) {
	owner, name, err := splitFullName(fullName)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("/repos/")
	b.WriteString(url.PathEscape(owner))
	b.WriteString("/")
	b.WriteString(url.PathEscape(name))
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", invalidf("the path segment %s under %s is empty or a dot segment. Name the object, such as pulls or a pull request number", quote(seg), fullName)
		}
		b.WriteString("/")
		b.WriteString(url.PathEscape(seg))
	}
	return b.String(), nil
}

// splitFullName splits fullName into owner and name. It refuses anything
// that is not exactly "owner/name" with both parts valid GitHub names, and
// any "..", before a call can reach GitHub.
func splitFullName(fullName string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", invalidf("the repository %s is not owner/name. Name it as GitHub spells it, such as Strazahq/pay-api", quote(fullName))
	}
	if strings.Contains(fullName, "..") {
		return "", "", invalidf("the repository %s holds \"..\". Name it as GitHub spells it, such as Strazahq/pay-api", quote(fullName))
	}
	if err := checkOwner("repository owner", owner); err != nil {
		return "", "", err
	}
	if !validName(name, true) {
		return "", "", invalidf("the repository name %s is not a GitHub repository name. It has 1 to %d letters, digits, '.', '-' or '_'", quote(name), maxNameLen)
	}
	return owner, name, nil
}

// checkOwner checks a user or organization login. what names it in the
// error, such as "organization".
func checkOwner(what, login string) error {
	if !validName(login, false) {
		return invalidf("the %s %s is not a GitHub login. It has 1 to %d letters, digits, '-' or '_'", what, quote(login), maxNameLen)
	}
	return nil
}

// validName reports whether s is 1 to maxNameLen ASCII letters, digits, '-'
// or '_', and '.' when dots is set. A name of only dots, or one with "..", is
// never valid.
func validName(s string, dots bool) bool {
	if s == "" || len(s) > maxNameLen || strings.Contains(s, "..") || s == "." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
		case c == '.' && dots:
		default:
			return false
		}
	}
	return true
}

// checkPath checks a REST path relative to the API base. It must start with
// a single "/", hold only printable ASCII without '?', '#' or '\', and have no
// empty or dot segment, escaped or not. Query parameters go in the query
// argument and each segment is escaped with url.PathEscape.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return invalidf("the path %s does not start with a single /. Give it relative to the API base, such as /repos/Strazahq/pay-api/pulls", quote(path))
	}
	for i := 0; i < len(path); i++ {
		if c := path[i]; c <= ' ' || c >= 0x7f || c == '?' || c == '#' || c == '\\' {
			return invalidf("the path %s holds a character a GitHub path may not. Put query parameters in the query, and escape each segment with url.PathEscape", quote(path))
		}
	}
	for _, seg := range strings.Split(path[1:], "/") {
		if seg == "" {
			return invalidf("the path %s has an empty segment. Leave out the doubled or trailing /", quote(path))
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return invalidf("the path %s holds a broken %% escape. Escape each segment with url.PathEscape", quote(path))
		}
		if strings.ContainsFunc(decoded, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return invalidf("the path %s holds an escaped control character. Name the object directly", quote(path))
		}
		// An escaped "/" or "\" could turn into a dot segment further on.
		for _, part := range strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == "." || part == ".." {
				return invalidf("the path %s has a dot segment, which could leave the object it names. Name the object directly", quote(path))
			}
		}
	}
	return nil
}

// quote quotes a refused argument for an error text, cut to maxQuoted bytes.
func quote(s string) string {
	if len(s) > maxQuoted {
		return strconv.Quote(s[:maxQuoted]) + "..."
	}
	return strconv.Quote(s)
}
