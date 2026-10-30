package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxResponseBytes caps how much of a GitHub answer the client reads, so an
// unexpected endpoint cannot exhaust memory.
const maxResponseBytes = 10 << 20 // 10 MiB

// maxMessageBytes caps GitHub's "message" kept on a StatusError.
const maxMessageBytes = 512

// ErrInvalidInput marks an argument the client refuses before any call to
// GitHub: a name that is not owner/name, an unsafe path, an empty token or
// empty permissions. Match it with errors.Is.
var ErrInvalidInput = errors.New("invalid input for a GitHub call")

// inputError is an argument refused before any call. Its text says what is
// wrong; it matches ErrInvalidInput.
type inputError struct{ text string }

func (e *inputError) Error() string { return e.text }

// Is makes errors.Is(err, ErrInvalidInput) hold.
func (e *inputError) Is(target error) bool { return target == ErrInvalidInput }

// invalidf returns an inputError with a formatted text.
func invalidf(format string, args ...any) error {
	return &inputError{text: fmt.Sprintf(format, args...)}
}

// unreachedError is a call that got no answer from GitHub: a refused
// connection, a timeout, a cancelled context. Its text names the method and
// the path, never the base URL. It wraps the cause, so errors.Is finds
// context.DeadlineExceeded or context.Canceled.
type unreachedError struct {
	text string
	err  error
}

func (e *unreachedError) Error() string { return e.text }

// Unwrap returns the cause.
func (e *unreachedError) Unwrap() error { return e.err }

// githubMessage is the part of GitHub's error body the client keeps.
type githubMessage struct {
	Message string `json:"message"`
	// Errors says what failed in a 422, such as "not all refs are readable".
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// text is the message, then each error's own message after a colon.
func (m githubMessage) text() string {
	var reasons []string
	for _, e := range m.Errors {
		if e.Message != "" {
			reasons = append(reasons, e.Message)
		}
	}
	if len(reasons) == 0 {
		return m.Message
	}
	return m.Message + ": " + strings.Join(reasons, "; ")
}

// callAsApp makes one call authenticated as the App, with its JWT. A 401
// drops the cached JWT, so a JWT GitHub refused is not sent again.
func (a *App) callAsApp(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
	signed, err := a.appJWT()
	if err != nil {
		return nil, err
	}
	h, err := a.call(ctx, signed, method, path, query, body, out)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusUnauthorized {
		a.dropJWT(signed)
	}
	return h, err
}

// call makes one request to GitHub with the bearer credential cred, the App
// JWT or an installation token. body, when not nil, goes as JSON. A 2xx JSON
// answer is decoded into out when out is not nil. It returns the answer's
// header, for paging. Errors name the method and the path, never cred or the
// base URL: a 404 is ErrNotFound, a 3xx is ErrRedirect, any other non-2xx a
// *StatusError.
func (a *App) call(ctx context.Context, cred, method, path string, query url.Values, body, out any) (http.Header, error) {
	// Checked first, as every error text below may quote the path.
	if cred != "" && strings.Contains(path, cred) {
		return nil, invalidf("%s: the path holds the call's credential. A token never goes in a path", method)
	}
	if err := checkPath(path); err != nil {
		return nil, err
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, invalidf("%s %s: the request body cannot be sent as JSON: %v", method, path, err)
		}
		reqBody = bytes.NewReader(b)
	}
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, invalidf("%s %s: the request cannot be built: %s", method, path, a.scrub(withoutURL(err).Error()))
	}
	req.Header.Set("Accept", mediaType)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+cred)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		cause := withoutURL(err)
		return nil, &unreachedError{
			text: fmt.Sprintf("%s %s: GitHub did not answer: %s", method, path, a.scrub(cause.Error())),
			err:  cause,
		}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &unreachedError{
			text: fmt.Sprintf("%s %s: reading GitHub's answer: %s", method, path, a.scrub(err.Error())),
			err:  err,
		}
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%s %s: GitHub's answer is larger than 10 MiB, the most this client reads. Ask for less, such as a smaller page", method, path)
	}

	switch code := resp.StatusCode; {
	case code >= 300 && code < 400:
		return nil, fmt.Errorf("%s %s: %w (%s) and it was not followed. A renamed or transferred repository answers this way: name it as GitHub spells it now",
			method, path, ErrRedirect, resp.Status)
	case code == http.StatusNotFound:
		return nil, fmt.Errorf("%s %s: %w. It does not exist, or the App's installation cannot see it", method, path, ErrNotFound)
	case code < 200 || code >= 300:
		se := &StatusError{
			Method:      method,
			Path:        path,
			StatusCode:  code,
			Status:      resp.Status,
			RateLimited: rateLimited(resp),
		}
		var msg githubMessage
		if json.Unmarshal(data, &msg) == nil {
			// The message may reach a caller, so a credential GitHub echoed
			// back is cut out.
			text := msg.text()
			if cred != "" {
				text = strings.ReplaceAll(text, cred, "[redacted]")
			}
			se.Message = oneLine(text, maxMessageBytes)
		}
		return nil, se
	}

	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("%s %s: decoding GitHub's answer: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

// rateLimited reports a primary or secondary rate limit: a 429, or a 403
// with x-ratelimit-remaining 0 or a retry-after header.
func rateLimited(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return strings.TrimSpace(resp.Header.Get("X-Ratelimit-Remaining")) == "0" ||
			resp.Header.Get("Retry-After") != ""
	}
	return false
}

// withoutURL drops the request URL from a net/http error. A *url.Error's
// text repeats the full URL, the base included, and the callers already name
// the path.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// scrub replaces the base URL and its host in an error text, so a transport
// error from the network stack does not name the GitHub API's address.
func (a *App) scrub(text string) string {
	return a.scrubber.Replace(text)
}

// oneLine makes untrusted text from GitHub safe to show: control characters
// become spaces, runs of space collapse, and it is cut to at most limit bytes
// on a rune boundary.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// hasNext reports whether a Link header names a rel="next" page. The client
// never follows the link itself: it asks for the next page number on its own
// base URL, so a Link header cannot send a token elsewhere.
func hasNext(h http.Header) bool {
	for _, v := range h.Values("Link") {
		for {
			open := strings.IndexByte(v, '<')
			if open < 0 {
				break
			}
			end := strings.IndexByte(v[open:], '>')
			if end < 0 {
				break
			}
			params := v[open+end+1:]
			v = ""
			if next := strings.IndexByte(params, '<'); next >= 0 {
				params, v = params[:next], params[next:]
			}
			for _, p := range strings.Split(params, ";") {
				name, val, ok := strings.Cut(strings.Trim(strings.TrimSpace(p), ", "), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
					continue
				}
				for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(val), `"`)) {
					if strings.EqualFold(rel, "next") {
						return true
					}
				}
			}
		}
	}
	return false
}
