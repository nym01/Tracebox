package api

import (
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
	if _, ok := language.Lookup(req.Language); !ok {
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
	return nil
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
