// Command tracebox-mcp is a Model Context Protocol (MCP) server that exposes the
// Tracebox sandbox to AI agents as a single `run_code` tool.
//
// It is a thin stdio client over the Tracebox HTTP API: every tool call is
// translated into a POST /run request against the running API server. No sandbox
// logic lives here — the server must be running separately (see cmd/tracebox).
//
// Configuration:
//
//	TRACEBOX_API_URL   base URL of the Tracebox API (default http://localhost:8080)
//
// See README.md in this directory for client setup (Claude Desktop / Claude Code).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultAPIURL = "http://localhost:8080"

// httpTimeout bounds a single /run call. Compiled languages plus generous wall
// limits can take a while, so this is comfortably above the API's own limits.
const httpTimeout = 60 * time.Second

// maxOutputBytes caps each returned stream so the agent gets a useful tail
// rather than a giant dump.
const maxOutputBytes = 8 * 1024

// supportedLanguages mirrors the IDs in configs/languages.yaml.
var supportedLanguages = []string{"py3", "cpp", "c", "bash", "js", "java", "verilog"}

// runCodeInput is the tool's argument schema. The jsonschema tags are surfaced
// to the calling agent as field descriptions.
type runCodeInput struct {
	Language string `json:"language" jsonschema:"language to run; one of py3, cpp, c, bash, js, java, verilog"`
	Code     string `json:"code" jsonschema:"the full source code to execute in the sandbox"`
	Stdin    string `json:"stdin,omitempty" jsonschema:"optional standard input fed to the program"`
}

// runCodeOutput is the structured result returned to the agent. Kept concise:
// just what is needed to understand what happened.
type runCodeOutput struct {
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	Stdout        string `json:"stdout,omitempty"`
	Stderr        string `json:"stderr,omitempty"`
	CompileOutput string `json:"compile_output,omitempty"`
	DurationMs    int64  `json:"duration_ms,omitempty"`
	MemoryPeakKB  int64  `json:"memory_peak_kb,omitempty"`
}

// --- Minimal mirror of the Tracebox HTTP API request/response shapes. ---
// Defined locally so the MCP binary stays a thin HTTP client and does not pull
// in the server-side runner/sandbox packages.

type testCase struct {
	Stdin string `json:"stdin,omitempty"`
}

type runRequest struct {
	Language         string     `json:"language"`
	Source           string     `json:"source"`
	SourceFilename   string     `json:"source_filename,omitempty"`
	ArtifactFilename string     `json:"artifact_filename,omitempty"`
	Tests            []testCase `json:"tests"`
}

type buildResult struct {
	Status string `json:"status"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type testResult struct {
	Status       string `json:"status"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	DurationMs   int64  `json:"duration_ms"`
	MemoryPeakKB int64  `json:"memory_peak_kb"`
}

type runResponse struct {
	RunID  string       `json:"run_id"`
	Status string       `json:"status"`
	Build  *buildResult `json:"build,omitempty"`
	Tests  []testResult `json:"tests"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// javaClassRe extracts the (public) top-level class name so Java sources can be
// written to a matching filename, which javac requires.
var javaClassRe = regexp.MustCompile(`(?m)^\s*public\s+(?:final\s+|abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

func main() {
	apiURL := strings.TrimRight(os.Getenv("TRACEBOX_API_URL"), "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	client := &http.Client{Timeout: httpTimeout}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "tracebox",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "run_code",
		Description: "Execute untrusted source code in the Tracebox sandbox and return its " +
			"output. Supports py3, cpp, c, bash, js, java, and verilog.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runCodeInput) (*mcp.CallToolResult, runCodeOutput, error) {
		return runCode(ctx, client, apiURL, in)
	})

	log.Printf("tracebox-mcp: serving over stdio, API at %s", apiURL)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("tracebox-mcp: %v", err)
	}
}

// runCode translates a tool call into a POST /run and maps the response back.
// Tool-level failures (bad input, API unreachable, API error) are returned as
// an error CallToolResult rather than a Go error, so the agent sees the reason.
func runCode(ctx context.Context, client *http.Client, apiURL string, in runCodeInput) (*mcp.CallToolResult, runCodeOutput, error) {
	if !isSupported(in.Language) {
		return toolError("unsupported language %q; must be one of %s", in.Language, strings.Join(supportedLanguages, ", "))
	}
	if strings.TrimSpace(in.Code) == "" {
		return toolError("code is empty")
	}

	reqBody := runRequest{
		Language: in.Language,
		Source:   in.Code,
		// A single test case carries the optional stdin; expected_stdout is left
		// empty since the agent only wants the program's output, not a verdict.
		Tests: []testCase{{Stdin: in.Stdin}},
	}
	// Java needs the source file (and the run target) named after its public class.
	if in.Language == "java" {
		class := "Main"
		if m := javaClassRe.FindStringSubmatch(in.Code); m != nil {
			class = m[1]
		}
		reqBody.SourceFilename = class + ".java"
		reqBody.ArtifactFilename = class
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return toolError("failed to encode request: %v", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/run", bytes.NewReader(payload))
	if err != nil {
		return toolError("failed to build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return toolError("could not reach Tracebox API at %s: %v", apiURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolError("failed to read API response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr apiError
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
			return toolError("API error (%s): %s", apiErr.Error.Code, apiErr.Error.Message)
		}
		return toolError("API returned status %d: %s", resp.StatusCode, truncate(string(body), 512))
	}

	var rr runResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		return toolError("failed to decode API response: %v", err)
	}

	out := runCodeOutput{RunID: rr.RunID, Status: rr.Status}
	if rr.Build != nil && rr.Build.Status != "build_ok" {
		out.CompileOutput = truncate(joinOutput(rr.Build.Stdout, rr.Build.Stderr), maxOutputBytes)
	}
	if len(rr.Tests) > 0 {
		t := rr.Tests[0]
		out.Stdout = truncate(t.Stdout, maxOutputBytes)
		out.Stderr = truncate(t.Stderr, maxOutputBytes)
		out.DurationMs = t.DurationMs
		out.MemoryPeakKB = t.MemoryPeakKB
	}

	// A short human-readable summary accompanies the structured output for
	// agents that read text content rather than structured results.
	summary := fmt.Sprintf("status=%s run_id=%s", out.Status, out.RunID)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, out, nil
}

func isSupported(lang string) bool {
	return slices.Contains(supportedLanguages, lang)
}

// toolError builds an error CallToolResult carrying a formatted message.
func toolError(format string, args ...any) (*mcp.CallToolResult, runCodeOutput, error) {
	msg := fmt.Sprintf(format, args...)
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}, runCodeOutput{}, nil
}

func joinOutput(stdout, stderr string) string {
	switch {
	case stdout == "":
		return stderr
	case stderr == "":
		return stdout
	default:
		return stdout + "\n" + stderr
	}
}

// truncate keeps the trailing maxBytes of s (the tail is usually the most
// relevant part of program output) and prefixes a notice when it cuts.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return fmt.Sprintf("...[truncated %d bytes]...\n%s", len(s)-maxBytes, s[len(s)-maxBytes:])
}
