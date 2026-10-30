package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// width is the transcript's line width.
const width = 78

// transcript writes the readable record of the demo, to every writer at
// once, and keeps a copy to check that no secret reached it.
type transcript struct {
	w    io.Writer
	copy bytes.Buffer
}

func (t *transcript) printf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	t.copy.WriteString(s)
	_, _ = io.WriteString(t.w, s)
}

func (t *transcript) rule(ch string) { t.printf("%s\n", strings.Repeat(ch, width)) }

// title starts the transcript.
func (t *transcript) title(text string) {
	t.rule("=")
	t.printf(" %s\n", text)
	t.rule("=")
}

// step starts one step.
func (t *transcript) step(id, text string) {
	t.printf("\n")
	t.rule("-")
	t.printf("%s. %s\n", id, text)
	t.rule("-")
}

// field prints a label and text wrapped beside it.
func (t *transcript) field(label, text string) {
	const indent = 10
	if text == "" {
		t.printf("%s\n", label)
		return
	}
	lines := wrap(text, width-indent)
	for i, l := range lines {
		if i == 0 {
			t.printf("%-*s%s\n", indent, label, l)
		} else {
			t.printf("%-*s%s\n", indent, "", l)
		}
	}
}

// block prints lines as they are, indented and marked, for command output,
// tool results and log lines. Long lines stay whole, so a log line or a
// hash reads as the program wrote it.
func (t *transcript) block(text string) {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		t.printf("%10s| (nothing)\n", "")
		return
	}
	for _, l := range strings.Split(text, "\n") {
		t.printf("%s\n", strings.TrimRight(fmt.Sprintf("%10s| %s", "", l), " "))
	}
}

// wrap breaks text into lines of at most n characters at spaces, and keeps
// line breaks already in it.
func wrap(text string, n int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= n:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return out
}
