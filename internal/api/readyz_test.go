package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nym01/goboxd/internal/language"
)

// --- parseFirstLine ---

func TestParseFirstLine(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Python 3.11.2\n", "Python 3.11.2"},
		{"g++ (Debian 12.2.0) 12.2.0\nCopyright (C) ...\n", "g++ (Debian 12.2.0) 12.2.0"},
		{"GNU bash, version 5.2.0(1)-release (x86_64-pc-linux-gnu)", "GNU bash, version 5.2.0(1)-release (x86_64-pc-linux-gnu)"},
		{"  v18.19.0  \n", "v18.19.0"},
		{"javac 17.0.9\n", "javac 17.0.9"},
		{"Icarus Verilog version 11.0 (stable)\n(c) ...\n", "Icarus Verilog version 11.0 (stable)"},
	}
	for _, tc := range cases {
		got := parseFirstLine(tc.input)
		if got != tc.want {
			t.Errorf("parseFirstLine(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- buildReadyz ---

func TestBuildReadyzAllPass(t *testing.T) {
	langs := []*language.Language{
		{ID: "py3", Name: "Python 3", Run: language.RunConfig{Cmd: "/usr/bin/python3"}},
		{ID: "cpp", Name: "C++", Run: language.RunConfig{Cmd: "./solution"}, Build: &language.BuildConfig{Cmd: "/usr/bin/g++"}},
	}
	probe := func(cmd string, _ []string) (string, error) {
		versions := map[string]string{
			"/usr/bin/python3": "Python 3.11.2",
			"/usr/bin/g++":    "g++ (Debian 12.2.0) 12.2.0",
		}
		if v, ok := versions[cmd]; ok {
			return v, nil
		}
		return "", fmt.Errorf("unexpected probe cmd: %s", cmd)
	}

	result := buildReadyz(langs, "", probe)

	if result.Status != "ok" {
		t.Errorf("status: want ok, got %q", result.Status)
	}
	if s := result.Languages["py3"]; !s.OK || s.Version != "Python 3.11.2" {
		t.Errorf("py3: %+v", s)
	}
	if s := result.Languages["cpp"]; !s.OK || s.Version != "g++ (Debian 12.2.0) 12.2.0" {
		t.Errorf("cpp: %+v", s)
	}
}

func TestBuildReadyzOneFails(t *testing.T) {
	langs := []*language.Language{
		{ID: "py3", Name: "Python 3", Run: language.RunConfig{Cmd: "/usr/bin/python3"}},
		{ID: "java", Name: "Java", Run: language.RunConfig{Cmd: "/usr/bin/java"}, Build: &language.BuildConfig{Cmd: "/usr/bin/javac"}},
	}
	probe := func(cmd string, _ []string) (string, error) {
		if cmd == "/usr/bin/python3" {
			return "Python 3.11.2", nil
		}
		return "", fmt.Errorf("javac not found at /usr/bin/javac")
	}

	result := buildReadyz(langs, "", probe)

	if result.Status != "degraded" {
		t.Errorf("status: want degraded, got %q", result.Status)
	}
	if s := result.Languages["py3"]; !s.OK {
		t.Errorf("py3 should be ok: %+v", s)
	}
	if s := result.Languages["java"]; s.OK || s.Error == "" {
		t.Errorf("java should be failed with error: %+v", s)
	}
}

func TestBuildReadyzProbeCmdSelection(t *testing.T) {
	// compiled language → build cmd; interpreted → run cmd
	langs := []*language.Language{
		{ID: "cpp", Run: language.RunConfig{Cmd: "./solution"}, Build: &language.BuildConfig{Cmd: "/usr/bin/g++"}},
		{ID: "py3", Run: language.RunConfig{Cmd: "/usr/bin/python3"}},
	}
	probed := map[string]bool{}
	probe := func(cmd string, _ []string) (string, error) {
		probed[cmd] = true
		return "v1.0", nil
	}
	buildReadyz(langs, "", probe)

	if !probed["/usr/bin/g++"] {
		t.Error("compiled language: expected build cmd /usr/bin/g++ to be probed")
	}
	if probed["./solution"] {
		t.Error("compiled language: run cmd ./solution should not be probed")
	}
	if !probed["/usr/bin/python3"] {
		t.Error("interpreted language: expected run cmd /usr/bin/python3 to be probed")
	}
}

// --- nsjail probe via buildReadyz ---

func TestBuildReadyzNsjailAvailable(t *testing.T) {
	langs := []*language.Language{}
	probe := func(cmd string, _ []string) (string, error) {
		if cmd == "/usr/local/bin/nsjail" {
			return "3.4", nil
		}
		return "", fmt.Errorf("unexpected probe cmd: %s", cmd)
	}

	result := buildReadyz(langs, "/usr/local/bin/nsjail", probe)

	if result.Status != "ok" {
		t.Errorf("status: want ok, got %q", result.Status)
	}
	if result.Nsjail == nil {
		t.Fatal("nsjail field should be present")
	}
	if !result.Nsjail.OK {
		t.Errorf("nsjail.ok: want true; error: %s", result.Nsjail.Error)
	}
	if result.Nsjail.Version != "3.4" {
		t.Errorf("nsjail.version: want 3.4, got %q", result.Nsjail.Version)
	}
}

func TestBuildReadyzNsjailMissing(t *testing.T) {
	langs := []*language.Language{}
	probe := func(cmd string, _ []string) (string, error) {
		return "", fmt.Errorf("exec: %q: executable file not found in $PATH", cmd)
	}

	result := buildReadyz(langs, "/usr/local/bin/nsjail", probe)

	if result.Status != "degraded" {
		t.Errorf("status: want degraded, got %q", result.Status)
	}
	if result.Nsjail == nil {
		t.Fatal("nsjail field should be present")
	}
	if result.Nsjail.OK {
		t.Error("nsjail.ok: want false")
	}
	if result.Nsjail.Error == "" {
		t.Error("nsjail.error: want non-empty")
	}
}

// --- readyzHandler ---

func TestReadyzHandlerAllPass(t *testing.T) {
	orig := cachedReadyz
	cachedReadyz = &ReadyzResult{
		Status: "ok",
		Languages: map[string]LanguageStatus{
			"py3": {OK: true, Version: "Python 3.11.2"},
			"cpp": {OK: true, Version: "g++ 12.2.0"},
		},
	}
	defer func() { cachedReadyz = orig }()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	readyzHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp ReadyzResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: want ok, got %q", resp.Status)
	}
	if s := resp.Languages["py3"]; !s.OK || s.Version != "Python 3.11.2" {
		t.Errorf("py3: %+v", s)
	}
	if s := resp.Languages["cpp"]; !s.OK || s.Version != "g++ 12.2.0" {
		t.Errorf("cpp: %+v", s)
	}
}

func TestReadyzHandlerDegraded(t *testing.T) {
	orig := cachedReadyz
	cachedReadyz = &ReadyzResult{
		Status: "degraded",
		Languages: map[string]LanguageStatus{
			"py3":  {OK: true, Version: "Python 3.11.2"},
			"java": {OK: false, Error: "javac not found at /usr/bin/javac"},
		},
	}
	defer func() { cachedReadyz = orig }()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	readyzHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	var resp ReadyzResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("status: want degraded, got %q", resp.Status)
	}
	if s := resp.Languages["java"]; s.OK || s.Error == "" {
		t.Errorf("java should be failed with error: %+v", s)
	}
	if s := resp.Languages["py3"]; !s.OK {
		t.Errorf("py3 should still be ok: %+v", s)
	}
}

// --- readyzHandler nsjail-specific ---

func TestReadyzHandlerNsjailAvailable(t *testing.T) {
	orig := cachedReadyz
	cachedReadyz = &ReadyzResult{
		Status:    "ok",
		Nsjail:    &NsjailStatus{OK: true, Version: "3.4"},
		Languages: map[string]LanguageStatus{},
	}
	defer func() { cachedReadyz = orig }()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	readyzHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp ReadyzResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: want ok, got %q", resp.Status)
	}
	if resp.Nsjail == nil {
		t.Fatal("nsjail field missing from response")
	}
	if !resp.Nsjail.OK {
		t.Errorf("nsjail.ok: want true")
	}
	if resp.Nsjail.Version != "3.4" {
		t.Errorf("nsjail.version: want 3.4, got %q", resp.Nsjail.Version)
	}
}

func TestReadyzHandlerNsjailMissing(t *testing.T) {
	orig := cachedReadyz
	cachedReadyz = &ReadyzResult{
		Status:    "degraded",
		Nsjail:    &NsjailStatus{OK: false, Error: "nsjail not found"},
		Languages: map[string]LanguageStatus{},
	}
	defer func() { cachedReadyz = orig }()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	readyzHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	var resp ReadyzResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("status: want degraded, got %q", resp.Status)
	}
	if resp.Nsjail == nil {
		t.Fatal("nsjail field missing from response")
	}
	if resp.Nsjail.OK {
		t.Error("nsjail.ok: want false")
	}
	if resp.Nsjail.Error == "" {
		t.Error("nsjail.error: want non-empty")
	}
}
