// Command tracebox-cli lets a developer run a source file in the Tracebox
// sandbox instead of on their own machine:
//
//	tracebox run script.py
//
// It reads the file, detects the language from its extension, and sends the
// code to a running Tracebox API server via POST /run, then prints the
// program's output alongside a plain-English explanation of what happened.
//
// Like cmd/tracebox-mcp, this is a THIN CLIENT over the HTTP /run endpoint: no
// sandbox logic lives here. The API server (cmd/tracebox) must be running
// separately.
//
// Configuration:
//
//	TRACEBOX_API_URL   base URL of the Tracebox API (default http://localhost:8080)
//
// Exit codes:
//
//	0  the sandboxed program ran (regardless of what it printed)
//	1  the run itself failed: build_failed, runtime_error, time_exceeded,
//	   memory_exceeded, internal_error, not_executed
//	2  CLI/usage error: bad arguments, unknown file extension, unreadable file
//	3  could not reach the API, or the API returned an error
//
// See README.md in this directory for usage examples.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultAPIURL = "http://localhost:8080"

// httpTimeout bounds a single /run call. Compiled languages plus generous wall
// limits can take a while, so this is comfortably above the API's own limits.
const httpTimeout = 60 * time.Second

// Exit codes. See the package doc comment for the contract.
const (
	exitRan     = 0 // sandboxed program ran
	exitRunFail = 1 // run failed (build/runtime/limit/internal)
	exitUsage   = 2 // CLI or usage error
	exitAPI     = 3 // could not reach the API or API error
)

// extToLanguage maps a file extension (lowercase, with leading dot) to one of
// the seven supported Tracebox language IDs. The IDs and their canonical
// extensions mirror configs/languages.yaml.
var extToLanguage = map[string]string{
	".py":   "py3",
	".cpp":  "cpp",
	".cc":   "cpp",
	".cxx":  "cpp",
	".c":    "c",
	".sh":   "bash",
	".js":   "js",
	".java": "java",
	".v":    "verilog",
}

// javaClassRe extracts the (public) top-level class name so Java sources can be
// written to a matching filename, which javac requires. Mirrors tracebox-mcp.
var javaClassRe = regexp.MustCompile(`(?m)^\s*public\s+(?:final\s+|abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)

// --- Minimal mirror of the Tracebox HTTP API request/response shapes. ---
// Defined locally so the CLI stays a thin HTTP client and does not pull in the
// server-side runner/sandbox packages. Kept in sync with cmd/tracebox-mcp.

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

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses arguments, performs the request, prints the result, and returns
// the process exit code. Splitting this out of main keeps os.Exit at the edge.
func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return exitRan
	case "run":
		return runCommand(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tracebox: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return exitUsage
	}
}

// runCommand implements `tracebox run <file> [--stdin S | --stdin-file PATH]`.
func runCommand(args []string) int {
	var (
		file      string
		stdin     string
		stdinSet  bool
		stdinFile string
	)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--stdin":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "tracebox: --stdin requires a value")
				return exitUsage
			}
			stdin, stdinSet = args[i+1], true
			i++
		case strings.HasPrefix(arg, "--stdin="):
			stdin, stdinSet = strings.TrimPrefix(arg, "--stdin="), true
		case arg == "--stdin-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "tracebox: --stdin-file requires a path")
				return exitUsage
			}
			stdinFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--stdin-file="):
			stdinFile = strings.TrimPrefix(arg, "--stdin-file=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(os.Stderr, "tracebox: unknown flag %q\n", arg)
			return exitUsage
		default:
			if file != "" {
				fmt.Fprintf(os.Stderr, "tracebox: unexpected extra argument %q\n", arg)
				return exitUsage
			}
			file = arg
		}
	}

	if file == "" {
		fmt.Fprintln(os.Stderr, "tracebox: run requires a source file")
		usage(os.Stderr)
		return exitUsage
	}
	if stdinSet && stdinFile != "" {
		fmt.Fprintln(os.Stderr, "tracebox: use only one of --stdin and --stdin-file")
		return exitUsage
	}

	lang, ok := extToLanguage[strings.ToLower(filepath.Ext(file))]
	if !ok {
		fmt.Fprintf(os.Stderr, "tracebox: cannot detect language for %q; supported extensions: %s\n",
			file, supportedExtensions())
		return exitUsage
	}

	source, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: cannot read %q: %v\n", file, err)
		return exitUsage
	}
	if strings.TrimSpace(string(source)) == "" {
		fmt.Fprintf(os.Stderr, "tracebox: %q is empty\n", file)
		return exitUsage
	}

	if stdinFile != "" {
		b, err := os.ReadFile(stdinFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tracebox: cannot read stdin file %q: %v\n", stdinFile, err)
			return exitUsage
		}
		stdin = string(b)
	}

	apiURL := strings.TrimRight(os.Getenv("TRACEBOX_API_URL"), "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	reqBody := runRequest{
		Language: lang,
		Source:   string(source),
		// A single test case carries the optional stdin; expected_stdout is left
		// empty because we only want the program's output, not a verdict.
		Tests: []testCase{{Stdin: stdin}},
	}
	// Java needs the source file (and run target) named after its public class.
	if lang == "java" {
		class := "Main"
		if m := javaClassRe.FindStringSubmatch(string(source)); m != nil {
			class = m[1]
		}
		reqBody.SourceFilename = class + ".java"
		reqBody.ArtifactFilename = class
	}

	resp, code := postRun(apiURL, reqBody)
	if code != exitRan {
		return code
	}
	return report(resp)
}

// postRun sends the request and decodes the response. The returned exit code is
// exitRan on success or exitAPI when the API could not be reached/decoded.
func postRun(apiURL string, reqBody runRequest) (*runResponse, int) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: failed to encode request: %v\n", err)
		return nil, exitAPI
	}

	client := &http.Client{Timeout: httpTimeout}
	httpReq, err := http.NewRequest(http.MethodPost, apiURL+"/run", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: failed to build request: %v\n", err)
		return nil, exitAPI
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: could not reach Tracebox API at %s: %v\n", apiURL, err)
		return nil, exitAPI
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: failed to read API response: %v\n", err)
		return nil, exitAPI
	}

	if httpResp.StatusCode != http.StatusOK {
		var apiErr apiError
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
			fmt.Fprintf(os.Stderr, "tracebox: API error (%s): %s\n", apiErr.Error.Code, apiErr.Error.Message)
		} else {
			fmt.Fprintf(os.Stderr, "tracebox: API returned status %d: %s\n", httpResp.StatusCode, truncateHead(string(body), 512))
		}
		return nil, exitAPI
	}

	var rr runResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: failed to decode API response: %v\n", err)
		return nil, exitAPI
	}
	return &rr, exitRan
}

// report prints the program output and a plain-English explanation, then
// returns the process exit code derived from the run status.
func report(rr *runResponse) int {
	var test *testResult
	if len(rr.Tests) > 0 {
		test = &rr.Tests[0]
	}

	// On a build failure, the compiler output is the most useful thing to show.
	if rr.Build != nil && rr.Build.Status != "build_ok" {
		out := joinOutput(rr.Build.Stdout, rr.Build.Stderr)
		if out != "" {
			fmt.Println("=== compile output ===")
			fmt.Println(strings.TrimRight(out, "\n"))
		}
	}

	if test != nil {
		if test.Stdout != "" {
			fmt.Println("=== stdout ===")
			fmt.Println(strings.TrimRight(test.Stdout, "\n"))
		}
		if test.Stderr != "" {
			fmt.Println("=== stderr ===")
			fmt.Println(strings.TrimRight(test.Stderr, "\n"))
		}
	}

	expl := explain(rr.Status)
	fmt.Println("===")
	fmt.Printf("%s: %s\n", expl.label, expl.detail)
	fmt.Printf("run_id: %s\n", rr.RunID)
	if test != nil {
		fmt.Printf("duration: %dms", test.DurationMs)
		if test.MemoryPeakKB > 0 {
			fmt.Printf("  memory: %dKB", test.MemoryPeakKB)
		}
		fmt.Println()
	}

	return exitCodeFor(rr.Status)
}

// ranStatuses are the API verdicts that mean "the program executed". Because
// the CLI never supplies an expected output, the comparison verdicts
// (accepted / wrong_output / output_whitespace_mismatch) are not pass/fail
// signals — they all just mean the code ran. This mirrors tracebox-mcp's
// reportedStatus() and the frontend's explain.ts.
var ranStatuses = map[string]bool{
	"accepted":                   true,
	"wrong_output":               true,
	"output_whitespace_mismatch": true,
}

// exitCodeFor maps a run status to the process exit code: 0 if the program ran,
// exitRunFail otherwise.
func exitCodeFor(status string) int {
	if ranStatuses[status] {
		return exitRan
	}
	return exitRunFail
}

type explanation struct {
	label  string
	detail string
}

// explain maps a top-level run status to a short plain-English label and
// sentence. The wording follows the frontend's explain.ts so the CLI, the web
// UI, and the MCP server all describe results the same way.
func explain(status string) explanation {
	if ranStatuses[status] {
		return explanation{
			label:  "ran successfully",
			detail: "The program executed to completion inside the sandbox. Its output is shown above; this isn't checked against any expected answer, so a clean run just means it finished without errors.",
		}
	}
	switch status {
	case "runtime_error":
		return explanation{
			label:  "crashed",
			detail: "The program exited with a non-zero status or was terminated by the sandbox. Check the standard error output above for the cause — an exception, a failed assertion, or a blocked operation.",
		}
	case "time_exceeded":
		return explanation{
			label:  "took too long",
			detail: "The program ran past its time limit and was stopped by the sandbox. This usually points to an infinite loop or work that's too slow to finish in time.",
		}
	case "memory_exceeded":
		return explanation{
			label:  "used too much memory",
			detail: "The program exceeded its memory limit and was killed by the sandbox. Look for large allocations, unbounded data structures, or runaway recursion.",
		}
	case "build_failed":
		return explanation{
			label:  "failed to compile",
			detail: "The compiler rejected the code before it could run. The compile output above shows the errors — fix those and run again.",
		}
	case "internal_error":
		return explanation{
			label:  "hit a sandbox error",
			detail: "Something went wrong inside Tracebox itself, not in your code. Try running again.",
		}
	case "not_executed":
		return explanation{
			label:  "was not run",
			detail: "Execution was skipped, usually because an earlier phase (such as compilation) did not succeed.",
		}
	default:
		return explanation{
			label:  strings.ReplaceAll(status, "_", " "),
			detail: "The run finished with this status; see the output above for details.",
		}
	}
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

// truncateHead keeps the leading maxBytes of s, for short error snippets.
func truncateHead(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "...[truncated]"
}

// supportedExtensions returns the recognised extensions, for error messages.
func supportedExtensions() string {
	seen := make(map[string]bool)
	var exts []string
	for ext := range extToLanguage {
		if !seen[ext] {
			seen[ext] = true
			exts = append(exts, ext)
		}
	}
	// Deterministic order for a stable message.
	for i := 0; i < len(exts); i++ {
		for j := i + 1; j < len(exts); j++ {
			if exts[j] < exts[i] {
				exts[i], exts[j] = exts[j], exts[i]
			}
		}
	}
	return strings.Join(exts, ", ")
}

func usage(w io.Writer) {
	fmt.Fprint(w, `tracebox — run a source file in the Tracebox sandbox

Usage:
  tracebox run <file> [--stdin "input" | --stdin-file path]

The language is detected from the file extension and the code is sent to the
Tracebox API (POST /run). The program's stdout/stderr and a plain-English
explanation of the result are printed.

Flags:
  --stdin S          feed S to the program's standard input
  --stdin-file PATH  feed the contents of PATH to standard input

Environment:
  TRACEBOX_API_URL   base URL of the Tracebox API (default http://localhost:8080)

Exit codes:
  0  the sandboxed program ran (whatever it printed)
  1  the run failed (build_failed, runtime_error, time_exceeded, ...)
  2  CLI/usage error (bad args, unknown extension, unreadable file)
  3  could not reach the API, or the API returned an error

Supported extensions: .py .cpp .cc .cxx .c .sh .js .java .v
`)
}
