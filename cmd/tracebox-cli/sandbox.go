package main

// This file implements the `tracebox start` / `tracebox stop` subcommands, which
// bring the Tracebox sandbox up and down with docker compose from any directory —
// the standing-operation counterpart to re-running the full tracebox.ps1 /
// tracebox.sh setup script every time.
//
// The CLI binary does not know where the Tracebox repo (and its
// docker-compose.yml) lives, so the setup scripts record the repo's absolute path
// in a small config file (see configPath / repoConfigKey). start/stop read that
// file to locate the compose project. If the file is missing — the CLI was
// installed but the setup script never ran here, or it ran on a different machine —
// the user gets a clear "run the setup script once" message rather than a cryptic
// failure.
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

// repoConfigKey is the key, in the ~/.tracebox/config file, under which the setup
// scripts record the absolute path of the Tracebox repo (the directory containing
// docker-compose.yml). The file is a simple `key=value` text file, one per line.
const repoConfigKey = "repo_path"

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

// readRepoPath reads the recorded repo path from the config file. A missing config
// file (the common "never set up here" case) returns a descriptive error pointing
// the user at the setup script; the caller turns that into a friendly message.
func readRepoPath() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("Tracebox repo location not found (%s does not exist).\n"+
				"Run tracebox.ps1 (Windows) or tracebox.sh (Linux/macOS) from the repo once to set this up.", path)
		}
		return "", fmt.Errorf("cannot read config file %s: %w", path, err)
	}
	defer f.Close()

	var repo string
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
		if strings.TrimSpace(key) == repoConfigKey {
			repo = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("cannot read config file %s: %w", path, err)
	}
	if repo == "" {
		return "", fmt.Errorf("config file %s does not record a %s.\n"+
			"Re-run tracebox.ps1 / tracebox.sh from the repo to set this up.", path, repoConfigKey)
	}

	compose := filepath.Join(repo, "docker-compose.yml")
	if _, err := os.Stat(compose); err != nil {
		return "", fmt.Errorf("recorded repo path %q has no docker-compose.yml (%v).\n"+
			"Re-run tracebox.ps1 / tracebox.sh from the current repo to refresh the path.", repo, err)
	}
	return repo, nil
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

	repo, err := readRepoPath()
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

	compose := filepath.Join(repo, "docker-compose.yml")
	fmt.Printf("Starting the Tracebox sandbox in %s mode...\n", mode)
	fmt.Println("(First build compiles the sandbox image and can take a few minutes.)")

	cmd := exec.Command("docker", "compose",
		"-f", compose,
		"--project-directory", repo,
		"up", "-d", "--build")
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

	repo, err := readRepoPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: %v\n", err)
		return exitUsage
	}
	if err := checkDocker(); err != nil {
		fmt.Fprintf(os.Stderr, "tracebox: %v\n", err)
		return exitAPI
	}

	compose := filepath.Join(repo, "docker-compose.yml")
	fmt.Println("Stopping the Tracebox sandbox (docker compose down)...")
	cmd := exec.Command("docker", "compose",
		"-f", compose,
		"--project-directory", repo,
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
