package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync/atomic"

	"github.com/nym01/goboxd/internal/language"
)

const (
	infoVersion    = "0.2.0"
	maxTests       = 50
)

var (
	buildCommit string = "dev"
	jobsTotal   atomic.Int64
)

// SetBuildCommit stores the commit hash injected via ldflags at startup.
func SetBuildCommit(commit string) {
	buildCommit = commit
}

func incrementJobsTotal() {
	jobsTotal.Add(1)
}

type infoBuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
}

type infoLangEntry struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Version          string          `json:"version"`
	DefaultRunLimits language.Limits `json:"default_run_limits"`
}

type infoLimits struct {
	MaxSourceBytes int `json:"max_source_bytes"`
	MaxTests       int `json:"max_tests"`
}

type infoStats struct {
	JobsTotal int64 `json:"jobs_total"`
}

type infoResponse struct {
	BuildInfo infoBuildInfo   `json:"build_info"`
	Languages []infoLangEntry `json:"languages"`
	Limits    infoLimits      `json:"limits"`
	Stats     infoStats       `json:"stats"`
}

func infoHandler(w http.ResponseWriter, _ *http.Request) {
	langs := language.All()
	entries := make([]infoLangEntry, 0, len(langs))
	for _, lang := range langs {
		ver := ""
		if cachedReadyz != nil {
			if s, ok := cachedReadyz.Languages[lang.ID]; ok {
				ver = s.Version
			}
		}
		entries = append(entries, infoLangEntry{
			ID:               lang.ID,
			Name:             lang.Name,
			Version:          ver,
			DefaultRunLimits: lang.Run.Limits,
		})
	}

	resp := infoResponse{
		BuildInfo: infoBuildInfo{
			Version:   infoVersion,
			Commit:    buildCommit,
			GoVersion: runtime.Version(),
		},
		Languages: entries,
		Limits: infoLimits{
			MaxSourceBytes: maxSourceBytes,
			MaxTests:       maxTests,
		},
		Stats: infoStats{
			JobsTotal: jobsTotal.Load(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
