package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GvisorRunner runs the command inside a gVisor (runsc) sandbox. Like
// NsjailRunner it implements the Runner interface and returns the identical
// RunResult shape, so the API handler can swap to it via GOBOXD_RUNNER=gvisor
// without any caller changes. It is purely additive: NsjailRunner and
// SubprocessRunner are untouched and remain the default.
//
// This is the Phase 7 Stage 1 integration: py3 ONLY. The other six languages
// return a clear "not yet supported" error from Run (see gvisorLanguageSupported)
// rather than silently falling back or failing confusingly — multi-language
// rootfs support is deliberately future work (see experiments/gvisor-poc/FINDINGS.md
// §6 "Explicitly deferred").
//
// Why gVisor, and why now: the security audit's Finding F (the /proc host-info
// leak — host kernel version, CPU model, live host loadavg) is left as a known
// limitation under nsjail because masking those files reliably risks breaking the
// managed runtimes that read them. gVisor's sentry presents a *synthesised* /proc,
// closing that whole class with no per-file masking. The POC validated this end to
// end; this runner proves it inside the real service.
//
// Structural difference from nsjail. nsjail synthesises a minimal mount namespace
// by bind-mounting individual host files per language (the resolve*Mounts logic in
// nsjail.go). gVisor instead runs the container against a populated *rootfs* tree.
// So, mirroring NewNsjailRunner's "resolve once at startup, fail loud" pattern, a
// single shared py3 rootfs is prepared in the image build (see the Dockerfile) and
// validated once here; each request only writes a tiny per-request OCI bundle
// (config.json) that points root at that shared read-only rootfs and bind-mounts
// the per-request work directory (where the handler already wrote the source file)
// writable at gvisorWorkDir. The work directory is the per-request analogue of
// nsjail's writable --bindmount work dir.
type GvisorRunner struct {
	// RunscPath is the path to the runsc binary. Defaults to "runsc" (resolved
	// via PATH) when empty.
	RunscPath string

	// Py3RootfsPath is the absolute path of the shared, read-only python3 root
	// filesystem prepared once in the image build. Every py3 request runs against
	// it (root.readonly = true), so concurrent runs share one read-only tree —
	// exactly how the runsc Docker runtime shares image layers. Populated and
	// validated by NewGvisorRunner; an empty value means the zero-value runner
	// used only by the compile-time assertions.
	Py3RootfsPath string
}

const (
	// gvisorPlatform is the gVisor platform passed via --platform. systrap traps
	// guest syscalls with seccomp+signals entirely in userspace and needs no
	// /dev/kvm, which Docker Desktop does not expose in WSL2 (POC §1). On a real
	// Linux host with /dev/kvm the faster KVM platform would also be available —
	// a performance upside, not a correctness requirement.
	gvisorPlatform = "systrap"

	// gvisorWorkDir is the in-sandbox mount point for the per-request work
	// directory. The handler writes the source file into spec.WorkDir and passes
	// the source as a relative arg (e.g. "solution.py") plus Cmd="/usr/bin/python3";
	// binding WorkDir here and setting process.cwd to it makes those resolve
	// exactly as they do under nsjail's --cwd + writable work-dir bind.
	gvisorWorkDir = "/work"

	// gvisorSentryMemHeadroomKB is the extra memory budget added to the guest's
	// MemoryKB when sizing the OCI memory limit. Unlike nsjail (where the cgroup
	// memory.max applies to the guest alone), gVisor's cgroup limit covers the
	// sentry process AND the guest combined, and the sentry has a substantial
	// footprint of its own: the POC (§4) found a 256 MB *total* limit was too small
	// for the sandbox to even start ("waiting for sandbox to start: EOF"), while
	// 512 MB started and correctly OOM-killed a 3 GB bomb (exit 137). The sentry's
	// own footprint therefore sits somewhere in the 256–512 MB band in this
	// environment, so we budget a flat 512 MB of headroom above the guest's
	// MemoryKB. This guarantees the sentry always has room to start regardless of
	// the per-language guest budget (64–512 MB across languages), trading some
	// precision in the guest's *effective* cap for reliable startup. CAVEAT: it
	// also means the guest's visible /proc/meminfo MemTotal is (MemoryKB + headroom),
	// and a guest can allocate up to roughly that before the OOM killer fires — a
	// looser real cap than nsjail's. That is acceptable for this stage (the
	// memory-bomb test allocates 3 GB, far above any limit+headroom) and is the
	// kind of tuning the POC flags for a real-Linux retest, where the lower
	// (KVM-platform) sentry overhead allows a tighter headroom.
	gvisorSentryMemHeadroomKB = 512 * 1024

	// gvisorWallGraceSec is added to the guest wall-time budget when sizing the Go
	// context deadline. Unlike nsjail (which enforces --time_limit on the guest
	// internally, with the context only a backstop), runsc has no built-in
	// wall-time limit, so the Go context is the SOLE enforcer. The grace covers
	// gVisor's out-of-guest startup overhead (sentry+gofer bring-up, which is
	// noticeably higher without the KVM platform — POC §6) so a fast program that
	// finishes well inside its guest budget is never falsely flagged time_exceeded;
	// the cost is that a genuine runaway is killed up to grace seconds late.
	gvisorWallGraceSec = 10
)

// defaultGvisorRootfsPath is where the image build stages the shared py3 rootfs
// (see the Dockerfile). Overridable via GOBOXD_GVISOR_ROOTFS for testing or an
// alternative layout.
const defaultGvisorRootfsPath = "/opt/gvisor/rootfs/py3"

// NewGvisorRunner constructs a GvisorRunner with its dependencies validated up
// front, mirroring NewNsjailRunner: it confirms the runsc binary is present and
// runnable (runsc --version) and that the shared py3 rootfs exists, so a broken
// gVisor deployment fails loudly at startup rather than on the first request. Any
// error here is meant to be fatal to startup, exactly like a failed nsjail mount
// resolution.
func NewGvisorRunner(ctx context.Context, runscPath string) (GvisorRunner, error) {
	if runscPath == "" {
		runscPath = "runsc"
	}

	// Fail loud if runsc is missing or non-functional. runsc --version is a cheap
	// liveness probe that does not need cgroups, KVM or a bundle.
	if out, err := exec.CommandContext(ctx, runscPath, "--version").CombinedOutput(); err != nil {
		return GvisorRunner{}, fmt.Errorf("gvisor runner: runsc not available (%s --version): %w: %s",
			runscPath, err, strings.TrimSpace(string(out)))
	}

	rootfs := os.Getenv("GOBOXD_GVISOR_ROOTFS")
	if rootfs == "" {
		rootfs = defaultGvisorRootfsPath
	}
	abs, err := filepath.Abs(rootfs)
	if err != nil {
		return GvisorRunner{}, fmt.Errorf("gvisor runner: resolve rootfs path %q: %w", rootfs, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return GvisorRunner{}, fmt.Errorf("gvisor runner: py3 rootfs %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return GvisorRunner{}, fmt.Errorf("gvisor runner: py3 rootfs %s is not a directory", abs)
	}
	// The rootfs must actually contain the python3 the language registry invokes
	// (/usr/bin/python3), or every request would fail at execve. Checking here keeps
	// the failure at startup.
	if _, err := os.Stat(filepath.Join(abs, "usr", "bin", "python3")); err != nil {
		return GvisorRunner{}, fmt.Errorf("gvisor runner: py3 rootfs %s missing usr/bin/python3: %w", abs, err)
	}

	return GvisorRunner{
		RunscPath:     runscPath,
		Py3RootfsPath: abs,
	}, nil
}

// gvisorLanguageSupported reports whether the command is one this stage's
// GvisorRunner can run. Only the python3 interpreter is supported; everything else
// (the compiler build/run steps for cpp/c/java/verilog, bash, node) is deliberately
// out of scope for Stage 1 and gets a clear error from Run rather than a silent
// fallback or a confusing failure. pythonInterpreter is defined in nsjail.go.
func gvisorLanguageSupported(cmd string) bool {
	return cmd == pythonInterpreter
}

func (r GvisorRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	if !gvisorLanguageSupported(spec.Cmd) {
		// A clean, explicit error — no fallback to nsjail/subprocess, no crash. The
		// handler maps a runner error to internal_error (build phase) or the test's
		// internal_error status (run phase), which is an acceptable "clean error"
		// for an unsupported language this stage. Multi-language rootfs support is
		// future work (see the type doc and FINDINGS.md §6).
		return RunResult{}, fmt.Errorf("gvisor runner: language not yet supported in this stage (only py3); command %q", spec.Cmd)
	}

	wallSec := spec.WallTimeSec
	if wallSec <= 0 {
		wallSec = 10
	}

	runscPath := r.RunscPath
	if runscPath == "" {
		runscPath = "runsc"
	}

	// Per-request OCI bundle directory. It holds only the tiny config.json; the
	// rootfs is the shared read-only tree, referenced by absolute path, not copied.
	bundleDir, err := os.MkdirTemp("", "goboxd-gvisor-")
	if err != nil {
		return RunResult{}, fmt.Errorf("gvisor runner: create bundle dir: %w", err)
	}
	defer os.RemoveAll(bundleDir)

	// The container id must be unique and stable for run + cleanup; the bundle dir's
	// basename is both.
	containerID := filepath.Base(bundleDir)

	ociSpec := r.buildOCISpec(spec, containerID)
	specBytes, err := json.MarshalIndent(ociSpec, "", "  ")
	if err != nil {
		return RunResult{}, fmt.Errorf("gvisor runner: marshal OCI spec: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), specBytes, 0600); err != nil {
		return RunResult{}, fmt.Errorf("gvisor runner: write config.json: %w", err)
	}

	// The Go context is the sole wall-time enforcer (runsc has no internal
	// --time_limit). Deadline = guest budget + startup grace so a fast program is
	// not falsely timed out by sentry bring-up overhead.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(wallSec+gvisorWallGraceSec)*time.Second)
	defer cancel()

	// runsc global flags precede the subcommand. --network=none gives the guest no
	// network (the sandbox is offline, as under nsjail's empty net namespace);
	// --platform selects systrap (no KVM needed). NOT --ignore-cgroups: we WANT the
	// cgroup so memory/cpu limits and the synthesised /proc meminfo apply (POC §3/§4),
	// which requires the container to run with --cgroupns=host (already required for
	// nsjail; see docker-compose.yml).
	args := []string{
		"--platform=" + gvisorPlatform,
		"--network=none",
		"run",
		"--bundle", bundleDir,
		containerID,
	}

	cmd := exec.CommandContext(runCtx, runscPath, args...)
	cmd.Stdin = strings.NewReader(spec.Stdin)

	outW := &cappedWriter{limit: outputCap}
	errW := &cappedWriter{limit: outputCap}
	cmd.Stdout = outW
	cmd.Stderr = errW

	// NOTE on the eBPF tracer: spec.OnStart is deliberately NOT called here. The
	// tracer attaches to HOST syscall tracepoints filtered by cgroup, but under
	// gVisor the guest's syscalls are serviced by the sentry and never hit those
	// host tracepoints (POC §5), so attaching would capture nothing useful for the
	// guest. Skipping attachment entirely is the simplest correct behaviour for this
	// stage: gVisor runs simply produce no trace events (the handler's traceRun
	// stays empty, which emitTraceEvents/persistRun handle as a no-op). A gVisor-native
	// trace source (runsc trace points) is future work.

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}
	waitErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()

	// Best-effort teardown of the sandbox's runtime state. On a clean exit runsc run
	// already removes it; on a wall-time kill (we SIGKILLed runsc via the context)
	// it can linger, so delete --force unconditionally with a fresh short context
	// (runCtx may already be cancelled). Errors are ignored: a missing container is
	// the expected case after a clean run.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = exec.CommandContext(cleanupCtx, runscPath,
		"--platform="+gvisorPlatform, "--network=none",
		"delete", "--force", containerID).Run()
	cleanupCancel()

	var exitCode int
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// Timeout detection. With no internal runsc limit, a DeadlineExceeded on our
	// context is the only timeout signal (we do NOT use nsjail's "duration >= wall"
	// heuristic, which would misfire given the added startup grace).
	timedOut := runCtx.Err() == context.DeadlineExceeded

	res := RunResult{
		Stdout:     string(outW.Bytes()),
		Stderr:     string(errW.Bytes()),
		DurationMs: durationMs,
		// MemoryPeakKB is not reported: like nsjail, the guest's cgroup is torn down
		// with the sandbox before we can read memory.peak, and ProcessState reflects
		// the runsc wrapper, not the guest. Left at 0.
		MemoryPeakKB: 0,
		ExitCode:     exitCode,
		TimedOut:     timedOut,
		// memory_exceeded detection: identical to nsjail. The POC confirmed a guest
		// OOM under the cgroup limit surfaces as exit 137 here too, so the shared
		// oomKilled() mapping (a non-timeout SIGKILL while a memory limit was in
		// force) applies unchanged. spec.MemoryKB (the guest budget) is the >0 flag,
		// independent of the headroom we add to the actual cgroup limit.
		MemoryExceeded: oomKilled(exitCode, timedOut, spec.MemoryKB),
	}

	if timedOut {
		return res, nil
	}
	// A non-zero guest exit arrives as *exec.ExitError; that is a normal result
	// (runtime error), not a runner failure. Only surface other errors.
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return res, waitErr
		}
	}
	return res, nil
}

// buildOCISpec assembles the OCI runtime spec (config.json) for one request. It is
// split out from Run (which only writes and execs it) so the spec construction — in
// particular the resource-limit translation and the py3 rootfs/work-dir wiring — is
// unit-testable without invoking runsc. containerID feeds the per-run cgroupsPath so
// concurrent runs get distinct cgroups under the host hierarchy. (The wall-time
// limit is not part of the spec — runsc has no in-spec time limit — so it is not
// a parameter here; Run enforces it via the Go context.)
func (r GvisorRunner) buildOCISpec(spec RunSpec, containerID string) ociSpec {
	s := ociSpec{
		OCIVersion: "1.0.0",
		Process: ociProcess{
			Terminal: false,
			User:     ociUser{UID: 0, GID: 0},
			// Cmd + Args verbatim: e.g. ["/usr/bin/python3", "solution.py"]. The
			// source is a relative path resolved against Cwd (the work-dir bind).
			Args: append([]string{spec.Cmd}, spec.Args...),
			Env: []string{
				"PATH=/usr/local/bin:/usr/bin:/bin",
				"HOME=/",
				"LANG=C.UTF-8",
			},
			Cwd:             gvisorWorkDir,
			NoNewPrivileges: true,
			Rlimits: []ociRlimit{
				{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024},
			},
		},
		Root: ociRoot{
			// Absolute path to the shared py3 rootfs, read-only. Shared safely across
			// concurrent runs (the gofer opens it read-only); per-request writes go to
			// the work-dir bind and the tmpfs /tmp below.
			Path:     r.Py3RootfsPath,
			Readonly: true,
		},
		Hostname: "sandbox",
		Mounts: []ociMount{
			// Synthesised procfs — this is what fixes Finding F: /proc/version,
			// /proc/cpuinfo, /proc/loadavg etc. are served by the sentry, not the host.
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs",
				Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "nodev", "mode=1777", "size=65536k"}},
			// The per-request work directory, writable. This is where the handler wrote
			// the source file and where the program may create artifacts — the gVisor
			// analogue of nsjail's writable --bindmount work dir.
			{Destination: gvisorWorkDir, Type: "bind", Source: spec.WorkDir,
				Options: []string{"rbind", "rw"}},
		},
		Linux: ociLinux{
			Namespaces: []ociNamespace{
				{Type: "pid"},
				{Type: "network"},
				{Type: "ipc"},
				{Type: "uts"},
				{Type: "mount"},
			},
			// Per-run cgroup under the host v2 hierarchy (requires --cgroupns=host).
			CgroupsPath: "/goboxd-gvisor/" + containerID,
			Resources:   gvisorResources(spec),
		},
	}
	return s
}

// gvisorResources translates the RunSpec limits into an OCI linux.resources block,
// or nil when no limit is set (the zero-value runner / unlimited request). The
// translation mirrors nsjail's cgroup v2 mapping with two gVisor-specific
// adjustments documented inline.
func gvisorResources(spec RunSpec) *ociResources {
	if spec.MemoryKB <= 0 && spec.CPUMsPerSec <= 0 {
		return nil
	}
	res := &ociResources{}

	if spec.MemoryKB > 0 {
		// Guest budget + sentry headroom (see gvisorSentryMemHeadroomKB): the gVisor
		// cgroup limit covers sentry+guest combined, unlike nsjail's guest-only
		// memory.max. swap is pinned equal to limit (OCI swap is memory+swap total) so
		// the cap is hard, matching nsjail's --cgroup_mem_swap_max 0 intent; WSL2 has
		// no swap controller, but gVisor also bounds guest RAM internally, which is
		// what drives the OOM (POC §4).
		limitBytes := int64(spec.MemoryKB+gvisorSentryMemHeadroomKB) * 1024
		res.Memory = &ociMemory{Limit: &limitBytes, Swap: &limitBytes}
	}

	if spec.CPUMsPerSec > 0 {
		// cpu.max equivalent: quota microseconds of CPU per period microseconds.
		// CPUMsPerSec ms per 1 s == (CPUMsPerSec*1000) µs per 1_000_000 µs — the same
		// bandwidth nsjail writes via --cgroup_cpu_ms_per_sec. gVisor honours it and
		// also derives the guest's visible cpu count from it (POC §3/§4).
		quota := int64(spec.CPUMsPerSec) * 1000
		period := uint64(1_000_000)
		res.CPU = &ociCPU{Quota: &quota, Period: &period}
	}

	// NOTE: spec.MaxProcesses (pids.limit) is intentionally NOT translated in this
	// stage. The POC (§4/§6) found that setting an OCI pids limit deterministically
	// trips a sentry-startup failure in this WSL2 environment — the sentry is itself
	// multi-threaded and forks a `umounter` helper at bring-up (fork/exec EAGAIN /
	// "failed to create new OS thread" / fatal newosproc), so the cap must budget for
	// the sentry's own tasks, not just the guest's. Rather than risk flaky startup,
	// the pids limit is deferred to a real-Linux retest with a generous budget. This
	// is a documented gap for the gVisor backend, narrower than nsjail's per-guest
	// pids.max; it does not affect the memory/cpu limits or Finding F.

	return res
}

// --- Minimal OCI runtime-spec types -----------------------------------------
//
// Only the subset of the OCI runtime spec this runner sets is modelled; omitempty
// keeps config.json close to what `runsc spec` emits while letting us construct it
// directly (testable, and no dependency on shelling out to `runsc spec` per request).

type ociSpec struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname,omitempty"`
	Mounts     []ociMount `json:"mounts,omitempty"`
	Linux      ociLinux   `json:"linux"`
}

type ociProcess struct {
	Terminal        bool        `json:"terminal"`
	User            ociUser     `json:"user"`
	Args            []string    `json:"args"`
	Env             []string    `json:"env"`
	Cwd             string      `json:"cwd"`
	NoNewPrivileges bool        `json:"noNewPrivileges"`
	Rlimits         []ociRlimit `json:"rlimits,omitempty"`
}

type ociUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type ociRlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces  []ociNamespace `json:"namespaces,omitempty"`
	Resources   *ociResources  `json:"resources,omitempty"`
	CgroupsPath string         `json:"cgroupsPath,omitempty"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

type ociResources struct {
	Memory *ociMemory `json:"memory,omitempty"`
	CPU    *ociCPU    `json:"cpu,omitempty"`
}

type ociMemory struct {
	Limit *int64 `json:"limit,omitempty"`
	Swap  *int64 `json:"swap,omitempty"`
}

type ociCPU struct {
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`
}
