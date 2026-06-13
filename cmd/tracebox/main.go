package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nym01/goboxd/internal/api"
	"github.com/nym01/goboxd/internal/language"
	"github.com/nym01/goboxd/internal/runner"
)

// Commit is injected at build time via -ldflags "-X main.Commit=$(git rev-parse --short HEAD)".
var Commit = "dev"

const jailDirPrefix = "goboxd-"
const jailMaxAge = 10 * time.Minute

// sweepOrphanedJails removes subdirectories of baseDir whose names start with
// prefix and whose modification time is older than maxAge. It is called at
// startup to clean up temp directories left behind by a previous process crash.
func sweepOrphanedJails(baseDir, prefix string, maxAge time.Duration) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		log.Printf("startup sweep: cannot read %s: %v", baseDir, err)
		return
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if removeErr := os.RemoveAll(filepath.Join(baseDir, e.Name())); removeErr == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("startup sweep: removed %d orphaned jail directories from %s", removed, baseDir)
	}
}

// selectRunner picks the sandbox implementation from the GOBOXD_RUNNER env var.
// "nsjail" uses NsjailRunner; anything else (including unset) keeps the
// SubprocessRunner default so existing behavior is unchanged until nsjail is
// explicitly opted into. Constructing the nsjail runner resolves its py3 mount
// profile up front, so it returns an error the caller should treat as fatal.
func selectRunner() (runner.Runner, error) {
	switch os.Getenv("GOBOXD_RUNNER") {
	case "nsjail":
		path := os.Getenv("GOBOXD_NSJAIL_PATH")
		if path == "" {
			path = "/usr/local/bin/nsjail"
		}
		log.Printf("runner: nsjail (%s)", path)
		return runner.NewNsjailRunner(context.Background(), path)
	default:
		log.Println("runner: subprocess")
		return runner.SubprocessRunner{}, nil
	}
}

func main() {
	sweepOrphanedJails(os.TempDir(), jailDirPrefix, jailMaxAge)

	if err := language.LoadRegistry("configs/languages.yaml"); err != nil {
		log.Fatalf("startup: %v", err)
	}
	nsjailPath := os.Getenv("GOBOXD_NSJAIL_PATH")
	if nsjailPath == "" {
		nsjailPath = "/usr/local/bin/nsjail"
	}
	api.SetBuildCommit(Commit)
	r, err := selectRunner()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	api.SetRunner(r)
	api.InitReadyz(nsjailPath)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	log.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
