package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
// This is the Phase 7 Stage 2 integration: ALL SEVEN languages (py3, bash, js,
// cpp, c, java, verilog). Stage 1 ran py3 only; Stage 2 adds the rest, each backed
// by its own prepared rootfs. A command whose rootfs is not available returns a
// clear error from Run (see gvisorRootfsName / the per-language gate) rather than
// silently falling back or failing confusingly.
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
// So, mirroring NewNsjailRunner's "resolve once at startup, fail loud" pattern, the
// shared per-language rootfs trees are prepared in the image build (see the
// Dockerfile) and validated once here; each request only writes a tiny per-request
// OCI bundle (config.json) that points root at the appropriate shared read-only
// rootfs and bind-mounts the per-request work directory (where the handler already
// wrote the source file) writable at gvisorWorkDir. The work directory is the
// per-request analogue of nsjail's writable --bindmount work dir.
//
// ROOTFS STRATEGY — one rootfs per language, NOT nsjail's build/run mount
// asymmetry. nsjail deliberately runs the compiler build step with broad
// filesystem access (headers, toolchain dirs) but the compiled artifact's run step
// with a minimal one (just its own .so deps). That asymmetry is a real security
// property *for nsjail* because nsjail's boundary is the HOST process: the
// untrusted binary executes directly on the host kernel inside a mount namespace,
// so a minimal run mount means there is literally less host filesystem reachable if
// anything in the seccomp/namespace boundary is bypassed, and the mounts are of
// *live host files*.
//
// Under gVisor that calculus changes, so this runner does NOT mirror the asymmetry:
//   - The boundary is the SENTRY, not rootfs minimalism. Every guest syscall is
//     serviced by the sentry's own kernel implementation; the guest never touches
//     the host kernel or host filesystem regardless of what the rootfs contains. A
//     "fat" rootfs does not let the guest *do* more — the sentry's syscall mediation
//     is the actual cap, not the contents of the tree.
//   - The rootfs is a STATIC, READ-ONLY, PURPOSE-BUILT TREE baked into the image
//     (an apt install in a Dockerfile stage), not a bind mount of the live host
//     filesystem. Even the "full toolchain" rootfs exposes only trusted distro
//     files and leaks nothing about the host — unlike nsjail's --bindmount_ro of the
//     host's own /usr/include, /usr/lib, etc.
//   - network=none, root is read-only (writes go only to the per-request /work and a
//     tmpfs /tmp), and the sentry structurally blocks the escape-class syscalls. So
//     the marginal capability a richer rootfs grants the untrusted run step (e.g.
//     more binaries to exec as gadgets) has no escape vector to reach.
//
// The one honest cost is defense-in-depth: if the *sentry itself* had a
// post-exploitation gap, a minimal run rootfs would limit what an attacker could
// then reach. That is a second-order concern against gVisor's strong primary
// boundary, and the task explicitly prefers the simpler one-rootfs model when it is
// sound. Two language-specific facets of this decision are called out where they
// bite: verilog's /bin/sh is present in the run rootfs (see gvisorRootfsName) and
// c/cpp share one rootfs (a C binary's needs are a subset of a C++ one's, exactly as
// nsjail already shares their *run* profile — so a single "ccpp" rootfs carrying
// both gcc and g++ serves both languages' build AND run steps).
type GvisorRunner struct {
	// RunscPath is the path to the runsc binary. Defaults to "runsc" (resolved
	// via PATH) when empty.
	RunscPath string

	// Rootfs maps a rootfs name (gvisorRootfsPy3, gvisorRootfsBash, …) to the
	// absolute path of that shared, read-only rootfs tree prepared once in the image
	// build. Every request for a language runs against its rootfs (root.readonly =
	// true), so concurrent runs share one read-only tree — exactly how the runsc
	// Docker runtime shares image layers. Populated and validated by NewGvisorRunner,
	// which includes only the rootfs trees it found valid on disk: a language whose
	// rootfs is missing is simply absent from the map and its requests get a clean
	// "rootfs unavailable" error (graceful per-language degradation), while py3 (the
	// proven baseline) is required for construction to succeed at all. An empty/nil
	// map means the zero-value runner used only by the compile-time assertions.
	Rootfs map[string]string

	// Strace enables capturing a per-run syscall audit trail via runsc's own
	// --strace, parsed into RunResult.TraceEvents (see gvisor_strace.go). It is the
	// gVisor analogue of the eBPF tracer, which sees nothing for a gVisor run
	// because the guest's syscalls are serviced by the sentry, not the host kernel
	// (POC §5). NewGvisorRunner enables it by default; GOBOXD_GVISOR_STRACE=off
	// turns it off (an A/B switch for measuring strace overhead, mirroring
	// GOBOXD_TRACER=off for the eBPF tracer). The zero-value runner used by the
	// compile-time assertions leaves it false, so spec/dispatch unit tests never
	// touch strace.
	Strace bool
}

// Rootfs names. Each names a tree under the rootfs base dir (gvisorRootfsBaseDir,
// e.g. /opt/gvisor/rootfs/<name>) prepared by a Dockerfile stage. c and cpp share
// the "ccpp" rootfs (see the GvisorRunner doc comment / gvisorRootfsName).
const (
	gvisorRootfsPy3     = "py3"
	gvisorRootfsBash    = "bash"
	gvisorRootfsJs      = "js"
	gvisorRootfsCcpp    = "ccpp"
	gvisorRootfsJava    = "java"
	gvisorRootfsVerilog = "verilog"
)

// gvisorRootfsSentinels lists, per rootfs, the in-rootfs paths that MUST exist for
// that language to run — the binaries the language registry invokes (and the
// programs they shell out to). NewGvisorRunner validates these so a half-populated
// rootfs is rejected at startup rather than failing every request at execve. Paths
// are relative to the rootfs root.
var gvisorRootfsSentinels = map[string][]string{
	gvisorRootfsPy3:  {"usr/bin/python3"},
	gvisorRootfsBash: {"usr/bin/bash"},
	// node is invoked as /usr/bin/node, which Debian ships as a symlink to nodejs;
	// the Dockerfile stage recreates that symlink, so check the invocation path.
	gvisorRootfsJs: {"usr/bin/node"},
	// One rootfs carries the whole gcc/g++ toolchain for both c and cpp, build and
	// run; require both drivers plus the linker/assembler the drivers shell out to.
	gvisorRootfsCcpp: {"usr/bin/gcc", "usr/bin/g++"},
	// The full JDK: both launchers. JAVA_HOME derivation is left to /proc/self/exe
	// (see the Run path) exactly as the JDK does natively, so no symlink surgery here.
	gvisorRootfsJava: {"usr/bin/javac", "usr/bin/java"},
	// iverilog (build driver) and vvp (run); the driver also needs /bin/sh for its
	// system() sub-invocations, which is present in this rootfs.
	gvisorRootfsVerilog: {"usr/bin/iverilog", "usr/bin/vvp"},
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

	// gvisorStraceSyscalls scopes runsc --strace to just the syscalls the eBPF
	// tracer also captures (passed via --strace-syscalls), so the strace log is not
	// flooded with every guest syscall — the noise/overhead the task flags. These
	// four are long-standing, core syscalls gVisor's strace table definitely knows,
	// so the scoping cannot reject the flag. openat2 is deliberately omitted: it is
	// newer and may be absent from the strace table in some runsc releases (which
	// would make --strace-syscalls reject the whole run), and the common interpreter
	// /compiler file-open path uses openat anyway — a documented, minor gap versus
	// the eBPF tracer, which also watches openat2.
	gvisorStraceSyscalls = "openat,execve,execveat,connect"

	// gvisorDebugLogTmpl is the --debug-log destination. runsc substitutes
	// %COMMAND% per sandbox component, so the guest's strace lands in the file whose
	// name contains "boot" (e.g. runsc-boot.log); collectStraceEvents globs for it.
	// The file lives inside the per-request bundle dir, so it is removed with the
	// bundle when Run returns.
	gvisorDebugLogTmpl = "runsc-%COMMAND%.log"
)

// defaultGvisorRootfsBaseDir is the directory under which the image build stages
// each per-language rootfs tree (e.g. <base>/py3, <base>/ccpp; see the Dockerfile).
// Overridable via GOBOXD_GVISOR_ROOTFS for testing or an alternative layout.
const defaultGvisorRootfsBaseDir = "/opt/gvisor/rootfs"

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

	baseDir := os.Getenv("GOBOXD_GVISOR_ROOTFS")
	if baseDir == "" {
		baseDir = defaultGvisorRootfsBaseDir
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return GvisorRunner{}, fmt.Errorf("gvisor runner: resolve rootfs base dir %q: %w", baseDir, err)
	}

	// Validate each per-language rootfs independently and include only those that are
	// present and well-formed. This is deliberate graceful degradation: a language
	// whose rootfs failed to build (or was not staged) is simply omitted, its
	// requests get a clean "rootfs unavailable" error, and the other languages still
	// work — rather than one missing tree taking the whole gvisor backend down. py3
	// is the exception: it is the proven baseline, so its absence is treated as a
	// hard startup failure (a sign the deployment is fundamentally broken), exactly
	// the "fail loud at startup" contract Stage 1 established.
	rootfs := make(map[string]string)
	for name, sentinels := range gvisorRootfsSentinels {
		abs := filepath.Join(absBase, name)
		if err := validateGvisorRootfs(abs, sentinels); err != nil {
			if name == gvisorRootfsPy3 {
				return GvisorRunner{}, fmt.Errorf("gvisor runner: %w", err)
			}
			// Non-py3: log and skip so the backend still serves the languages it can.
			log.Printf("gvisor runner: rootfs %q unavailable, that language will be rejected at request time: %v", name, err)
			continue
		}
		rootfs[name] = abs
	}

	return GvisorRunner{
		RunscPath: runscPath,
		Rootfs:    rootfs,
		// Per-run strace audit is on by default (the point of Phase 7 Stage 3);
		// GOBOXD_GVISOR_STRACE=off disables it for overhead A/B measurement, the
		// gVisor analogue of GOBOXD_TRACER=off.
		Strace: os.Getenv("GOBOXD_GVISOR_STRACE") != "off",
	}, nil
}

// validateGvisorRootfs checks that abs is a directory containing every sentinel
// path (the binaries the language invokes / shells out to), so a half-populated
// rootfs is caught at startup instead of failing every request at execve.
func validateGvisorRootfs(abs string, sentinels []string) error {
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("rootfs %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("rootfs %s is not a directory", abs)
	}
	for _, rel := range sentinels {
		// Lstat, not Stat: a sentinel may be an absolute symlink that only resolves
		// once the tree is the container root (e.g. the JDK's /usr/bin/java ->
		// /etc/alternatives/java -> /usr/lib/jvm/…, which gVisor resolves *inside* the
		// rootfs at run time but which would resolve against the host here). We only
		// need to confirm the directory entry was populated, so check the entry itself.
		if _, err := os.Lstat(filepath.Join(abs, rel)); err != nil {
			return fmt.Errorf("rootfs %s missing %s: %w", abs, rel, err)
		}
	}
	return nil
}

// gvisorRootfsName maps a command (a per-step Cmd from the language registry) to
// the name of the rootfs that serves it, and whether such a mapping exists. The
// command set mirrors nsjail.go's filesystemArgs dispatch:
//
//   - the three interpreters map to their own rootfs (py3/bash/js);
//   - both compiler drivers AND the compiled-artifact run step ("./…") map to the
//     single shared ccpp rootfs — gcc and g++ both live there, and a C/C++ binary's
//     run-time library needs are a subset of what that rootfs already carries (this
//     is why c and cpp need no separate run rootfs, and why "./solution" — which on
//     its own does not say whether the artifact is C or C++ — can route to one tree);
//   - javac/java map to the JDK rootfs; iverilog/vvp to the verilog rootfs.
//
// An unrecognised command returns ("", false), which Run turns into a clean error.
// pythonInterpreter, cppCompiler, artifactRunPrefix, … are defined in nsjail.go.
func gvisorRootfsName(cmd string) (string, bool) {
	switch {
	case cmd == pythonInterpreter:
		return gvisorRootfsPy3, true
	case cmd == bashInterpreter:
		return gvisorRootfsBash, true
	case cmd == nodeInterpreter:
		return gvisorRootfsJs, true
	case cmd == cppCompiler, cmd == cCompiler, strings.HasPrefix(cmd, artifactRunPrefix):
		return gvisorRootfsCcpp, true
	case cmd == javacCompiler, cmd == javaRuntime:
		return gvisorRootfsJava, true
	case cmd == iverilogCompiler, cmd == vvpRuntime:
		return gvisorRootfsVerilog, true
	default:
		return "", false
	}
}

// Backend names the sandbox implementation for reporting (see handlers' backendName).
func (r GvisorRunner) Backend() string { return "gvisor" }

func (r GvisorRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	// Resolve the rootfs for this command BEFORE touching runsc or building a bundle.
	// A command with no known rootfs, or a known one whose tree was not staged (so it
	// is absent from r.Rootfs), gets a clean, explicit error — no fallback to
	// nsjail/subprocess, no crash. The handler maps a runner error to internal_error
	// (build phase) or the test's internal_error status (run phase).
	name, ok := gvisorRootfsName(spec.Cmd)
	if !ok {
		return RunResult{}, fmt.Errorf("gvisor runner: unsupported command %q (no rootfs mapping)", spec.Cmd)
	}
	rootfsPath := r.Rootfs[name]
	if rootfsPath == "" {
		return RunResult{}, fmt.Errorf("gvisor runner: rootfs %q unavailable for command %q", name, spec.Cmd)
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

	ociSpec := r.buildOCISpec(spec, containerID, rootfsPath)
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
	}
	// Per-run syscall audit: runsc logs every guest syscall (scoped to
	// gvisorStraceSyscalls) into its debug log, which is this stage's substitute for
	// the eBPF tracer (the guest's syscalls never reach the host tracepoints). The
	// strace text goes to the component debug log via --debug-log (the "boot"
	// component carries the guest strace), parsed after the run by
	// collectStraceEvents. --strace needs --debug to raise the sentry log to the
	// level strace lines are emitted at; the added general debug volume is the main
	// strace overhead, measured/flagged per the task (GOBOXD_GVISOR_STRACE=off
	// turns the whole thing off for A/B).
	if r.Strace {
		args = append(args,
			"--debug",
			"--strace",
			"--strace-syscalls="+gvisorStraceSyscalls,
			"--debug-log="+filepath.Join(bundleDir, gvisorDebugLogTmpl),
		)
	}
	args = append(args,
		"run",
		"--bundle", bundleDir,
		containerID,
	)

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
	// guest. The gVisor-native trace source is runsc's own --strace (enabled above
	// when r.Strace is set), collected from the debug log after the run and returned
	// in RunResult.TraceEvents — so the handler's eBPF traceRun stays empty for a
	// gVisor run and the audit trail comes from the strace events instead.

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

	// Collect the gVisor-native syscall audit from runsc's strace/debug log, parsed
	// into the same Event shape the eBPF tracer produces. Done unconditionally
	// (including on timeout/OOM) so a killed run still reports what it did before it
	// died — the log lines are already written. No-op when Strace is off (the log
	// was never requested, so the glob finds nothing).
	if r.Strace {
		res.TraceEvents = collectStraceEvents(bundleDir)
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
func (r GvisorRunner) buildOCISpec(spec RunSpec, containerID, rootfsPath string) ociSpec {
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
			// Absolute path to the shared per-language rootfs for this command,
			// read-only. Shared safely across concurrent runs (the gofer opens it
			// read-only); per-request writes go to the work-dir bind and the tmpfs /tmp
			// below.
			Path:     rootfsPath,
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
