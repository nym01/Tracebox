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
	"github.com/nym01/goboxd/internal/store"
	"github.com/nym01/goboxd/internal/tracer"
)

// defaultDBPath is where run data is persisted when TRACEBOX_DB_PATH is unset.
// In the container /data is a mounted volume (see docker-compose.yml) so the
// audit trail survives restarts.
const defaultDBPath = "/data/tracebox.db"

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
// "nsjail" uses NsjailRunner; "gvisor" uses GvisorRunner (py3-only, Phase 7
// Stage 1); anything else (including unset) keeps the SubprocessRunner default so
// existing behavior is unchanged until a sandbox is explicitly opted into.
// Constructing the nsjail and gvisor runners validates their dependencies up front
// (nsjail resolves its mount profiles; gvisor probes the runsc binary and the
// shared py3 rootfs), so each returns an error the caller should treat as fatal.
func selectRunner() (runner.Runner, error) {
	switch os.Getenv("GOBOXD_RUNNER") {
	case "nsjail":
		path := os.Getenv("GOBOXD_NSJAIL_PATH")
		if path == "" {
			path = "/usr/local/bin/nsjail"
		}
		log.Printf("runner: nsjail (%s)", path)
		return runner.NewNsjailRunner(context.Background(), path)
	case "gvisor":
		path := os.Getenv("GOBOXD_RUNSC_PATH")
		if path == "" {
			path = "/usr/local/bin/runsc"
		}
		log.Printf("runner: gvisor (%s)", path)
		return runner.NewGvisorRunner(context.Background(), path)
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

	// Start the eBPF syscall tracer (Phase 4): file opens (openat/openat2),
	// process spawns (execve/execveat) and network connects (connect). It attaches
	// once for the process lifetime and captures what each sandboxed run opens,
	// spawns and tries to connect to. It needs a privileged
	// Linux container with BTF; if it cannot start (non-privileged dev, non-Linux,
	// missing capabilities) the server runs normally with tracing disabled rather
	// than failing — the sandbox itself does not depend on it. Set GOBOXD_TRACER=off
	// to skip it entirely (used for A/B overhead measurement and incident triage).
	if os.Getenv("GOBOXD_TRACER") == "off" {
		log.Println("tracer: disabled via GOBOXD_TRACER=off")
	} else if t, err := tracer.Start(); err != nil {
		log.Printf("tracer: syscall tracing disabled: %v", err)
	} else {
		defer t.Stop()
		api.SetTracer(t)
		log.Println("tracer: syscall tracing enabled (file-open + exec + connect)")
	}

	// Open the SQLite run store for the queryable audit trail (Phase 5). Like the
	// tracer, this is additive: if it cannot be opened the server runs normally
	// with persistence disabled (stdout logging is unaffected). GET /runs and
	// GET /runs/{run_id} then report no data rather than failing.
	dbPath := os.Getenv("TRACEBOX_DB_PATH")
	if dbPath == "" {
		dbPath = defaultDBPath
	}
	if st, err := store.Open(dbPath); err != nil {
		log.Printf("store: run persistence disabled: %v", err)
	} else {
		defer st.Close()
		api.SetStore(st)
		log.Printf("store: run persistence enabled (%s)", dbPath)
	}

	api.InitReadyz(nsjailPath)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	log.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", api.WithCORS(mux)); err != nil {
		log.Fatal(err)
	}
}
