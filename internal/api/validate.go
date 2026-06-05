package api

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/nym01/goboxd/internal/language"
)

const maxSourceBytes = 256 * 1024
const maxFilenameLen = 64

type TestCase struct {
	Stdin          string `json:"stdin"`
	ExpectedStdout string `json:"expected_stdout"`
}

// PhaseConfig carries per-request limits and flags for a build or run phase.
// Anything omitted falls back to the language default.
type PhaseConfig struct {
	Limits *language.Limits `json:"limits,omitempty"`
	Flags  []string         `json:"flags,omitempty"`
}

type RunRequest struct {
	Language         string       `json:"language"`
	Source           string       `json:"source"`
	SourceFilename   string       `json:"source_filename"`
	ArtifactFilename string       `json:"artifact_filename"`
	Build            *PhaseConfig `json:"build,omitempty"`
	Run              *PhaseConfig `json:"run,omitempty"`
	Tests            []TestCase   `json:"tests"`
}

type validationError struct {
	Code    string
	Message string
}

func validateRunRequest(req *RunRequest) *validationError {
	lang, ok := language.Lookup(req.Language)
	if !ok {
		return &validationError{Code: "unknown_language", Message: "language not supported"}
	}
	if len(req.Source) == 0 || !utf8.ValidString(req.Source) || len(req.Source) > maxSourceBytes {
		return &validationError{Code: "invalid_source", Message: "source is missing, not valid UTF-8, or exceeds 256 KiB"}
	}
	if verr := validateFilename(req.SourceFilename); verr != nil {
		return verr
	}
	if verr := validateFilename(req.ArtifactFilename); verr != nil {
		return verr
	}
	if len(req.Tests) == 0 {
		return &validationError{Code: "invalid_tests", Message: "tests must contain at least one entry"}
	}

	var allowlist []string
	if lang.Build != nil {
		allowlist = lang.Build.FlagAllowlist
	}
	if req.Build != nil {
		if verr := validateFlags(req.Build.Flags, req.Language, allowlist); verr != nil {
			return verr
		}
	}
	if req.Run != nil {
		if verr := validateFlags(req.Run.Flags, req.Language, allowlist); verr != nil {
			return verr
		}
	}
	return nil
}

// validateFlags rejects any flag not matched by the allowlist.
// A nil allowlist means no flags are permitted for this language.
// An empty flags slice is always accepted.
func validateFlags(flags []string, langID string, allowlist []string) *validationError {
	if len(flags) == 0 {
		return nil
	}
	for _, f := range flags {
		if !flagAllowed(f, allowlist) {
			return &validationError{
				Code:    "disallowed_flag",
				Message: fmt.Sprintf("flag %s is not allowed for %s", f, langID),
			}
		}
	}
	return nil
}

// flagAllowed reports whether flag matches any pattern in the allowlist.
// Patterns may contain glob wildcards (e.g. -std=* matches -std=c++17).
func flagAllowed(flag string, allowlist []string) bool {
	for _, pattern := range allowlist {
		if matched, err := path.Match(pattern, flag); err == nil && matched {
			return true
		}
	}
	return false
}

func validateFilename(name string) *validationError {
	if name == "" {
		return nil
	}
	if len(name) > maxFilenameLen {
		return &validationError{
			Code:    "invalid_filename",
			Message: "filename must not exceed 64 characters",
		}
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return &validationError{
			Code:    "invalid_filename",
			Message: "filename must be a single path component and must not start with a dot",
		}
	}
	return nil
}
