//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunHandlerPy3(t *testing.T) {
	body := `{
		"language": "py3",
		"source": "print('hello')",
		"tests": [{"stdin": "", "expected_stdout": "hello\n"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	run(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("expected status accepted, got %q; tests=%+v", resp.Status, resp.Tests)
	}
}

func TestRunHandlerCpp(t *testing.T) {
	body := `{
		"language": "cpp",
		"source": "#include <iostream>\nint main(){std::cout<<\"hello\\n\";}",
		"source_filename": "solution.cpp",
		"artifact_filename": "solution",
		"tests": [{"stdin": "", "expected_stdout": "hello\n"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	run(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp RunResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "accepted" {
		t.Errorf("expected status accepted, got %q; build=%+v tests=%+v", resp.Status, resp.Build, resp.Tests)
	}
}
