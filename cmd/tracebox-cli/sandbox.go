package main

// This file implements the `tracebox start` / `tracebox stop` subcommands, which
// bring the Tracebox sandbox up and down with docker compose from any directory —
// the standing-operation counterpart to re-running the full tracebox.ps1 /
// tracebox.sh setup script every time.
//
// The CLI binary does not know where the Tracebox compose project lives, so the
// setup scripts record its location in a small config file (see configPath). Two
// install shapes are supported, distinguished by which key the config carries:
//
//   repo_path     — REPO mode (git clone + build from source). Points at the repo
//                   directory; start builds the image locally (docker compose
//                   --build) from the repo's Dockerfile.
//   compose_file  — STANDALONE mode (no-clone, no-Go). Points directly at the
//                   minimal ~/.tracebox/docker-compose.yml the setup script wrote,
//                   which references the prebuilt ghcr.io/nym01/tracebox image;
//                   start pulls that image instead of building.
//
// compose_file takes precedence if both are present. start/stop read this to
// locate the compose project. If the file is missing — the CLI was installed but
// the setup script never ran here, or it ran on a different machine — the user
// gets a clear "run the setup script once" message rather than a cryptic failure.
//
// Like the rest of the CLI this stays a thin wrapper: it shells out to the same
// `docker compose` the setup scripts use and polls the same /healthz + /readyz
// endpoints. No sandbox logic lives here.

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config keys recorded in the ~/.tracebox/config file (a simple `key=value` text
// file, one per line). repoConfigKey is written by a git-clone setup (REPO mode);
// composeConfigKey is written by a standalone setup (STANDALONE mode). See the
// file-level comment for how they differ.
const (
	repoConfigKey    = "repo_path"
	composeConfigKey = "compose_file"
)

// healthTimeout bounds how long `tracebox start` waits for the API to become
// healthy after the containers come up. Matches the 120s used by tracebox.ps1 /
// tracebox.sh.
const healthTimeout = 120 * time.Second

// configPath returns the absolute path of the Tracebox CLI config file:
// %USERPROFILE%\.tracebox\config on Windows, ~/.tracebox/config elsewhere.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate your home directory: %w", err)
	}
	return filepath.Join(home, ".tracebox", "config"), nil
}

// composeConfig describes a resolved Tracebox compose project: where its
// docker-compose.yml is, what directory docker compose should treat as the
// project root, and whether `start` builds the image from source (repo mode) or
// runs the prebuilt published image (standalone mode).
type composeConfig struct {
	composeFile string // absolute path to docker-compose.yml (-f)
	projectDir  string // --project-directory
	build       bool   // true => `up --build` (repo mode); false => pull published image
}

// readComposeConfig reads ~/.tracebox/config and resolves the compose project.
// compose_file (standalone mode) wins over repo_path (repo mode) if both are set.
// A missing config file (the common "never set up here" case) returns a
// descriptive error pointing the user at the setup script; the caller turns that
// into a friendly message.
func readComposeConfig() (composeConfig, error) {
	path, err := configPath()
	if err != nil {
		return composeConfig{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return composeConfig{}, fmt.Errorf("Tracebox setup not found (%s does not exist).\n"+
				"Run tracebox.ps1 (Windows) or tracebox.sh (Linux/macOS) once to set this up.", path)
		}
		return composeConfig{}, fmt.Errorf("cannot read config file %s: %w", path, err)
	}
	defer f.Close()

	var repo, compose string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case repoConfigKey:
			repo = strings.TrimSpace(value)
		case composeConfigKey:
			compose = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return composeConfig{}, fmt.Errorf("cannot read config file %s: %w", path, err)
	}

	// Standalone mode: an explicit compose_file pointing at the prebuilt-image
	// compose the setup script wrote. Run it as-is — no local build.
	if compose != "" {
		if _, err := os.Stat(compose); err != nil {
			return composeConfig{}, fmt.Errorf("recorded %s %q is missing (%v).\n"+
				"Re-run tracebox.ps1 / tracebox.sh to refresh the standalone setup.", composeConfigKey, compose, err)
		}
		return composeConfig{
			composeFile: compose,
			projectDir:  filepath.Dir(compose),
			build:       false,
		}, nil
	}

	// Repo mode: build the image locally from the cloned repo.
	if repo != "" {
		composeFile := filepath.Join(repo, "docker-compose.yml")
		if _, err := os.Stat(composeFile); err != nil {
			return composeConfig{}, fmt.Errorf("recorded repo path %q has no docker-compose.yml (%v).\n"+
				"Re-run tracebox.ps1 / tracebox.sh from the current repo to refresh the path.", repo, err)
		}
		return composeConfig{
			composeFile: composeFile,
			projectDir:  repo,
			build:       true,
		}, nil
	}

	return composeConfig{}, fmt.Errorf("config file %s records neither %s nor %s.\n"+
		"Re-run tracebox.ps1 / tracebox.sh to set this up.", path, composeConfigKey, repoConfigKey)
}

// checkDocker confirms the docker CLI is installed and the daemon is responding,
// mirroring the check in tracebox.ps1 / tracebox.sh. It returns a friendly,
// actionable error otherwise.
func checkDocker() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("Docker not found - install it from https://docker.com/get-started,\n" +
			"make sure Docker Desktop is running, then try again.")
	}
	// `docker info` succeeds only when the daemon is actually running.
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Docker is installed but the daemon is not responding.\n" +
			"Start Docker Desktop, wait for it to finish starting, then try again.")
	}
	return nil
}

// apiBaseURL returns the API base URL the same way runCommand does, so start's
// health polling targets whatever TRACEBOX_API_URL points at.
func apiBaseURL() string {
	apiURL := strings.TrimRight(os.Getenv("TRACEBOX_API_URL"), "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	return apiURL
}

// startCommand implements `tracebox start [--strict]`. Without --strict it brings
// the sandbox up with the default nsjail backend; with --strict it sets
// GOBOXD_RUNNER=gvisor so docker compose starts the stronger-isolation gVisor
// backend (see docker-compose.yml's GOBOXD_RUNNER default).
func startCommand(args []string) int {
	strict := false
	for _, arg := range args {
		switch arg {
		case "--strict":
			strict = true
		default:
			fmt.Fprintf(os.Stderr, "tracebox: unknown argument %q for `start`\n", arg)
			fmt.Fprintln(os.Stderr, "Usage: tracebox start [--strict]")
			return exitUsage
		}
	}

	cfg, err := readComposeConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: %v\n", err)
		return exitUsage
	}
	if err := checkDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: %v\n", err)
		return exitAPI
	}

	mode := "nsjail (default)"
	runnerEnv := "nsjail"
	if strict {
		mode = "gvisor (--strict)"
		runnerEnv = "gvisor"
	}

	compose := cfg.composeFile
	fmt.Printf("Starting the Tracebox sandbox in %s mode...\n", mode)

	// Repo mode builds the image from source; standalone mode runs the prebuilt
	// published image (pulling it on first use). The compose args differ only by
	// the --build flag.
	upArgs := []string{"compose", "-f", compose, "--project-directory", cfg.projectDir, "up", "-d"}
	if cfg.build {
		upArgs = append(upArgs, "--build")
		fmt.Println("(First build compiles the sandbox image and can take a few minutes.)")
	} else {
		fmt.Println("(First start pulls the sandbox image and can take a few minutes.)")
	}

	cmd := exec.Command("docker", upArgs...)
	// Set GOBOXD_RUNNER explicitly so the chosen backend is deterministic
	// regardless of any value inherited from the caller's environment. The compose
	// file reads ${GOBOXD_RUNNER:-nsjail}, so this picks the backend directly.
	cmd.Env = append(os.Environ(), "GOBOXD_RUNNER="+runnerEnv)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: docker compose failed to start the sandbox: %v\n", err)
		return exitAPI
	}

	apiURL := apiBaseURL()
	fmt.Printf("Containers are up. Waiting for the API at %s to become healthy...\n", apiURL)
	healthOk, readyOk := waitForHealthy(apiURL, healthTimeout)

	fmt.Println()
	if !healthOk {
		fmt.Fprintf(os.Stderr, "tracebox: the API did not pass /healthz within %s.\n", healthTimeout)
		fmt.Fprintf(os.Stderr, "          Check the logs with:  docker compose -f %q logs\n", compose)
		return exitAPI
	}
	fmt.Println("========================================")
	fmt.Println(" Tracebox sandbox started")
	fmt.Println("========================================")
	fmt.Printf("  Mode        : %s\n", mode)
	fmt.Printf("  Sandbox API : running at %s\n", apiURL)
	if readyOk {
		fmt.Println("  Health      : /healthz and /readyz both OK")
	} else {
		fmt.Println("  Health      : /healthz OK, but /readyz is not fully ready (a language probe may be degraded)")
		fmt.Printf("                Check 'docker compose -f %q logs' if runs fail.\n", compose)
	}
	fmt.Println()
	fmt.Println("Run scripts from any directory:  tracebox run script.py")
	fmt.Println("Stop the sandbox with:           tracebox stop")
	return exitRan
}

// stopCommand implements `tracebox stop`: docker compose down using the recorded
// repo path.
func stopCommand(args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "tracebox: `stop` takes no arguments (got %q)\n", args[0])
		fmt.Fprintln(os.Stderr, "Usage: tracebox stop")
		return exitUsage
	}

	cfg, err := readComposeConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: %v\n", err)
		return exitUsage
	}
	if err := checkDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: %v\n", err)
		return exitAPI
	}

	compose := cfg.composeFile
	fmt.Println("Stopping the Tracebox sandbox (docker compose down)...")
	cmd := exec.Command("docker", "compose",
		"-f", compose,
		"--project-directory", cfg.projectDir,
		"down")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: docker compose failed to stop the sandbox: %v\n", err)
		return exitAPI
	}

	fmt.Println()
	fmt.Println("Tracebox sandbox stopped. Start it again with:  tracebox start")
	return exitRan
}

// waitForHealthy polls /healthz then /readyz until both return 200 or the timeout
// elapses, mirroring the loop in tracebox.ps1 / tracebox.sh. It reports whether
// each endpoint became healthy. A degraded /readyz (healthOk true, readyOk false)
// is not treated as a hard failure, matching the setup scripts.
func waitForHealthy(apiURL string, timeout time.Duration) (healthOk, readyOk bool) {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !healthOk && probe(client, apiURL+"/healthz") {
			healthOk = true
			fmt.Println("  /healthz is up.")
		}
		if healthOk && !readyOk && probe(client, apiURL+"/readyz") {
			readyOk = true
			fmt.Println("  /readyz reports ready.")
		}
		if healthOk && readyOk {
			return healthOk, readyOk
		}
		time.Sleep(3 * time.Second)
	}
	return healthOk, readyOk
}

// probe returns true when a GET to url returns HTTP 200.
func probe(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
