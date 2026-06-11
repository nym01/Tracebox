package api

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nym01/goboxd/internal/language"
)

// LanguageStatus is the probe result for one language in GET /readyz.
type LanguageStatus struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ReadyzResult is the response body for GET /readyz.
type ReadyzResult struct {
	Status    string                    `json:"status"`
	Languages map[string]LanguageStatus `json:"languages"`
}

var cachedReadyz *ReadyzResult

// InitReadyz probes every registered language and caches the result.
// Must be called once at startup before serving requests.
func InitReadyz() {
	cachedReadyz = buildReadyz(language.All(), execVersionProbe)
}

// buildReadyz runs probe for each language and assembles a ReadyzResult.
// Separated so tests can inject a fake probe.
func buildReadyz(langs []*language.Language, probe func(string) (string, error)) *ReadyzResult {
	statuses := make(map[string]LanguageStatus, len(langs))
	allOK := true
	for _, lang := range langs {
		cmd := probeCmdFor(lang)
		ver, err := probe(cmd)
		if err != nil {
			statuses[lang.ID] = LanguageStatus{OK: false, Error: err.Error()}
			allOK = false
		} else {
			statuses[lang.ID] = LanguageStatus{OK: true, Version: ver}
		}
	}
	top := "ok"
	if !allOK {
		top = "degraded"
	}
	return &ReadyzResult{Status: top, Languages: statuses}
}

// probeCmdFor picks which binary to probe: build cmd for compiled languages,
// run cmd for interpreted ones.
func probeCmdFor(lang *language.Language) string {
	if lang.Build != nil {
		return lang.Build.Cmd
	}
	return lang.Run.Cmd
}

// parseFirstLine returns the first line of output, trimmed of whitespace.
func parseFirstLine(output string) string {
	if i := strings.IndexByte(output, '\n'); i >= 0 {
		return strings.TrimSpace(output[:i])
	}
	return strings.TrimSpace(output)
}

// execVersionProbe runs "cmd --version" and returns the first output line.
func execVersionProbe(cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmd, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", cmd, err)
	}
	return parseFirstLine(string(out)), nil
}
