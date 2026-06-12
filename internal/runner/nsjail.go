package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// nsjailGraceSec is added to the wall-time limit when sizing the Go context
// deadline. nsjail enforces the real limit internally via --time_limit; the
// outer context only fires as a backstop if nsjail itself hangs.
const nsjailGraceSec = 5

// These are the command paths wired to the minimal mount-namespace profile. The
// interpreted languages (py3, bash, js) each run in a fresh mount namespace with
// only their binary, shared libraries, and any language-specific extra paths
// bind-mounted read-only. cpp and c each add a compiler build-step profile (g++
// and gcc respectively); their compiled-artifact run steps share a single profile
// (see filesystemArgs), because a C binary's library needs are a subset of a
// C++ binary's. java adds two more profiles (javac build, java run), both of which
// mount the whole JDK runtime image — see resolveJavaMounts. verilog adds the last
// two (iverilog build, vvp run), both of which mount the Icarus "ivl base
// directory" — see resolveVerilogMounts. With verilog migrated, no language is left
// on --disable_clone_newns; that branch now only catches genuinely unknown commands.
const (
	pythonInterpreter = "/usr/bin/python3"
	bashInterpreter   = "/usr/bin/bash"
	nodeInterpreter   = "/usr/bin/node"
	cppCompiler       = "/usr/bin/g++"
	cCompiler         = "/usr/bin/gcc"
	javacCompiler     = "/usr/bin/javac"
	javaRuntime       = "/usr/bin/java"
	iverilogCompiler  = "/usr/bin/iverilog"
	vvpRuntime        = "/usr/bin/vvp"
)

// artifactRunPrefix is how the language registry invokes a compiled artifact: as
// a path relative to the work directory, e.g. "./solution". Both cpp and c run
// their binaries this way, so a command starting with this prefix is the run step
// of a compiled language and gets the compiled-artifact mount profile.
const artifactRunPrefix = "./"

// defaultSeccompPolicyPath is where the kafel seccomp deny-list policy is shipped
// in the image (the Dockerfile copies configs/ into the working directory /app, so
// this relative path resolves at runtime). It is passed to nsjail unchanged via
// --seccomp_policy for every language.
const defaultSeccompPolicyPath = "configs/seccomp.policy"

// resolveSeccompPolicy returns the path to the seccomp policy file to hand nsjail,
// after verifying it exists. Resolving at startup means a missing or mis-deployed
// policy fails loudly when the runner is constructed (exactly like a failed mount
// resolution) instead of surfacing as a per-request nsjail error. It is a package
// var so tests can substitute a fake that does not touch disk.
var resolveSeccompPolicy = func(ctx context.Context) (string, error) {
	if _, err := os.Stat(defaultSeccompPolicyPath); err != nil {
		return "", fmt.Errorf("seccomp policy %s: %w", defaultSeccompPolicyPath, err)
	}
	return defaultSeccompPolicyPath, nil
}

// NsjailRunner runs the command inside an nsjail sandbox. It implements the
// same Runner interface as SubprocessRunner and returns the identical
// RunResult shape, so callers can swap between the two without changes.
//
// Filesystem isolation is being layered on one batch of languages at a time. The
// interpreted languages py3, bash and js each run in their own mount namespace
// with a minimal read-only root (interpreter binary + shared libraries + any
// language-specific extra paths) and a writable per-request work directory, so the
// sandboxed code cannot see the container's filesystem. cpp and c each get a
// compiler build-step profile (the g++/gcc driver, the toolchain programs it
// shells out to, its header and library search paths), and they share a single
// compiled-artifact run profile for executing the resulting binary — see
// filesystemArgs for why the build and run profiles differ. java and verilog each
// add a build profile and a run profile of their own. With all seven languages
// migrated, no language uses --disable_clone_newns any more; every request runs in
// its own mount namespace with a minimal read-only root.
//
// Each profile's read-only mount list is the same for every request, so it is
// resolved once at startup (via NewNsjailRunner) and cached rather than shelling
// out to ldd / the compiler on every Run.
type NsjailRunner struct {
	// NsjailPath is the path to the nsjail binary. Defaults to "nsjail"
	// (resolved via PATH) when empty.
	NsjailPath string

	// py3Mounts, bashMounts and jsMounts each hold the resolved read-only
	// --bindmount_ro flag pairs for that interpreter (its binary, shared
	// libraries, and any language-specific extra paths). They are populated once
	// by NewNsjailRunner and reused by every request; empty slices mean
	// construction went through the zero value rather than NewNsjailRunner (used
	// only by the compile-time assertions).
	py3Mounts  []string
	bashMounts []string
	jsMounts   []string

	// cppBuildMounts holds the read-only mounts for the g++ build step (the
	// compiler driver, the toolchain programs it shells out to — cc1plus, as, ld
	// — its header search paths and link-time library directories). cBuildMounts
	// holds the equivalent for the gcc build step (cc1 instead of cc1plus, and no
	// C++ header dirs). cppRunMounts holds the minimal read-only shared-library
	// mounts a compiled binary needs to execute; it is shared by both the cpp and
	// c run steps because a C binary's library needs are a subset of a C++ one's.
	// All three are resolved once by NewNsjailRunner.
	cppBuildMounts []string
	cBuildMounts   []string
	cppRunMounts   []string

	// javaBuildMounts holds the read-only mounts for the javac build step and
	// javaRunMounts those for the java run step. Both mount the whole JDK runtime
	// image (its bin/, lib/ and lib/modules under JAVA_HOME) read-only and recreate
	// the /usr/bin/javac or /usr/bin/java symlink inside the sandbox — see
	// resolveJavaMounts for why a symlink rather than a bind mount. Unlike cpp/c,
	// java's run step is not a per-request compiled binary; it is the same java
	// launcher every request, so its mounts are static and cached at startup too.
	javaBuildMounts []string
	javaRunMounts   []string

	// verilogBuildMounts holds the read-only mounts for the iverilog build step and
	// verilogRunMounts those for the vvp run step. Both mount the Icarus "ivl base
	// directory" (which holds the programs iverilog shells out to — ivl, ivlpp — and
	// the VPI modules / config vvp loads) read-only, plus the launcher binary and the
	// shared libraries the launcher and those helpers need — see resolveVerilogMounts.
	// Like java, verilog's run step is the same vvp launcher every request (the
	// per-request content is the .vvp artifact in the work dir), so both are static
	// and cached at startup.
	verilogBuildMounts []string
	verilogRunMounts   []string

	// SeccompPolicyPath is the path to the kafel seccomp deny-list policy
	// (configs/seccomp.policy) that is passed to nsjail via --seccomp_policy. The
	// same policy applies to every language uniformly — it filters dangerous
	// syscalls (ptrace, bpf, mount, kexec_load, …) in the sandboxed child and is
	// orthogonal to the per-language filesystem mounts. It is resolved once by
	// NewNsjailRunner (which verifies the file exists, so a missing policy fails at
	// startup rather than per request); an empty value means no policy is passed
	// (only the zero-value runner used by the compile-time assertions).
	SeccompPolicyPath string
}

// NewNsjailRunner constructs an NsjailRunner with its per-language filesystem
// mount profiles resolved up front: the read-only mount lists for the three
// interpreted languages (py3, bash, js), the two compiler build profiles (g++ for
// cpp, gcc for c) and the shared compiled-artifact run profile. Resolving them
// here, once, avoids running ldd / the compiler per request and surfaces a broken
// sandbox (e.g. python3, node, g++ or gcc missing) at startup instead of on the
// first request. Any resolution error is meant to be fatal to startup, exactly
// like a failed language-registry load.
func NewNsjailRunner(ctx context.Context, nsjailPath string) (NsjailRunner, error) {
	py3, err := resolvePy3Mounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve py3 mounts: %w", err)
	}
	bash, err := resolveBashMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve bash mounts: %w", err)
	}
	js, err := resolveJsMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve js mounts: %w", err)
	}
	cppBuild, err := resolveCppBuildMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve cpp build mounts: %w", err)
	}
	cBuild, err := resolveCBuildMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve c build mounts: %w", err)
	}
	cppRun, err := resolveCppRunMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve cpp run mounts: %w", err)
	}
	javaBuild, err := resolveJavaBuildMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve java build mounts: %w", err)
	}
	javaRun, err := resolveJavaRunMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve java run mounts: %w", err)
	}
	verilogBuild, err := resolveVerilogBuildMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve verilog build mounts: %w", err)
	}
	verilogRun, err := resolveVerilogRunMounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve verilog run mounts: %w", err)
	}
	seccompPolicy, err := resolveSeccompPolicy(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve seccomp policy: %w", err)
	}
	return NsjailRunner{
		NsjailPath:         nsjailPath,
		py3Mounts:          py3,
		bashMounts:         bash,
		jsMounts:           js,
		cppBuildMounts:     cppBuild,
		cBuildMounts:       cBuild,
		cppRunMounts:       cppRun,
		javaBuildMounts:    javaBuild,
		javaRunMounts:      javaRun,
		verilogBuildMounts: verilogBuild,
		verilogRunMounts:   verilogRun,
		SeccompPolicyPath:  seccompPolicy,
	}, nil
}

func (r NsjailRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	wallSec := spec.WallTimeSec
	if wallSec <= 0 {
		wallSec = 10
	}

	// Outer context backstop: a few seconds beyond nsjail's own --time_limit so
	// nsjail kills the child first and we observe that as a timeout. The context
	// only fires if nsjail itself fails to terminate.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(wallSec+nsjailGraceSec)*time.Second)
	defer cancel()

	nsjailPath := r.NsjailPath
	if nsjailPath == "" {
		nsjailPath = "nsjail"
	}

	nsjailArgs := r.buildNsjailArgs(spec, wallSec)

	cmd := exec.CommandContext(runCtx, nsjailPath, nsjailArgs...)
	cmd.Stdin = strings.NewReader(spec.Stdin)

	outW := &cappedWriter{limit: outputCap}
	errW := &cappedWriter{limit: outputCap}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}
	waitErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()

	var exitCode int
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	// Timeout detection. nsjail enforces --time_limit and SIGKILLs the child at
	// the boundary, so a process that runs up to (or past) the wall limit is a
	// timeout. The outer-context deadline is the backstop for a hung nsjail.
	timedOut := runCtx.Err() == context.DeadlineExceeded
	if !timedOut && durationMs >= int64(wallSec)*1000 {
		timedOut = true
	}

	res := RunResult{
		Stdout:     string(outW.Bytes()),
		Stderr:     string(errW.Bytes()),
		DurationMs: durationMs,
		// MemoryPeakKB is not yet reported under nsjail: ProcessState.SysUsage
		// reflects the nsjail wrapper, not the sandboxed child. Left at 0 until
		// cgroup-based accounting is added in a later step.
		MemoryPeakKB: 0,
		ExitCode:     exitCode,
		TimedOut:     timedOut,
	}

	if timedOut {
		return res, nil
	}
	// A non-zero child exit arrives as *exec.ExitError; that is a normal result
	// (runtime error), not a runner failure. Only surface other errors.
	if waitErr != nil {
		if _, ok := waitErr.(*exec.ExitError); !ok {
			return res, waitErr
		}
	}
	return res, nil
}

// buildNsjailArgs assembles the full nsjail argument vector for one request: the
// base sandbox flags, the uniform seccomp policy, the per-language filesystem
// profile, the working directory, and finally the command and its arguments. It is
// split out from Run (which only execs the result) so the argument construction —
// in particular that the seccomp policy is applied to every language — is unit
// testable without actually invoking nsjail.
func (r NsjailRunner) buildNsjailArgs(spec RunSpec, wallSec int) []string {
	args := []string{
		"--mode", "o", // one-shot: run the command once and exit
		"--time_limit", strconv.Itoa(wallSec),
		"--really_quiet", // suppress nsjail's own logging so only child stdio remains
		// Give the child a PATH. nsjail starts the child with an empty environment
		// by default; compiler drivers then fail because they shell out to helper
		// tools by name — e.g. g++/gcc invoke "ld" and "as" via PATH ("collect2:
		// fatal error: cannot find 'ld'"). SubprocessRunner inherits the server's
		// environment, so this restores parity for every language's toolchain.
		"--env", "PATH=/usr/local/bin:/usr/bin:/bin",
		// Do NOT cap the virtual address space. --rlimit_as limits *virtual* memory,
		// not resident memory, and managed runtimes reserve enormous virtual regions
		// up front regardless of actual use: Node's V8 ("Failed to reserve virtual
		// memory for CodeRange") and the JVM ("Could not reserve enough space for
		// object heap" / pthread_create EAGAIN on thread-stack mmap) both abort under
		// any practical --rlimit_as. nsjail's own default rlimit_as (4 GiB) is also
		// too small for them, so we explicitly lift it. SubprocessRunner enforces no
		// memory limit either, so this matches its behavior; real (resident) memory
		// capping belongs in a cgroup limit, which is a separate hardening step.
		"--rlimit_as", "max",
	}

	// Seccomp syscall filter, applied UNIFORMLY to every language. The kafel
	// deny-list policy (configs/seccomp.policy) KILLs a fixed set of dangerous
	// syscalls (ptrace, bpf, mount, kexec_load, …) in the sandboxed child and is
	// independent of the per-language filesystem mounts, so it is added here in the
	// shared base rather than in filesystemArgs. Empty only for the zero-value
	// runner (the compile-time assertions); a real NsjailRunner always has it set.
	if r.SeccompPolicyPath != "" {
		args = append(args, "--seccomp_policy", r.SeccompPolicyPath)
	}

	// Filesystem isolation: each language gets a fresh mount namespace with a
	// minimal read-only root (or the --disable_clone_newns fallback for an unknown
	// command).
	args = append(args, r.filesystemArgs(spec)...)

	// Run inside the per-request working directory so relative artifacts (e.g.
	// ./solution) and the source file resolve exactly as under SubprocessRunner.
	if spec.WorkDir != "" {
		args = append(args, "--cwd", spec.WorkDir)
	}
	// Everything after "--" is the command and its arguments.
	args = append(args, "--", spec.Cmd)
	args = append(args, spec.Args...)
	return args
}

// filesystemArgs returns the nsjail flags that set up the child's filesystem
// view. For every isolated profile it builds a minimal mount namespace: a fresh
// tmpfs root (nsjail's default once the mount namespace is enabled) populated only
// with that profile's read-only mounts plus the per-request work directory mounted
// writable. The host root filesystem is never visible, so paths like /etc/passwd
// simply do not exist inside the sandbox.
//
// There are seven profiles:
//   - py3, bash, js: the interpreter binary, its shared libraries, and any
//     language-specific read-only paths.
//   - the cpp / c build step (Cmd == g++ / gcc): the compiler toolchain — driver,
//     cc1plus or cc1, as, ld, header search paths and link-time library
//     directories. These profiles are deliberately broader than the others: the
//     driver shells out to several programs and reads many headers and crt/library
//     files. Each also gets a writable tmpfs /tmp because the driver writes
//     intermediate .s/.o files there. The threat each contains is the untrusted
//     *source*; the compiler itself is trusted, so exposing system headers and
//     libraries read-only is an acceptable trade-off.
//   - the compiled-artifact run step (Cmd starts with "./"): the minimal set of
//     shared libraries a g++/gcc-compiled binary needs to execute, and nothing
//     else. cpp and c share this profile — a C binary's library needs are a subset
//     of a C++ one's. This is the security-critical profile — it runs the
//     untrusted *binary* — so it is kept as tight as possible: individual .so
//     files, no directories.
//   - the java build step (Cmd == javac) and java run step (Cmd == java): both
//     mount the whole JDK runtime image (JAVA_HOME's bin/, lib/ and lib/modules)
//     read-only and recreate the launcher symlink. Unlike cpp/c, java's run step
//     is not a per-request binary but the same java launcher every time, so it too
//     is static and cached. javac gets a writable tmpfs /tmp for the JVM's perf
//     scratch; the java run step does not (the JVM degrades gracefully without it).
//     See resolveJavaMounts for why both need the full runtime image and a symlink.
//   - the verilog build step (Cmd == iverilog) and verilog run step (Cmd == vvp):
//     both mount the Icarus "ivl base directory" read-only — it holds the programs
//     iverilog shells out to (ivl, ivlpp) and the VPI modules / config vvp loads —
//     plus the launcher binary and the shared libraries the launcher and those
//     helpers need. Like java, vvp's run step is the same launcher every request
//     (the per-request content is the .vvp artifact in the work dir), so both are
//     static and cached. iverilog gets a writable tmpfs /tmp for its driver's
//     intermediate command file; the vvp run step does not. See resolveVerilogMounts.
//
// Each profile's read-only mounts are identical for every request, so they are
// taken from the cache that NewNsjailRunner resolved once at startup. Only the
// writable work directory varies per request and is appended here.
//
// With all seven languages migrated, the --disable_clone_newns fallback in the
// default branch is no longer reached by any known language; it remains only as a
// safe default for a genuinely unknown command (one not in any profile), which then
// shares the host filesystem exactly as before rather than failing outright.
func (r NsjailRunner) filesystemArgs(spec RunSpec) []string {
	switch {
	case spec.Cmd == pythonInterpreter:
		return r.isolatedArgs(r.py3Mounts, spec, false)
	case spec.Cmd == bashInterpreter:
		return r.isolatedArgs(r.bashMounts, spec, false)
	case spec.Cmd == nodeInterpreter:
		return r.isolatedArgs(r.jsMounts, spec, false)
	case spec.Cmd == cppCompiler:
		// Build step: needs a writable /tmp for g++'s intermediate files.
		return r.isolatedArgs(r.cppBuildMounts, spec, true)
	case spec.Cmd == cCompiler:
		// Build step: needs a writable /tmp for gcc's intermediate files.
		return r.isolatedArgs(r.cBuildMounts, spec, true)
	case spec.Cmd == javacCompiler:
		// Build step: javac runs on the JVM, which writes its perf-data scratch
		// (/tmp/hsperfdata_*) under /tmp, so give it a writable tmpfs /tmp.
		return r.isolatedArgs(r.javaBuildMounts, spec, true)
	case spec.Cmd == javaRuntime:
		// Run step: the same java launcher every request. The JVM also wants
		// /tmp/hsperfdata_* but degrades gracefully without it, so the
		// security-critical run step gets no /tmp.
		return r.isolatedArgs(r.javaRunMounts, spec, false)
	case spec.Cmd == iverilogCompiler:
		// Build step: iverilog's driver writes its intermediate command file (and
		// ivl/ivlpp their scratch) under /tmp, so give it a writable tmpfs /tmp.
		return r.isolatedArgs(r.verilogBuildMounts, spec, true)
	case spec.Cmd == vvpRuntime:
		// Run step: the same vvp launcher every request (the per-request content is
		// the .vvp artifact in the work dir). The security-critical run step gets no
		// /tmp — vvp does not need scratch space for our programs.
		return r.isolatedArgs(r.verilogRunMounts, spec, false)
	case strings.HasPrefix(spec.Cmd, artifactRunPrefix):
		// Run step: executing the compiled binary in the work directory.
		return r.isolatedArgs(r.cppRunMounts, spec, false)
	default:
		return []string{"--disable_clone_newns"}
	}
}

// isolatedArgs assembles the per-request filesystem flags for an isolated
// profile: a copy of the cached read-only mounts, optionally a writable tmpfs
// /tmp, and the per-request work directory mounted writable. The cached slice is
// copied first so the per-request appends never mutate the shared profile.
func (r NsjailRunner) isolatedArgs(cached []string, spec RunSpec, tmpfsTmp bool) []string {
	args := append([]string(nil), cached...)

	// A writable tmpfs at /tmp. Only the build step needs it (g++ writes
	// intermediate .s/.o files under /tmp); the interpreted and run profiles pass
	// false so their sandbox has no /tmp at all.
	if tmpfsTmp {
		args = append(args, "--tmpfsmount", "/tmp")
	}

	// The per-request work directory, mounted writable. This is where the source
	// file was written and where the program (or compiler) may create artifacts —
	// for the run step it is also where the binary being executed lives. It is the
	// only writable, persistent path the sandbox can reach.
	if spec.WorkDir != "" {
		args = append(args, "--bindmount", spec.WorkDir)
	}

	return args
}

// resolveInterpreterMounts builds the static read-only --bindmount_ro flag pairs
// for an interpreter invoked at binPath: the binary itself, the shared libraries
// it is dynamically linked against, and any language-specific extra paths
// (e.g. a stdlib directory). These never change between requests, so the caller
// resolves them once at startup and caches the result.
func resolveInterpreterMounts(ctx context.Context, binPath string, extraPaths []string) ([]string, error) {
	// Read-only mount of the interpreter binary. The invocation path is often a
	// symlink (e.g. /usr/bin/python3 → python3.11, /usr/bin/node → nodejs); resolve
	// it and bind the real file onto the path the command invokes, so
	// execve(binPath) lands on the real binary regardless of how nsjail treats a
	// symlink source.
	realBin, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", binPath, err)
	}
	args := []string{"--bindmount_ro", realBin + ":" + binPath}

	// Shared libraries the interpreter is dynamically linked against (including
	// the dynamic loader itself). Without these, execve fails before any user code
	// runs.
	libs, err := sharedLibraries(ctx, binPath)
	if err != nil {
		return nil, err
	}
	for _, lib := range libs {
		args = append(args, "--bindmount_ro", lib)
	}

	// Language-specific extra read-only paths (e.g. an interpreter's stdlib
	// directory). Interpreters whose standard library is compiled into the binary
	// (like node) pass none.
	for _, p := range extraPaths {
		args = append(args, "--bindmount_ro", p)
	}

	return args, nil
}

// resolvePy3Mounts, resolveBashMounts and resolveJsMounts build the cached
// read-only mount lists for the three interpreted languages. They are package
// vars (not plain funcs) so tests can substitute fakes and assert each runs
// exactly once, at construction.

// resolvePy3Mounts resolves py3's mounts: the interpreter, its shared libraries,
// and the Python standard library directories. The interpreter imports os.py and
// the lib-dynload C extensions from the stdlib at startup; without them Python
// aborts with "Could not find platform independent libraries <prefix>". A missing
// stdlib means the sandbox can never run Python, so fail loudly at startup.
var resolvePy3Mounts = func(ctx context.Context) ([]string, error) {
	dirs := pythonStdlibDirs()
	if len(dirs) == 0 {
		return nil, fmt.Errorf("python stdlib directory not found under /usr/lib")
	}
	return resolveInterpreterMounts(ctx, pythonInterpreter, dirs)
}

// resolveBashMounts resolves bash's mounts. bash needs only its binary and the
// shared libraries it links against — no stdlib directory — so it passes no extra
// paths. (External commands like `cat` are deliberately not mounted: the sandbox
// is meant to be minimal, so a script that shells out to a tool that isn't bound
// simply cannot find it.)
var resolveBashMounts = func(ctx context.Context) ([]string, error) {
	return resolveInterpreterMounts(ctx, bashInterpreter, nil)
}

// nodeBuiltinsDir is where Debian's nodejs build loads its "externalized
// builtins" (acorn, cjs-module-lexer, undici, …) from at startup. Unlike a
// vanilla upstream node build, these JS sources are not embedded in the binary's
// snapshot; node aborts immediately ("Cannot load externalized builtin") if the
// directory is missing, even for a plain console.log. It is therefore a required
// read-only mount for js, exactly analogous to the Python stdlib for py3.
const nodeBuiltinsDir = "/usr/share/nodejs"

// resolveJsMounts resolves node's mounts: the binary, its shared libraries (libnode,
// libicu, libssl, … all picked up by ldd), and the externalized-builtins directory.
// Empirically a hello world that only calls console.log still needs nodeBuiltinsDir
// — without it node aborts before running any user code — so a missing directory
// means js can never run and we fail loudly at startup.
var resolveJsMounts = func(ctx context.Context) ([]string, error) {
	if fi, err := os.Stat(nodeBuiltinsDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("node builtins directory %s not found", nodeBuiltinsDir)
	}
	return resolveInterpreterMounts(ctx, nodeInterpreter, []string{nodeBuiltinsDir})
}

// --- cpp / c: build-step and run-step mount resolution -----------------------

// resolveCppBuildMounts and resolveCBuildMounts build the read-only mount lists
// for the g++ and gcc build steps respectively. Both are thin wrappers over
// resolveCompilerBuildMounts — the gcc and g++ drivers are the same family and
// differ only in which language they preprocess (g++ pulls in the C++ header
// directories, gcc does not) — and both are package vars so tests can substitute a
// fake (the real ones shell out to the compiler).
var resolveCppBuildMounts = func(ctx context.Context) ([]string, error) {
	return resolveCompilerBuildMounts(ctx, cppCompiler, "c++")
}

var resolveCBuildMounts = func(ctx context.Context) ([]string, error) {
	return resolveCompilerBuildMounts(ctx, cCompiler, "c")
}

// resolveCompilerBuildMounts builds the read-only mount list for a gcc-family
// build step. The driver (gcc/g++) shells out to other programs (cc1/cc1plus, as,
// ld) and reads a large set of files (C/C++ headers, crt object files, link-time
// libraries), so this profile is necessarily broader than an interpreter's: it
// mounts whole directories (the header search paths, the gcc install directory and
// the link-time library directories) read-only, plus the driver/assembler/linker
// binaries and the dynamic loader. Everything here is discovered by asking the
// compiler itself (parameterized by lang, "c" or "c++", since the include search
// list differs), so a compiler version bump does not silently break the mount.
//
// This is the least security-sensitive of the compiled profiles: the untrusted
// input is the *source*, and the compiler is trusted, so exposing system headers
// and libraries read-only is acceptable. The compiled binary's own run step (see
// resolveCppRunMounts) is locked down far more tightly.
func resolveCompilerBuildMounts(ctx context.Context, compiler, lang string) ([]string, error) {
	m := newMountSet()

	// Header search paths (e.g. /usr/include/c++/12, /usr/include) and the
	// link-time library directories (e.g. /usr/lib/x86_64-linux-gnu, the gcc
	// install dir) — mounted as whole read-only directories.
	includeDirs, err := compilerIncludeDirs(ctx, compiler, lang)
	if err != nil {
		return nil, err
	}
	libDirs, installDir, err := compilerSearchDirs(ctx, compiler)
	if err != nil {
		return nil, err
	}
	// Feed every directory the compiler named to the mountSet; it reduces them to
	// the minimal ancestor set (e.g. the gcc install dir that holds cc1/cc1plus,
	// the multiarch lib dir, /usr/include) and drops redundant descendants. Order
	// does not matter — ancestor-reduction is order-independent.
	m.addDirRO(installDir)
	for _, d := range libDirs {
		m.addDirRO(d)
	}
	for _, d := range includeDirs {
		m.addDirRO(d)
	}

	// The driver and the programs it invokes. cc1/cc1plus lives inside the gcc
	// install dir (already mounted as a directory); as and ld are separate binaries
	// that must be bound individually, along with the driver itself.
	m.addFileRO(compiler)
	for _, prog := range []string{"as", "ld"} {
		p, err := compilerProgPath(ctx, compiler, prog)
		if err != nil {
			return nil, err
		}
		m.addFileRO(p)
	}

	// The dynamic loader, needed to exec the driver/assembler/linker. It lives at a
	// fixed path (e.g. /lib64/ld-linux-x86-64.so.2) baked into every ELF as its
	// interpreter, so it must exist at that exact path inside the sandbox.
	loader, err := dynamicLoader(ctx, compiler)
	if err != nil {
		return nil, err
	}
	if loader != "" {
		m.addFileRO(loader)
	}

	return m.args(), nil
}

// cppProbeSource is a minimal C++ translation unit compiled once at startup to
// discover the shared libraries a g++-built binary depends on. It pulls in
// <iostream> so the probe links libstdc++ (and transitively libm, libgcc_s,
// libc) — the same libraries any of our compiled programs will need.
const cppProbeSource = "#include <iostream>\nint main(){std::cout<<\"\";return 0;}\n"

// resolveCppRunMounts builds the minimal read-only mount list for executing a
// g++-compiled binary. The binary itself is per-request content, but its *library
// dependencies* are static for our flag allow-list (which permits only -O*, -Wall,
// -Wextra and -std=*, none of which change linkage — a request cannot add -l
// flags), so the set is resolved once at startup rather than per request: a probe
// program is compiled and ldd'd, and those .so paths become the cached mounts.
//
// This profile is intentionally tight — individual shared objects, no directories
// — because this is the step that runs untrusted compiled code. The binary being
// executed is reached through the per-request writable work directory, so it is
// not mounted here. The c run step shares this same profile (its "./solution" Cmd
// dispatches here too): a C binary links a subset of what the C++ probe pulls in,
// so the cached C++ run set is a safe superset.
//
// A package var so tests can substitute a fake (the real one shells out to g++/ldd).
var resolveCppRunMounts = func(ctx context.Context) ([]string, error) {
	dir, err := os.MkdirTemp("", "goboxd-cppprobe-*")
	if err != nil {
		return nil, fmt.Errorf("cpp run mounts: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "probe.cpp")
	bin := filepath.Join(dir, "probe")
	if err := os.WriteFile(src, []byte(cppProbeSource), 0600); err != nil {
		return nil, fmt.Errorf("cpp run mounts: write probe: %w", err)
	}
	if out, err := exec.CommandContext(ctx, cppCompiler, "-o", bin, src).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("cpp run mounts: compile probe: %w: %s", err, strings.TrimSpace(string(out)))
	}

	libs, err := sharedLibraries(ctx, bin)
	if err != nil {
		return nil, fmt.Errorf("cpp run mounts: %w", err)
	}
	m := newMountSet()
	for _, lib := range libs {
		m.addFileRO(lib)
	}
	return m.args(), nil
}

// --- java: build-step and run-step mount resolution --------------------------

// resolveJavaBuildMounts and resolveJavaRunMounts build the read-only mount lists
// for the javac build step and the java run step. Both are thin wrappers over
// resolveJavaMounts (the two launchers live in the same JDK and need the same
// runtime image; they differ only in which launcher symlink is recreated), and
// both are package vars so tests can substitute a fake (the real ones inspect the
// JDK on the host).
var resolveJavaBuildMounts = func(ctx context.Context) ([]string, error) {
	return resolveJavaMounts(ctx, javacCompiler)
}

var resolveJavaRunMounts = func(ctx context.Context) ([]string, error) {
	return resolveJavaMounts(ctx, javaRuntime)
}

// resolveJavaMounts builds the read-only mount list for a JDK launcher (javac or
// java). Unlike an interpreter, a JDK launcher cannot run from just its binary
// plus shared libraries: it loads the runtime image (lib/modules), libjvm.so and
// the rest of the JDK's own libraries from JAVA_HOME at startup. So this profile
// mounts the entire JAVA_HOME directory tree read-only at its literal path — bin/,
// lib/, lib/modules, conf/ and everything else — which is broad but, like the
// compiler build profile, acceptable: the JDK is trusted, only the source/.class
// is untrusted, and the tree is read-only with no network in the jail.
//
// The crucial subtlety is the launcher symlink. The launcher derives JAVA_HOME by
// reading /proc/self/exe and stripping the trailing bin/<launcher>. If we bind-
// mounted the real launcher onto /usr/bin/java, /proc/self/exe would report
// /usr/bin/java and the JVM would mis-derive JAVA_HOME as /usr, then fail to find
// /usr/lib/modules ("Error occurred during initialization of VM"). Instead we
// recreate the original symlink (/usr/bin/java -> JAVA_HOME/bin/java) inside the
// sandbox with nsjail's --symlink, so execve resolves through it to the real path
// under the mounted JAVA_HOME and the derivation lands on the right home.
func resolveJavaMounts(ctx context.Context, launcher string) ([]string, error) {
	// The launcher path (/usr/bin/javac, /usr/bin/java) is a chain of symlinks
	// (Debian's update-alternatives) ending at JAVA_HOME/bin/<launcher>. Resolve it
	// so we can both mount JAVA_HOME and point the recreated symlink at the real
	// binary.
	realLauncher, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", launcher, err)
	}

	// JAVA_HOME is two levels up from the launcher (.../bin/<launcher>). Mount the
	// whole tree read-only; it carries lib/modules, lib/server/libjvm.so and the
	// rest of the runtime image the launcher loads at startup.
	javaHome := filepath.Dir(filepath.Dir(realLauncher))
	m := newMountSet()
	m.addDirRO(javaHome)
	if len(m.dirs) == 0 {
		return nil, fmt.Errorf("java mounts: JAVA_HOME %q is not a directory", javaHome)
	}

	// Debian's OpenJDK splits the editable config (java.security, logging.properties,
	// …) out into /etc/java-<ver>-openjdk and symlinks JAVA_HOME/conf/** at it. Those
	// symlinks are inside the mounted JAVA_HOME but their targets are not, so without
	// this the JVM aborts at startup ("Error loading java.security file"). Mount the
	// /etc config tree read-only too. Globbed (not hardcoded) so a version bump still
	// resolves; absent on a non-Debian JDK, which simply needs no extra mount.
	for _, d := range javaEtcConfigDirs() {
		m.addDirRO(d)
	}

	// The system shared libraries the launcher links against (libc, libz, …) and
	// the dynamic loader. The JDK's own libraries (libjli, …) live under JAVA_HOME
	// and are already covered by the directory mount, so addFileRO drops them.
	libs, err := sharedLibraries(ctx, realLauncher)
	if err != nil {
		return nil, fmt.Errorf("java mounts: %w", err)
	}
	// libjvm.so is dlopen'd by the launcher at startup, so ldd of the launcher does
	// not reveal libjvm.so's own dependencies — notably libstdc++, libgcc_s and
	// libm, which live in the system lib dir, not under JAVA_HOME. ldd libjvm.so
	// too and fold in everything it needs, or the JVM aborts before any user code
	// ("dl failure ... libstdc++.so.6: cannot open shared object file").
	for _, jvm := range jvmLibraries(javaHome) {
		jvmLibs, err := sharedLibraries(ctx, jvm)
		if err != nil {
			return nil, fmt.Errorf("java mounts: %w", err)
		}
		libs = append(libs, jvmLibs...)
	}
	for _, lib := range libs {
		m.addFileRO(lib)
	}

	args := m.args()
	// Recreate the launcher symlink inside the sandbox (see the doc comment for why
	// a symlink and not a bind mount). Format is target:linkpath.
	args = append(args, "--symlink", realLauncher+":"+launcher)
	return args, nil
}

// jvmLibraries returns the libjvm.so files under JAVA_HOME (normally just the
// server VM's lib/server/libjvm.so) so the caller can ldd them for the system
// libraries they pull in but the launcher's own ldd does not (libjvm.so is
// dlopen'd at runtime). It globs rather than hardcoding lib/server so an image
// shipping the VM elsewhere (e.g. lib/client) still resolves.
func jvmLibraries(javaHome string) []string {
	matches, _ := filepath.Glob(filepath.Join(javaHome, "lib", "*", "libjvm.so"))
	return matches
}

// javaEtcConfigDirs returns the Debian OpenJDK /etc config directories
// (e.g. /etc/java-17-openjdk) that JAVA_HOME/conf symlinks point at. Globbed so a
// minor/major version bump of the base image's JDK does not silently break the
// mount; returns nothing on a JDK that keeps its config inside JAVA_HOME.
func javaEtcConfigDirs() []string {
	matches, _ := filepath.Glob("/etc/java-*-openjdk*")
	var dirs []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			dirs = append(dirs, m)
		}
	}
	return dirs
}

// --- verilog: build-step and run-step mount resolution -----------------------

// resolveVerilogBuildMounts and resolveVerilogRunMounts build the read-only mount
// lists for the iverilog build step and the vvp run step. Both are thin wrappers
// over resolveVerilogMounts (the two launchers ship in the same Icarus install and
// both need the same "ivl base directory"; they differ only in which launcher
// binary is bound and which helpers contribute shared libraries), and both are
// package vars so tests can substitute a fake (the real ones inspect the host).
var resolveVerilogBuildMounts = func(ctx context.Context) ([]string, error) {
	return resolveVerilogMounts(ctx, iverilogCompiler, true)
}

var resolveVerilogRunMounts = func(ctx context.Context) ([]string, error) {
	return resolveVerilogMounts(ctx, vvpRuntime, false)
}

// shellPath is /bin/sh, which the iverilog driver needs because it invokes its
// sub-programs (ivlpp, ivl) via system(), i.e. through the shell. It is mounted for
// the iverilog build step only; the vvp run step deliberately omits it so untrusted
// Verilog cannot reach a shell through Icarus's $system() system task.
const shellPath = "/bin/sh"

// resolveVerilogMounts builds the read-only mount list for an Icarus launcher
// (iverilog or vvp). Neither runs from just its binary plus shared libraries:
// iverilog is a driver that shells out (via system()) to ivlpp (the preprocessor)
// and ivl (the compiler), and ivl/vvp in turn dlopen the code-generator target
// modules (*.tgt) and VPI modules (system.vpi, …) — all of which live in the Icarus
// "ivl base directory" (e.g. /usr/lib/ivl or the multiarch
// /usr/lib/x86_64-linux-gnu/ivl). So this profile mounts that whole base directory
// read-only at its literal path, which is broad but, like the compiler build and
// JDK profiles, acceptable: the Icarus install is trusted, only the source/.vvp is
// untrusted, the tree is read-only, and there is no network in the jail.
//
// The launcher's own ldd does not reveal the libraries its modules pull in: ivl,
// the *.tgt code generators and the *.vpi modules are exec'd or dlopen'd, and
// system.vpi in particular drags in libbz2/libz/libstdc++ that neither launcher
// links directly. So we additionally ldd every module file in the base directory
// and fold its libraries into the mount set — mirroring how the java profile ldd's
// libjvm.so for the launcher's hidden dependencies. (A run that loads system.vpi
// needs libbz2/libz too, so the run step gets these even though it omits the shell.)
//
// needShell adds /bin/sh and its libraries, required only by the iverilog driver
// (system()); the vvp run step passes false so the sandbox executing untrusted
// compiled code has no shell to reach via $system().
//
// Unlike java, no launcher symlink is needed: Icarus's tools find their base
// directory from a compiled-in path (overridable with -B), not by deriving it from
// /proc/self/exe, so binding the real binary straight onto its invocation path is
// safe.
func resolveVerilogMounts(ctx context.Context, launcher string, needShell bool) ([]string, error) {
	realBin, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", launcher, err)
	}

	m := newMountSet()

	// The ivl base directory: holds ivl/ivlpp (iverilog shells out to them), the
	// *.tgt code generators (ivl dlopens them) and the *.vpi modules (ivl and vvp
	// load them). Mount it whole, read-only.
	baseDirs := iverilogBaseDirs()
	if len(baseDirs) == 0 {
		return nil, fmt.Errorf("verilog mounts: Icarus ivl base directory not found under /usr/lib")
	}
	for _, d := range baseDirs {
		m.addDirRO(d)
	}

	// The shared libraries the launcher links against (libc, libreadline, …) and the
	// dynamic loader, plus the libraries the base-dir modules need — ldd of the
	// launcher alone under-reports these (ivl/ivlpp are exec'd, *.tgt/*.vpi are
	// dlopen'd). addFileRO drops any that already live inside the mounted base dir.
	libs, err := sharedLibraries(ctx, realBin)
	if err != nil {
		return nil, fmt.Errorf("verilog mounts: %w", err)
	}
	// The shell, for the iverilog driver's system() calls. Bound onto /bin/sh (it is
	// a symlink to dash on Debian) and ldd'd so dash's own libraries come along.
	if needShell {
		realShell, err := filepath.EvalSymlinks(shellPath)
		if err != nil {
			return nil, fmt.Errorf("verilog mounts: resolve %s: %w", shellPath, err)
		}
		m.addFileRO(shellPath) // binds realShell onto /bin/sh (addFileRO resolves the symlink)
		shLibs, err := sharedLibraries(ctx, realShell)
		if err != nil {
			return nil, fmt.Errorf("verilog mounts: %w", err)
		}
		libs = append(libs, shLibs...)
	}
	// ldd every module file in the base dir and fold in its libraries. Best-effort
	// per file: a non-dynamic or non-ELF entry (ldd refuses it) is skipped rather
	// than failing the whole resolution, so an odd file in the dir cannot break
	// startup — the modules we rely on (system.vpi, vvp.tgt) are dynamic ELF and ldd
	// cleanly.
	for _, mod := range iverilogModuleFiles(baseDirs) {
		modLibs, err := sharedLibraries(ctx, mod)
		if err != nil {
			continue
		}
		libs = append(libs, modLibs...)
	}
	for _, lib := range libs {
		m.addFileRO(lib)
	}

	args := m.args()
	// Bind the real launcher onto its invocation path so execve(launcher) lands on
	// the real binary (it is normally not a symlink, but resolve-then-bind keeps the
	// behaviour identical to the interpreter profiles if a distro ever makes it one).
	args = append(args, "--bindmount_ro", realBin+":"+launcher)
	return args, nil
}

// iverilogBaseDirs returns the Icarus "ivl base directory" (or directories) — where
// ivl, ivlpp, the *.tgt code generators and the *.vpi modules / config live. The
// location is distro-dependent (Debian uses /usr/lib/ivl on some releases and the
// multiarch /usr/lib/x86_64-linux-gnu/ivl on others), so it is globbed and validated
// by content rather than hardcoded, so an image change does not silently break the
// mount. A directory qualifies if it contains the ivl compiler binary or any Icarus
// config/system-function-table file.
func iverilogBaseDirs() []string {
	var dirs []string
	seen := map[string]bool{}
	for _, glob := range []string{
		"/usr/lib/ivl*",
		"/usr/lib/*/ivl*",
		"/usr/local/lib/ivl*",
	} {
		matches, _ := filepath.Glob(glob)
		for _, p := range matches {
			if seen[p] || !isIverilogBaseDir(p) {
				continue
			}
			seen[p] = true
			dirs = append(dirs, p)
		}
	}
	return dirs
}

// isIverilogBaseDir reports whether dir looks like an Icarus base directory: an
// existing directory holding the ivl compiler or at least one .conf/.sft control
// file (the markers vvp and iverilog look for there).
func isIverilogBaseDir(dir string) bool {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "ivl")); err == nil && !fi.IsDir() {
		return true
	}
	for _, g := range []string{"*.conf", "*.sft"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, g)); len(matches) > 0 {
			return true
		}
	}
	return false
}

// iverilogModuleFiles returns the loadable module files inside the ivl base
// directories — the helper executables (ivl, ivlpp, vhdlpp), the *.tgt code
// generators and the *.vpi modules — so the caller can ldd each for the shared
// libraries it needs but the launcher's own ldd does not reveal (these are exec'd
// or dlopen'd, not linked). Plain config files (*.conf, *.sft) are not returned;
// the caller ldd's best-effort and skips anything ldd refuses anyway.
func iverilogModuleFiles(baseDirs []string) []string {
	var files []string
	seen := map[string]bool{}
	add := func(p string) {
		if seen[p] {
			return
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, d := range baseDirs {
		for _, name := range []string{"ivl", "ivlpp", "vhdlpp"} {
			add(filepath.Join(d, name))
		}
		for _, glob := range []string{"*.tgt", "*.vpi"} {
			matches, _ := filepath.Glob(filepath.Join(d, glob))
			for _, p := range matches {
				add(p)
			}
		}
	}
	return files
}

// mountSet accumulates deduplicated read-only bind mounts and emits them as
// --bindmount_ro flag pairs. It keeps the mount list minimal and conflict-free in
// two ways:
//
//   - Directories are reduced to their ancestors: if both a directory and one of
//     its subdirectories are added, only the ancestor is kept, because a single
//     bind mount of the parent already exposes every child. This is essential —
//     g++ reports e.g. both /usr/include and /usr/include/x86_64-linux-gnu, and
//     /usr/include holds files (features.h) that are NOT under the child, so the
//     parent must win, not the child.
//   - A file is dropped if it already lives inside a mounted directory, and
//     duplicate destinations are dropped.
//
// Each mount keeps its requested (literal) path as the destination and binds the
// symlink-resolved real path as the source. This matters: aliased directories
// like /lib and /usr/lib (a symlink on Debian) must BOTH appear in the sandbox,
// because the toolchain hardcodes some absolute paths — e.g. the libm.so linker
// script references /lib/x86_64-linux-gnu/libm.so.6 literally — so collapsing them
// to one real path would leave those references dangling. addFileRO must be called
// only after all addDirRO calls, since file coverage is tested against the final
// directory set.
type bindMount struct {
	src string // symlink-resolved source on the host
	dst string // literal path the mount appears at inside the sandbox
}

// arg renders the mount as the value of a --bindmount_ro flag: "dst" when source
// and destination coincide, otherwise "src:dst".
func (b bindMount) arg() string {
	if b.src == b.dst {
		return b.dst
	}
	return b.src + ":" + b.dst
}

type mountSet struct {
	dirs     []bindMount     // directory mounts, kept ancestor-minimal by dst path
	files    []bindMount     // individual file mounts
	fileDsts map[string]bool // file destinations already added, for dedup
}

func newMountSet() *mountSet { return &mountSet{fileDsts: map[string]bool{}} }

// addDirRO records an existing directory to be mounted read-only at its literal
// path, maintaining the invariant that m.dirs contains no directory whose dst is
// nested under another's. If the new directory is already covered by an ancestor
// it is dropped; if it is an ancestor of existing entries, those descendants are
// dropped in its favour.
func (m *mountSet) addDirRO(dst string) {
	real, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return
	}
	sep := string(filepath.Separator)
	kept := make([]bindMount, 0, len(m.dirs)+1)
	for _, d := range m.dirs {
		if dst == d.dst || strings.HasPrefix(dst, d.dst+sep) {
			return // already covered by an existing ancestor (or duplicate)
		}
		if strings.HasPrefix(d.dst, dst+sep) {
			continue // d is a descendant of the new dir; drop it
		}
		kept = append(kept, d)
	}
	m.dirs = append(kept, bindMount{src: real, dst: dst})
}

// addFileRO records a file to be mounted read-only at dst, binding the
// symlink-resolved source onto dst so the file appears at the exact path the
// toolchain (or the ELF loader) expects. It is skipped if dst is already mounted
// or already lives inside a mounted directory.
func (m *mountSet) addFileRO(dst string) {
	if m.fileDsts[dst] || m.coveredByDir(dst) {
		return
	}
	real, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return
	}
	m.fileDsts[dst] = true
	m.files = append(m.files, bindMount{src: real, dst: dst})
}

func (m *mountSet) coveredByDir(p string) bool {
	sep := string(filepath.Separator)
	for _, d := range m.dirs {
		if p == d.dst || strings.HasPrefix(p, d.dst+sep) {
			return true
		}
	}
	return false
}

// args assembles the final --bindmount_ro flag pairs: every kept directory
// followed by every kept file.
func (m *mountSet) args() []string {
	out := make([]string, 0, 2*(len(m.dirs)+len(m.files)))
	for _, d := range m.dirs {
		out = append(out, "--bindmount_ro", d.arg())
	}
	for _, f := range m.files {
		out = append(out, "--bindmount_ro", f.arg())
	}
	return out
}

// compilerIncludeDirs returns the directories a gcc-family driver searches for
// headers, parsed from the "#include <...> search starts here:" / "End of search
// list." block that `<compiler> -E -v` prints on stderr. The lang argument ("c" or
// "c++") selects the language passed via -x, since the C++ search list adds the
// libstdc++ header directories. Resolving these from the compiler rather than
// hardcoding /usr/include/c++/<ver> keeps the mount correct across versions.
func compilerIncludeDirs(ctx context.Context, compiler, lang string) ([]string, error) {
	cmd := exec.CommandContext(ctx, compiler, "-E", "-v", "-x"+lang, os.DevNull, "-o", os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s -E -v: %w: %s", compiler, err, strings.TrimSpace(string(out)))
	}
	dirs := parseIncludeDirs(string(out))
	if len(dirs) == 0 {
		return nil, fmt.Errorf("%s reported no include search dirs", compiler)
	}
	return dirs, nil
}

// parseIncludeDirs extracts the header search directories from the
// "#include <...> search starts here:" / "End of search list." block of
// `g++ -E -v` output. Split out as a pure function so it can be unit-tested
// without a compiler on the host.
func parseIncludeDirs(out string) []string {
	var dirs []string
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#include <...> search starts here:"):
			inBlock = true
		case strings.HasPrefix(trimmed, "End of search list."):
			inBlock = false
		case inBlock && strings.HasPrefix(line, " "):
			dirs = append(dirs, trimmed)
		}
	}
	return dirs
}

// compilerSearchDirs returns a gcc-family driver's link-time library directories
// and its install directory (where cc1/cc1plus, collect2 and the crt object files
// live), parsed from `<compiler> -print-search-dirs`. The library list contains
// many ".." -laden, duplicate entries; they are cleaned and left for mountSet to
// deduplicate.
func compilerSearchDirs(ctx context.Context, compiler string) (libDirs []string, installDir string, err error) {
	out, err := exec.CommandContext(ctx, compiler, "-print-search-dirs").Output()
	if err != nil {
		return nil, "", fmt.Errorf("%s -print-search-dirs: %w", compiler, err)
	}
	libDirs, installDir = parseSearchDirs(string(out))
	if installDir == "" {
		return nil, "", fmt.Errorf("%s -print-search-dirs: no install dir", compiler)
	}
	return libDirs, installDir, nil
}

// parseSearchDirs extracts the install directory and the link-time library
// directories from `g++ -print-search-dirs` output. The library list uses
// "..":-laden, duplicate, colon-separated entries; they are filepath.Clean'd and
// left for mountSet to deduplicate. Split out as a pure function for testing.
func parseSearchDirs(out string) (libDirs []string, installDir string) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "install:"); ok {
			// These are Linux filesystem paths, so clean them with path (always
			// forward-slash) rather than filepath, which would rewrite separators
			// when the tests run on a non-Unix host.
			installDir = path.Clean(strings.TrimSpace(rest))
		}
		if rest, ok := strings.CutPrefix(line, "libraries:"); ok {
			// Format: "libraries: =/path/a:/path/b:..." — strip the leading '='.
			rest = strings.TrimPrefix(strings.TrimSpace(rest), "=")
			for _, p := range strings.Split(rest, ":") {
				if p != "" {
					libDirs = append(libDirs, path.Clean(p))
				}
			}
		}
	}
	return libDirs, installDir
}

// compilerProgPath resolves the absolute path of a toolchain program (e.g. "as",
// "ld") as a gcc-family driver would invoke it. `<compiler> -print-prog-name=X`
// returns an absolute path when the driver knows one and the bare name otherwise;
// in the latter case we fall back to a PATH lookup so the binary can still be
// bind-mounted.
func compilerProgPath(ctx context.Context, compiler, name string) (string, error) {
	out, err := exec.CommandContext(ctx, compiler, "-print-prog-name="+name).Output()
	if err != nil {
		return "", fmt.Errorf("%s -print-prog-name=%s: %w", compiler, name, err)
	}
	p := strings.TrimSpace(string(out))
	if strings.Contains(p, "/") {
		return p, nil
	}
	resolved, err := exec.LookPath(p)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", name, err)
	}
	return resolved, nil
}

// dynamicLoader returns the path of the ELF interpreter (dynamic loader) that
// binary uses, i.e. the ldd line with no "=>" mapping. It is needed because the
// loader path is baked into every dynamically linked ELF and must exist at that
// exact path inside the sandbox for execve to succeed.
func dynamicLoader(ctx context.Context, binary string) (string, error) {
	libs, err := sharedLibraries(ctx, binary)
	if err != nil {
		return "", err
	}
	for _, lib := range libs {
		if base := filepath.Base(lib); strings.HasPrefix(base, "ld-") || strings.HasPrefix(base, "ld-linux") {
			return lib, nil
		}
	}
	return "", nil
}

// sharedLibraries returns the absolute paths of the shared objects that binary
// is dynamically linked against, as reported by ldd. The dynamic loader (a line
// with no "=>") is included; the virtual DSO (linux-vdso) is skipped because it
// is kernel-provided and has no file on disk.
func sharedLibraries(ctx context.Context, binary string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "ldd", binary).Output()
	if err != nil {
		return nil, fmt.Errorf("ldd %s: %w", binary, err)
	}
	var libs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "linux-vdso") {
			continue
		}
		var path string
		if _, after, found := strings.Cut(line, "=>"); found {
			// e.g. "libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x...)"
			fields := strings.Fields(after)
			if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
				path = fields[0]
			}
		} else if strings.HasPrefix(line, "/") {
			// dynamic loader, e.g. "/lib64/ld-linux-x86-64.so.2 (0x...)"
			path = strings.Fields(line)[0]
		}
		if path != "" {
			libs = append(libs, path)
		}
	}
	return libs, nil
}

// pythonStdlibDirs locates the Python standard library directories under
// /usr/lib (e.g. /usr/lib/python3.11), identified by the presence of os.py. It
// is resolved at runtime rather than hardcoded so a minor-version bump of the
// base image's Python does not silently break the mount.
func pythonStdlibDirs() []string {
	matches, _ := filepath.Glob("/usr/lib/python3.*")
	var dirs []string
	for _, m := range matches {
		if fi, err := os.Stat(filepath.Join(m, "os.py")); err == nil && !fi.IsDir() {
			dirs = append(dirs, m)
		}
	}
	return dirs
}
