package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateRunRequest(t *testing.T) {
	validSource := "print('hello')"
	validTests := []TestCase{{Stdin: "", ExpectedStdout: "hello\n"}}

	cases := []struct {
		name     string
		req      RunRequest
		wantCode string
	}{
		{
			name:     "valid py3",
			req:      RunRequest{Language: "py3", Source: validSource, Tests: validTests},
			wantCode: "",
		},
		{
			name:     "valid cpp",
			req:      RunRequest{Language: "cpp", Source: "#include<iostream>", Tests: validTests},
			wantCode: "",
		},
		{
			name:     "unknown language",
			req:      RunRequest{Language: "cobol", Source: validSource, Tests: validTests},
			wantCode: "unknown_language",
		},
		{
			name:     "source missing",
			req:      RunRequest{Language: "py3", Source: "", Tests: validTests},
			wantCode: "invalid_source",
		},
		{
			name:     "source over 256 KiB",
			req:      RunRequest{Language: "py3", Source: strings.Repeat("a", maxSourceBytes+1), Tests: validTests},
			wantCode: "invalid_source",
		},
		{
			name:     "source_filename with forward slash",
			req:      RunRequest{Language: "py3", Source: validSource, SourceFilename: "a/b.py", Tests: validTests},
			wantCode: "invalid_filename",
		},
		{
			name:     "source_filename with backslash",
			req:      RunRequest{Language: "py3", Source: validSource, SourceFilename: `a\b.py`, Tests: validTests},
			wantCode: "invalid_filename",
		},
		{
			name:     "source_filename leading dot",
			req:      RunRequest{Language: "py3", Source: validSource, SourceFilename: ".secret.py", Tests: validTests},
			wantCode: "invalid_filename",
		},
		{
			name:     "artifact_filename with slash",
			req:      RunRequest{Language: "cpp", Source: validSource, ArtifactFilename: "out/solution", Tests: validTests},
			wantCode: "invalid_filename",
		},
		{
			name:     "artifact_filename leading dot",
			req:      RunRequest{Language: "cpp", Source: validSource, ArtifactFilename: ".solution", Tests: validTests},
			wantCode: "invalid_filename",
		},
		{
			name:     "tests nil",
			req:      RunRequest{Language: "py3", Source: validSource},
			wantCode: "invalid_tests",
		},
		{
			name:     "tests empty slice",
			req:      RunRequest{Language: "py3", Source: validSource, Tests: []TestCase{}},
			wantCode: "invalid_tests",
		},
		{
			name:     "source not valid UTF-8",
			req:      RunRequest{Language: "py3", Source: "\xff\xfe", Tests: validTests},
			wantCode: "invalid_source",
		},
		{
			name:     "source_filename too long",
			req:      RunRequest{Language: "py3", Source: validSource, SourceFilename: strings.Repeat("a", maxFilenameLen+1) + ".py", Tests: validTests},
			wantCode: "invalid_filename",
		},
		{
			name:     "artifact_filename too long",
			req:      RunRequest{Language: "cpp", Source: validSource, ArtifactFilename: strings.Repeat("a", maxFilenameLen+1), Tests: validTests},
			wantCode: "invalid_filename",
		},

		// Flag allow-list cases
		{
			name:     "cpp allowed flag passes",
			req:      RunRequest{Language: "cpp", Source: validSource, Build: &PhaseConfig{Flags: []string{"-O2"}}, Tests: validTests},
			wantCode: "",
		},
		{
			name:     "cpp disallowed flag rejected",
			req:      RunRequest{Language: "cpp", Source: validSource, Build: &PhaseConfig{Flags: []string{"-fplugin"}}, Tests: validTests},
			wantCode: "disallowed_flag",
		},
		{
			name:     "cpp glob match -std=c++17",
			req:      RunRequest{Language: "cpp", Source: validSource, Build: &PhaseConfig{Flags: []string{"-std=c++17"}}, Tests: validTests},
			wantCode: "",
		},
		{
			name:     "cpp empty build flags accepted",
			req:      RunRequest{Language: "cpp", Source: validSource, Build: &PhaseConfig{Flags: []string{}}, Tests: validTests},
			wantCode: "",
		},
		{
			name:     "py3 empty flags accepted",
			req:      RunRequest{Language: "py3", Source: validSource, Run: &PhaseConfig{Flags: []string{}}, Tests: validTests},
			wantCode: "",
		},
		{
			name:     "py3 no allowlist rejects all flags",
			req:      RunRequest{Language: "py3", Source: validSource, Run: &PhaseConfig{Flags: []string{"-v"}}, Tests: validTests},
			wantCode: "disallowed_flag",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verr := validateRunRequest(&tc.req)
			if tc.wantCode == "" {
				if verr != nil {
					t.Fatalf("expected no error, got code=%q message=%q", verr.Code, verr.Message)
				}
			} else {
				if verr == nil {
					t.Fatalf("expected code %q, got nil", tc.wantCode)
				}
				if verr.Code != tc.wantCode {
					t.Errorf("expected code %q, got %q", tc.wantCode, verr.Code)
				}
			}
		})
	}
}

func TestValidateFlags(t *testing.T) {
	cppAllowlist := []string{"-O0", "-O1", "-O2", "-O3", "-Wall", "-Wextra", "-std=*"}

	cases := []struct {
		name      string
		flags     []string
		allowlist []string
		wantCode  string
	}{
		{
			name:      "allowed literal flag",
			flags:     []string{"-O2"},
			allowlist: cppAllowlist,
			wantCode:  "",
		},
		{
			name:      "disallowed flag",
			flags:     []string{"-fplugin"},
			allowlist: cppAllowlist,
			wantCode:  "disallowed_flag",
		},
		{
			name:      "glob match -std=c++17",
			flags:     []string{"-std=c++17"},
			allowlist: cppAllowlist,
			wantCode:  "",
		},
		{
			name:      "glob match -std=gnu99",
			flags:     []string{"-std=gnu99"},
			allowlist: cppAllowlist,
			wantCode:  "",
		},
		{
			name:      "empty flags always accepted",
			flags:     []string{},
			allowlist: nil,
			wantCode:  "",
		},
		{
			name:      "nil flags always accepted",
			flags:     nil,
			allowlist: nil,
			wantCode:  "",
		},
		{
			name:      "nil allowlist rejects any flag",
			flags:     []string{"-O2"},
			allowlist: nil,
			wantCode:  "disallowed_flag",
		},
		{
			name:      "empty allowlist rejects any flag",
			flags:     []string{"-O2"},
			allowlist: []string{},
			wantCode:  "disallowed_flag",
		},
		{
			name:      "first flag disallowed stops early",
			flags:     []string{"-fplugin", "-O2"},
			allowlist: cppAllowlist,
			wantCode:  "disallowed_flag",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verr := validateFlags(tc.flags, "cpp", tc.allowlist)
			if tc.wantCode == "" {
				if verr != nil {
					t.Fatalf("expected no error, got code=%q message=%q", verr.Code, verr.Message)
				}
			} else {
				if verr == nil {
					t.Fatalf("expected code %q, got nil", tc.wantCode)
				}
				if verr.Code != tc.wantCode {
					t.Errorf("expected code %q, got %q", tc.wantCode, verr.Code)
				}
			}
		})
	}
}

func TestRunHandlerInvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{bad json}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	run(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"invalid_json"`) {
		t.Errorf("expected invalid_json in body, got %s", w.Body.String())
	}
}
