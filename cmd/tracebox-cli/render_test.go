package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func sampleSuccess() *runResponse {
	return &runResponse{
		RunID:   "7d3f1a2b-1111-2222-3333-444455556666",
		Status:  "accepted",
		Backend: "gvisor",
		Tests: []testResult{{
			Status:       "accepted",
			Stdout:       "Hello, world!\nSum of 1..10 = 55\n",
			DurationMs:   12,
			MemoryPeakKB: 2048,
			ExitCode:     0,
		}},
	}
}

func sampleCrash() *runResponse {
	return &runResponse{
		RunID:   "7d3f1a2b-1111-2222-3333-444455556666",
		Status:  "runtime_error",
		Backend: "nsjail",
		Tests: []testResult{{
			Status:     "runtime_error",
			Stderr:     "Traceback (most recent call last):\nZeroDivisionError: division by zero\n",
			DurationMs: 5,
			ExitCode:   1,
		}},
	}
}

func TestRenderPlain_NoEscapesAndStructure(t *testing.T) {
	var buf bytes.Buffer
	renderRun(&buf, styler{enabled: false}, sampleSuccess(), false)
	out := buf.String()

	if strings.Contains(out, "\033") {
		t.Fatalf("plain output must not contain ANSI escapes:\n%q", out)
	}
	for _, want := range []string{
		"ran successfully in Tracebox sandbox",
		"=== OUTPUT ===",
		"Hello, world!",
		"backend: gvisor",
		"exit: 0",
		"duration: 12ms",
		"memory: 2048KB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRenderStyled_BoxesColorsAndSandboxName(t *testing.T) {
	var buf bytes.Buffer
	renderRun(&buf, styler{enabled: true}, sampleSuccess(), false)
	out := buf.String()

	for _, want := range []string{
		"\033[1;32m",          // green-bold success line
		"in Tracebox sandbox", // sandbox context always visible
		"┌─", "│", "└",        // box drawing
		"OUTPUT",   // box label
		"\033[36m", // cyan border
		"\033[2m",  // dimmed metadata
		"gvisor",   // backend shown
	} {
		if !strings.Contains(out, want) {
			t.Errorf("styled output missing %q", want)
		}
	}
}

func TestRenderStyled_CrashUsesRedAndStderrBox(t *testing.T) {
	var buf bytes.Buffer
	renderRun(&buf, styler{enabled: true}, sampleCrash(), false)
	out := buf.String()

	for _, want := range []string{
		"\033[1;31m✗",   // red-bold failure mark
		"STDERR",        // stderr box label
		"run --verbose", // hint when not verbose
		"exit 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("crash output missing %q", want)
		}
	}
}

func TestRenderVerbose_AddsExplanationParagraph(t *testing.T) {
	var buf bytes.Buffer
	renderRun(&buf, styler{enabled: false}, sampleSuccess(), true)
	out := buf.String()
	if !strings.Contains(out, "executed to completion inside the sandbox") {
		t.Errorf("verbose output should include the long explanation:\n%s", out)
	}
}

func TestColorEnabled_DisabledByEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stdout) {
		t.Error("NO_COLOR set should disable color")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colorEnabled(os.Stdout) {
		t.Error("TERM=dumb should disable color")
	}
}
