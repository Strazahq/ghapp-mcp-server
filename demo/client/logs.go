package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// logEntry is one line of a log: its message and its attributes, nested
// groups flattened as "caller.kind".
type logEntry struct {
	msg   string
	attrs map[string]string
	raw   string
}

func (e logEntry) get(key string) string { return e.attrs[key] }

// readLog reads the lines of a log file from byte offset from on. ghapp's
// log mixes JSON lines, from its audit logger, with text lines from Go's log
// and slog packages; both are read.
func readLog(path string, from int64) ([]logEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	var out []logEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if e, ok := parseLogLine(sc.Text()); ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// logSize is the current size of a log, an offset for readLog.
func logSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func parseLogLine(line string) (logEntry, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			return logEntry{}, false
		}
		e := logEntry{attrs: map[string]string{}, raw: line}
		flatten("", m, e.attrs)
		e.msg = e.attrs["msg"]
		if e.msg == "" {
			e.msg = e.attrs["action"]
		}
		return e, true
	}
	// Go's log: "2026/10/18 08:00:00 INFO msg key=value ...".
	f := strings.SplitN(line, " ", 4)
	if len(f) < 4 {
		return logEntry{}, false
	}
	switch f[2] {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return logEntry{msg: strings.Join(f[2:], " "), attrs: map[string]string{}, raw: line}, true
	}
	msg, attrs := parseText(f[3])
	return logEntry{msg: msg, attrs: attrs, raw: line}, true
}

// flatten turns nested JSON into dotted keys, and lists and maps of values
// into their compact JSON.
func flatten(prefix string, m map[string]any, out map[string]string) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch v := v.(type) {
		case map[string]any:
			if k == "caller" {
				flatten(key, v, out)
				continue
			}
			b, _ := json.Marshal(v)
			out[key] = string(b)
		case string:
			out[key] = v
		case float64:
			out[key] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			b, _ := json.Marshal(v)
			out[key] = string(b)
		}
	}
}

// parseText reads slog's text form: a message, then key=value pairs whose
// value may be quoted.
func parseText(s string) (string, map[string]string) {
	attrs := map[string]string{}
	var words []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		j := i
		for j < len(s) && s[j] != ' ' && s[j] != '=' && s[j] != '"' {
			j++
		}
		if j < len(s) && s[j] == '=' && j > i {
			key := s[i:j]
			j++
			if j < len(s) && s[j] == '"' {
				k := j + 1
				for k < len(s) && s[k] != '"' {
					if s[k] == '\\' {
						k++
					}
					k++
				}
				end := min(k+1, len(s))
				v, err := strconv.Unquote(s[j:end])
				if err != nil {
					v = s[j:end]
				}
				attrs[key] = v
				i = end
				continue
			}
			k := j
			for k < len(s) && s[k] != ' ' {
				k++
			}
			attrs[key] = s[j:k]
			i = k
			continue
		}
		k := i
		for k < len(s) && s[k] != ' ' {
			k++
		}
		if len(attrs) == 0 {
			words = append(words, s[i:k])
		}
		i = k
	}
	return strings.Join(words, " "), attrs
}

// show is a log entry on one line: its message and the attributes named,
// in that order, those it has.
func (e logEntry) show(keys ...string) string {
	var parts []string
	if e.msg != "" {
		parts = append(parts, e.msg)
	}
	for _, k := range keys {
		if v, ok := e.attrs[k]; ok {
			if v == "" {
				v = `""`
			}
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// filter keeps the entries with msg whose attributes hold every pair of
// want.
func filter(entries []logEntry, msg string, want map[string]string) []logEntry {
	var out []logEntry
	for _, e := range entries {
		if e.msg != msg {
			continue
		}
		ok := true
		for k, v := range want {
			if e.attrs[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	return out
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// permsText turns ghapp's perms, JSON or Go's map form, into "a: read, b: write".
func permsText(s string) string {
	var m map[string]string
	if json.Unmarshal([]byte(s), &m) == nil {
		var parts []string
		for _, k := range sortedKeys(m) {
			parts = append(parts, fmt.Sprintf("%s: %s", k, m[k]))
		}
		return strings.Join(parts, ", ")
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "map["), "]")
	var parts []string
	for _, kv := range strings.Fields(inner) {
		k, v, _ := strings.Cut(kv, ":")
		parts = append(parts, k+": "+v)
	}
	return strings.Join(parts, ", ")
}
