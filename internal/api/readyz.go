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

// NsjailStatus is the probe result for the nsjail binary in GET /readyz.
type NsjailStatus struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ReadyzResult is the response body for GET /readyz.
type ReadyzResult struct {
	Status    string                    `json:"status"`
	Nsjail    *NsjailStatus             `json:"nsjail,omitempty"`
	Languages map[string]LanguageStatus `json:"languages"`
}

var cachedReadyz *ReadyzResult

// InitReadyz probes every registered language and the nsjail binary, and
// caches the result. nsjailPath is the path to the nsjail binary
// (e.g. /usr/local/bin/nsjail). Must be called once at startup before
// serving requests.
func InitReadyz(nsjailPath string) {
	cachedReadyz = buildReadyz(language.All(), nsjailPath, execVersionProbe)
}

// buildReadyz runs probe for each language and (if nsjailPath is non-empty)
// for the nsjail binary, and assembles a ReadyzResult.
// Separated so tests can inject a fake probe.
func buildReadyz(langs []*language.Language, nsjailPath string, probe func(cmd string, args []string) (string, error)) *ReadyzResult {
	statuses := make(map[string]LanguageStatus, len(langs))
	allOK := true
	for _, lang := range langs {
		cmd := probeCmdFor(lang)
		args := lang.VersionArgs
		if len(args) == 0 {
			args = []string{"--version"}
		}
		ver, err := probe(cmd, args)
		if err != nil {
			statuses[lang.ID] = LanguageStatus{OK: false, Error: err.Error()}
			allOK = false
		} else {
			statuses[lang.ID] = LanguageStatus{OK: true, Version: ver}
		}
	}

	var nsjailSt *NsjailStatus
	if nsjailPath != "" {
		ver, err := probe(nsjailPath, []string{"--help"})
		ns := &NsjailStatus{}
		if err != nil {
			ns.OK = false
			ns.Error = err.Error()
			allOK = false
		} else {
			ns.OK = true
			ns.Version = ver
		}
		nsjailSt = ns
	}

	top := "ok"
	if !allOK {
		top = "degraded"
	}
	return &ReadyzResult{Status: top, Nsjail: nsjailSt, Languages: statuses}
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

// execVersionProbe runs "cmd args..." and returns the first output line.
func execVersionProbe(cmd string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmd, args...).CombinedOutput()
	line := parseFirstLine(string(out))
	if line != "" {
		return line, nil
	}
	if err != nil {
		return "", fmt.Errorf("%s %v: %w", cmd, args, err)
	}
	return line, nil
}
