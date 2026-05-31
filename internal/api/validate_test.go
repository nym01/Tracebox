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
			req:      RunRequest{Language: "java", Source: validSource, Tests: validTests},
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
