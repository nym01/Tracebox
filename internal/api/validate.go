package api

import (
	"strings"

	"github.com/nym01/goboxd/internal/language"
)

const maxSourceBytes = 256 * 1024

type TestCase struct {
	Stdin          string `json:"stdin"`
	ExpectedStdout string `json:"expected_stdout"`
}

type RunRequest struct {
	Language         string     `json:"language"`
	Source           string     `json:"source"`
	SourceFilename   string     `json:"source_filename"`
	ArtifactFilename string     `json:"artifact_filename"`
	Tests            []TestCase `json:"tests"`
}

type validationError struct {
	Code    string
	Message string
}

func validateRunRequest(req *RunRequest) *validationError {
	if _, ok := language.Lookup(req.Language); !ok {
		return &validationError{Code: "unknown_language", Message: "language not supported"}
	}
	if len(req.Source) == 0 || len(req.Source) > maxSourceBytes {
		return &validationError{Code: "invalid_source", Message: "source is missing or exceeds 256 KiB"}
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
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return &validationError{
			Code:    "invalid_filename",
			Message: "filename must be a single path component and must not start with a dot",
		}
	}
	return nil
}
