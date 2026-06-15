package main

// Terminal rendering for `tracebox run`. This file owns ALL presentation: the
// status line, the bordered OUTPUT/STDERR boxes, and the dimmed metadata line.
// It is a pure view over the decoded runResponse — no HTTP, no sandbox logic.
//
// Color and box-drawing are used only on a color-capable TTY. When stdout is
// not a terminal (piped to a file, a pipeline) or NO_COLOR is set, renderRun
// falls back to a plain-text equivalent with the same structure but no escapes
// or box characters, so redirected output stays clean and grep-friendly.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// ANSI escape sequences. Kept local and tiny so the CLI needs no color library.
const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiCyan   = "\033[36m"
	ansiCyanB  = "\033[1;36m"
	ansiRed    = "\033[31m"
	ansiRedB   = "\033[1;31m"
	ansiGreenB = "\033[1;32m"
)

// maxBoxWidth caps how wide the OUTPUT/STDERR boxes grow with their content.
const maxBoxWidth = 80

// styler applies ANSI styling, but only when enabled. A disabled styler is the
// signal renderRun uses to pick the plain-text layout.
type styler struct{ enabled bool }

// newStyler enables styling only for a color-capable terminal: f must be a TTY,
// NO_COLOR must be unset, and TERM must not be "dumb".
func newStyler(f *os.File) styler {
	return styler{enabled: colorEnabled(f)}
}

func colorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// wrap returns text surrounded by the given ANSI code (and a reset), or text
// unchanged when styling is disabled or there is nothing to color.
func (s styler) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return code + text + ansiReset
}

// renderRun writes the full rendering of a completed run to w.
func renderRun(w io.Writer, s styler, rr *runResponse, verbose bool) {
	var test *testResult
	if len(rr.Tests) > 0 {
		test = &rr.Tests[0]
	}
	if s.enabled {
		renderStyled(w, s, rr, test, verbose)
	} else {
		renderPlain(w, rr, test, verbose)
	}
}

// renderStyled is the colored, box-drawn layout used on a TTY.
func renderStyled(w io.Writer, s styler, rr *runResponse, test *testResult, verbose bool) {
	expl := explain(rr.Status)
	ran := ranStatuses[rr.Status]

	// Status line — explicitly names the sandbox so the core value (this ran
	// inside Tracebox, not on the host) is always visible.
	mark, markCode := "✓", ansiGreenB
	if !ran {
		mark, markCode = "✗", ansiRedB
	}
	line := s.wrap(markCode, mark+" "+expl.label+" in Tracebox sandbox")
	switch {
	case ran:
		line += "  " + s.wrap(ansiDim, "(no expected output provided)")
	case !verbose:
		line += "  " + s.wrap(ansiDim, "(run --verbose for details)")
	}
	fmt.Fprintln(w, line)

	// Long explanation only on --verbose; the default stays short.
	if verbose {
		fmt.Fprintln(w)
		for _, l := range wrapText(expl.detail, 76) {
			fmt.Fprintln(w, s.wrap(ansiDim, "  "+l))
		}
	}

	// On a build failure the compiler output is the most useful thing to show.
	if out := compileOutput(rr); out != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, s.box("COMPILE ERRORS", ansiRed, ansiRedB, out))
	}
	if test != nil && test.Stdout != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, s.box("OUTPUT", ansiCyan, ansiCyanB, test.Stdout))
	}
	if test != nil && test.Stderr != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, s.box("STDERR", ansiRed, ansiRedB, test.Stderr))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, s.wrap(ansiDim, metadataLine(rr, test, " · ", false)))
}

// renderPlain is the escape-free, box-free fallback for non-TTY / NO_COLOR.
func renderPlain(w io.Writer, rr *runResponse, test *testResult, verbose bool) {
	expl := explain(rr.Status)

	head := expl.label + " in Tracebox sandbox"
	if ranStatuses[rr.Status] {
		head += " (no expected output provided)"
	}
	fmt.Fprintln(w, head)

	if verbose {
		fmt.Fprintln(w)
		fmt.Fprintln(w, expl.detail)
	}

	if out := compileOutput(rr); out != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "=== COMPILE ERRORS ===")
		fmt.Fprintln(w, strings.TrimRight(out, "\n"))
	}
	if test != nil && test.Stdout != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "=== OUTPUT ===")
		fmt.Fprintln(w, strings.TrimRight(test.Stdout, "\n"))
	}
	if test != nil && test.Stderr != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "=== STDERR ===")
		fmt.Fprintln(w, strings.TrimRight(test.Stderr, "\n"))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, metadataLine(rr, test, "  ", true))
}

// box renders content inside a labeled, colored border:
//
//	┌─ LABEL ──────────
//	│ line one
//	│ line two
//	└──────────────────
//
// The border and label are colored; content lines are left in the terminal's
// default color so the program's own output is never recolored. Returns a
// string ending in a newline.
func (s styler) box(label, borderCode, labelCode, content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

	// Width is driven by the longest content line (plus the "│ " gutter), with a
	// floor that keeps the label readable and a ceiling so wide output doesn't
	// draw an enormous rule.
	width := len([]rune(label)) + 8
	for _, l := range lines {
		if n := len([]rune(l)) + 2; n > width {
			width = n
		}
	}
	if width > maxBoxWidth {
		width = maxBoxWidth
	}

	dashes := max(width-(len([]rune(label))+4), 1) // "┌─ " + label + " "

	var b strings.Builder
	b.WriteString(s.wrap(borderCode, "┌─"))
	b.WriteString(" ")
	b.WriteString(s.wrap(labelCode, label))
	b.WriteString(" ")
	b.WriteString(s.wrap(borderCode, strings.Repeat("─", dashes)))
	b.WriteString("\n")
	for _, l := range lines {
		b.WriteString(s.wrap(borderCode, "│"))
		b.WriteString(" ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString(s.wrap(borderCode, "└"+strings.Repeat("─", width-1)))
	b.WriteString("\n")
	return b.String()
}

// metadataLine builds the dimmed run-metadata line: run_id, backend, exit code,
// duration, and (when known) peak memory. styled uses bare "key value" parts
// joined by sep; plain uses "key: value". The backend is shown so it's clear
// the code ran inside an isolated sandbox (nsjail/gvisor), not on the host.
func metadataLine(rr *runResponse, test *testResult, sep string, labeled bool) string {
	kv := func(key, val string) string {
		if labeled {
			return key + ": " + val
		}
		return key + " " + val
	}

	parts := []string{kv("run_id", rr.RunID)}
	if rr.Backend != "" {
		// The backend reads naturally on its own in the styled line; label it in
		// plain mode for clarity.
		if labeled {
			parts = append(parts, "backend: "+rr.Backend)
		} else {
			parts = append(parts, rr.Backend)
		}
	}
	if test != nil && test.Status != "not_executed" {
		parts = append(parts, kv("exit", fmt.Sprintf("%d", test.ExitCode)))
	}
	if d := durationMs(rr, test); d >= 0 {
		parts = append(parts, kv("duration", fmt.Sprintf("%dms", d)))
	}
	if test != nil && test.MemoryPeakKB > 0 {
		parts = append(parts, kv("memory", fmt.Sprintf("%dKB", test.MemoryPeakKB)))
	}
	return strings.Join(parts, sep)
}

// durationMs picks the most relevant duration to report: the test's own run
// time, falling back to the build time on a compile failure (no test ran).
func durationMs(rr *runResponse, test *testResult) int64 {
	if test != nil && test.DurationMs > 0 {
		return test.DurationMs
	}
	if rr.Build != nil {
		return rr.Build.DurationMs
	}
	if test != nil {
		return test.DurationMs
	}
	return -1
}

// compileOutput returns the build stdout/stderr when the build failed, else "".
func compileOutput(rr *runResponse) string {
	if rr.Build != nil && rr.Build.Status != "build_ok" {
		return joinOutput(rr.Build.Stdout, rr.Build.Stderr)
	}
	return ""
}

// wrapText greedily word-wraps s to at most width columns per line.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	lines := make([]string, 0, 4)
	cur := words[0]
	for _, word := range words[1:] {
		if len(cur)+1+len(word) > width {
			lines = append(lines, cur)
			cur = word
			continue
		}
		cur += " " + word
	}
	return append(lines, cur)
}
