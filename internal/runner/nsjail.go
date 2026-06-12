package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// nsjailGraceSec is added to the wall-time limit when sizing the Go context
// deadline. nsjail enforces the real limit internally via --time_limit; the
// outer context only fires as a backstop if nsjail itself hangs.
const nsjailGraceSec = 5

// pythonInterpreter is the command path the py3 language runs. It is the only
// language wired to the minimal mount-namespace profile so far (Phase 1, step 1);
// every other language still shares the host filesystem via --disable_clone_newns.
const pythonInterpreter = "/usr/bin/python3"

// NsjailRunner runs the command inside an nsjail sandbox. It implements the
// same Runner interface as SubprocessRunner and returns the identical
// RunResult shape, so callers can swap between the two without changes.
//
// Filesystem isolation is being layered on one language at a time. py3 runs in
// its own mount namespace with a minimal read-only root (interpreter + shared
// libraries + standard library) and a writable per-request work directory, so
// the sandboxed code cannot see the container's filesystem. Every other language
// still uses --disable_clone_newns and shares the host filesystem until it gets
// its own mount profile.
//
// The py3 read-only mount list is the same for every request, so it is resolved
// once at startup (via NewNsjailRunner) and cached in py3Mounts rather than
// shelling out to ldd on every Run.
type NsjailRunner struct {
	// NsjailPath is the path to the nsjail binary. Defaults to "nsjail"
	// (resolved via PATH) when empty.
	NsjailPath string

	// py3Mounts holds the resolved read-only --bindmount_ro flag pairs for
	// py3 (interpreter binary, its shared libraries, and the standard library
	// directories). It is populated once by NewNsjailRunner and reused by every
	// request; an empty slice means construction went through the zero value
	// rather than NewNsjailRunner (used only by the compile-time assertions).
	py3Mounts []string
}

// NewNsjailRunner constructs an NsjailRunner with its per-language filesystem
// mount profiles resolved up front. Today that means the py3 read-only mount
// list (interpreter, shared libraries, stdlib): resolving it here, once, avoids
// running ldd per request and surfaces a broken sandbox (e.g. python3 missing)
// at startup instead of on the first request. The returned error is meant to be
// fatal to startup, exactly like a failed language-registry load.
func NewNsjailRunner(ctx context.Context, nsjailPath string) (NsjailRunner, error) {
	mounts, err := resolvePy3Mounts(ctx)
	if err != nil {
		return NsjailRunner{}, fmt.Errorf("nsjail runner: resolve py3 mounts: %w", err)
	}
	return NsjailRunner{NsjailPath: nsjailPath, py3Mounts: mounts}, nil
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
// view. For py3 it builds a minimal mount namespace: a fresh tmpfs root (nsjail's
// default once the mount namespace is enabled) populated only with the interpreter,
// its shared libraries, and the Python standard library — all read-only — plus the
// per-request work directory mounted writable. The host root filesystem is never
// visible, so paths like /etc/passwd simply do not exist inside the sandbox.
//
// The read-only py3 mounts are identical for every request, so they are taken
// from the cache that NewNsjailRunner resolved once at startup. Only the writable
// work directory varies per request and is appended here.
//
// Every other language still gets --disable_clone_newns, which keeps the mount
// namespace disabled so the child shares the host filesystem (and finds its
// compiler/interpreter and WorkDir) exactly as before. Those languages will each
// get their own minimal mount profile in later steps.
func (r NsjailRunner) filesystemArgs(spec RunSpec) []string {
	if spec.Cmd != pythonInterpreter {
		return []string{"--disable_clone_newns"}
	}

	// Start from a copy of the cached read-only mounts (interpreter, shared
	// libraries, stdlib) so the per-request work-dir append never mutates the
	// shared slice.
	args := append([]string(nil), r.py3Mounts...)

	// The per-request work directory, mounted writable. This is where the source
	// file was written and where the program may create artifacts. It is the only
	// writable, non-tmpfs path the sandbox can reach.
	if spec.WorkDir != "" {
		args = append(args, "--bindmount", spec.WorkDir)
	}

	return args
}

// resolvePy3Mounts builds the static read-only --bindmount_ro flag pairs for
// py3: the interpreter binary, the shared libraries it is dynamically linked
// against, and the standard library directories. These never change between
// requests, so NewNsjailRunner resolves them once at startup and caches the
// result. It is a package var (not a plain func) so tests can substitute a fake
// and assert it runs exactly once.
var resolvePy3Mounts = func(ctx context.Context) ([]string, error) {
	// Read-only mount of the interpreter binary. /usr/bin/python3 is a symlink
	// (→ python3.11); resolve it and bind the real file onto the path the command
	// invokes, so execve(/usr/bin/python3) lands on the real binary regardless of
	// how nsjail treats a symlink source.
	realBin, err := filepath.EvalSymlinks(pythonInterpreter)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", pythonInterpreter, err)
	}
	args := []string{"--bindmount_ro", realBin + ":" + pythonInterpreter}

	// Shared libraries the interpreter is dynamically linked against (including
	// the dynamic loader itself). Without these, execve fails before any Python
	// code runs.
	libs, err := sharedLibraries(ctx, pythonInterpreter)
	if err != nil {
		return nil, err
	}
	for _, lib := range libs {
		args = append(args, "--bindmount_ro", lib)
	}

	// The Python standard library directories. The interpreter imports os.py and
	// the lib-dynload C extensions from here at startup; without them Python aborts
	// with "Could not find platform independent libraries <prefix>". A missing
	// stdlib means the sandbox can never run Python, so fail loudly at startup.
	dirs := pythonStdlibDirs()
	if len(dirs) == 0 {
		return nil, fmt.Errorf("python stdlib directory not found under /usr/lib")
	}
	for _, dir := range dirs {
		args = append(args, "--bindmount_ro", dir)
	}

	return args, nil
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
