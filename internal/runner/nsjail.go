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
// C++ binary's. The remaining compiled languages (java, verilog) still share the
// host filesystem via --disable_clone_newns until they get their own profiles.
const (
	pythonInterpreter = "/usr/bin/python3"
	bashInterpreter   = "/usr/bin/bash"
	nodeInterpreter   = "/usr/bin/node"
	cppCompiler       = "/usr/bin/g++"
	cCompiler         = "/usr/bin/gcc"
)

// artifactRunPrefix is how the language registry invokes a compiled artifact: as
// a path relative to the work directory, e.g. "./solution". Both cpp and c run
// their binaries this way, so a command starting with this prefix is the run step
// of a compiled language and gets the compiled-artifact mount profile.
const artifactRunPrefix = "./"

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
// filesystemArgs for why the build and run profiles differ. The remaining
// compiled languages (java, verilog) still use --disable_clone_newns and share
// the host filesystem until they get their own mount profiles.
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
	return NsjailRunner{
		NsjailPath:     nsjailPath,
		py3Mounts:      py3,
		bashMounts:     bash,
		jsMounts:       js,
		cppBuildMounts: cppBuild,
		cBuildMounts:   cBuild,
		cppRunMounts:   cppRun,
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

	nsjailArgs := []string{
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

	// Filesystem isolation: py3 gets a fresh mount namespace with a minimal
	// read-only root; every other language still shares the host filesystem.
	nsjailArgs = append(nsjailArgs, r.filesystemArgs(spec)...)

	// Run inside the per-request working directory so relative artifacts (e.g.
	// ./solution) and the source file resolve exactly as under SubprocessRunner.
	if spec.WorkDir != "" {
		nsjailArgs = append(nsjailArgs, "--cwd", spec.WorkDir)
	}
	// Everything after "--" is the command and its arguments.
	nsjailArgs = append(nsjailArgs, "--", spec.Cmd)
	nsjailArgs = append(nsjailArgs, spec.Args...)

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

// filesystemArgs returns the nsjail flags that set up the child's filesystem
// view. For every isolated profile it builds a minimal mount namespace: a fresh
// tmpfs root (nsjail's default once the mount namespace is enabled) populated only
// with that profile's read-only mounts plus the per-request work directory mounted
// writable. The host root filesystem is never visible, so paths like /etc/passwd
// simply do not exist inside the sandbox.
//
// There are five profiles:
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
//
// Each profile's read-only mounts are identical for every request, so they are
// taken from the cache that NewNsjailRunner resolved once at startup. Only the
// writable work directory varies per request and is appended here.
//
// Every other command still gets --disable_clone_newns, which keeps the mount
// namespace disabled so the child shares the host filesystem (and finds its
// compiler/interpreter and WorkDir) exactly as before. Those languages will each
// get their own minimal mount profile in later steps.
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
