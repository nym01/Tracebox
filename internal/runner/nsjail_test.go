package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Compile-time assertions that both runner implementations satisfy the
// Runner interface. If either type drifts from the interface, the package
// fails to compile.
var (
	_ Runner = NsjailRunner{}
	_ Runner = SubprocessRunner{}
)

// resolverFn is the signature of the per-language mount resolvers
// (resolvePy3Mounts, resolveBashMounts, resolveJsMounts).
type resolverFn func(context.Context) ([]string, error)

// withStubbedResolvers swaps every per-profile mount resolver for the duration of
// a test and restores them afterward. The three interpreted-language resolvers can
// be supplied by the caller; a nil argument is replaced with a no-op resolver that
// succeeds with no mounts, so a test only has to supply the resolver(s) it cares
// about while the others stay out of the way (and, in particular, never shell out
// to ldd on a host that lacks the interpreter).
//
// The compiled-language resolvers (cpp build, c build, cpp/c run, java build, java
// run, verilog build, verilog run) are always stubbed to no-ops here so
// NewNsjailRunner never shells out to gcc/g++/ldd or inspects the JDK or Icarus
// install during a unit test. A test that exercises those profiles assigns
// resolveCppBuildMounts / resolveCBuildMounts / resolveCppRunMounts /
// resolveJavaBuildMounts / resolveJavaRunMounts / resolveVerilogBuildMounts /
// resolveVerilogRunMounts directly after calling this helper; the originals are
// still restored on cleanup.
func withStubbedResolvers(t *testing.T, py3, bash, js resolverFn) {
	t.Helper()
	origPy3, origBash, origJs := resolvePy3Mounts, resolveBashMounts, resolveJsMounts
	origCppBuild, origCBuild, origCppRun := resolveCppBuildMounts, resolveCBuildMounts, resolveCppRunMounts
	origJavaBuild, origJavaRun := resolveJavaBuildMounts, resolveJavaRunMounts
	origVerilogBuild, origVerilogRun := resolveVerilogBuildMounts, resolveVerilogRunMounts
	origSeccomp := resolveSeccompPolicy
	t.Cleanup(func() {
		resolvePy3Mounts, resolveBashMounts, resolveJsMounts = origPy3, origBash, origJs
		resolveCppBuildMounts, resolveCBuildMounts, resolveCppRunMounts = origCppBuild, origCBuild, origCppRun
		resolveJavaBuildMounts, resolveJavaRunMounts = origJavaBuild, origJavaRun
		resolveVerilogBuildMounts, resolveVerilogRunMounts = origVerilogBuild, origVerilogRun
		resolveSeccompPolicy = origSeccomp
	})
	noop := func(context.Context) ([]string, error) { return nil, nil }
	if py3 == nil {
		py3 = noop
	}
	if bash == nil {
		bash = noop
	}
	if js == nil {
		js = noop
	}
	resolvePy3Mounts, resolveBashMounts, resolveJsMounts = py3, bash, js
	resolveCppBuildMounts, resolveCBuildMounts, resolveCppRunMounts = noop, noop, noop
	resolveJavaBuildMounts, resolveJavaRunMounts = noop, noop
	resolveVerilogBuildMounts, resolveVerilogRunMounts = noop, noop
	// Stub the seccomp policy resolver too so unit tests never touch the on-disk
	// policy file (it lives at configs/seccomp.policy relative to the repo root,
	// not the package dir). The dedicated seccomp test overrides this afterward.
	resolveSeccompPolicy = func(context.Context) (string, error) { return stubSeccompPolicyPath, nil }
}

// stubSeccompPolicyPath is the fixed policy path the stubbed resolver returns in
// unit tests, standing in for the real configs/seccomp.policy.
const stubSeccompPolicyPath = "configs/seccomp.policy"

// assertMountsResolvedOnceAndReused verifies that the expensive mount resolution
// for one interpreted language (which shells out to ldd) happens exactly once, at
// construction, and that every subsequent request for that language reuses the
// cached result plus its own writable work dir rather than re-resolving.
func assertMountsResolvedOnceAndReused(t *testing.T, target string, cached []string) {
	t.Helper()

	var calls int
	stub := func(context.Context) ([]string, error) {
		calls++
		// Return a fresh copy so any accidental mutation by the caller cannot
		// hide a re-resolution behind shared state.
		return append([]string(nil), cached...), nil
	}

	switch target {
	case pythonInterpreter:
		withStubbedResolvers(t, stub, nil, nil)
	case bashInterpreter:
		withStubbedResolvers(t, nil, stub, nil)
	case nodeInterpreter:
		withStubbedResolvers(t, nil, nil, stub)
	default:
		t.Fatalf("unexpected target %q", target)
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected resolver to run once at construction, ran %d times", calls)
	}

	// Many requests, each with a different work directory. None of them should
	// trigger another resolution, and each should carry the cached read-only
	// mounts plus its own writable work dir.
	for i := range 5 {
		workDir := fmt.Sprintf("/tmp/goboxd-%d", i)
		got := r.filesystemArgs(RunSpec{Cmd: target, WorkDir: workDir})

		want := append(append([]string(nil), cached...), "--bindmount", workDir)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d: filesystemArgs = %v, want %v", i, got, want)
		}
	}

	if calls != 1 {
		t.Fatalf("resolver re-ran on subsequent requests: ran %d times total", calls)
	}
}

// TestPy3MountsResolvedOnceAndReused covers the py3 mount cache.
func TestPy3MountsResolvedOnceAndReused(t *testing.T) {
	assertMountsResolvedOnceAndReused(t, pythonInterpreter, []string{
		"--bindmount_ro", "/usr/bin/python3.11:/usr/bin/python3",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--bindmount_ro", "/usr/lib/python3.11",
	})
}

// TestBashMountsResolvedOnceAndReused covers the bash mount cache, which (unlike
// py3) carries only the binary and its shared libraries — no stdlib directory.
func TestBashMountsResolvedOnceAndReused(t *testing.T) {
	assertMountsResolvedOnceAndReused(t, bashInterpreter, []string{
		"--bindmount_ro", "/usr/bin/bash:/usr/bin/bash",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libtinfo.so.6",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
	})
}

// TestJsMountsResolvedOnceAndReused covers the js (node) mount cache, which
// carries the binary, its shared libraries, and Debian's externalized-builtins
// directory (node aborts at startup without it).
func TestJsMountsResolvedOnceAndReused(t *testing.T) {
	assertMountsResolvedOnceAndReused(t, nodeInterpreter, []string{
		"--bindmount_ro", "/usr/bin/node:/usr/bin/node",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libnode.so.108",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--bindmount_ro", "/usr/share/nodejs",
	})
}

// TestFilesystemArgsDoesNotMutateCache guards against the per-request work-dir
// append corrupting a shared cached slice across requests, for each isolated
// language.
func TestFilesystemArgsDoesNotMutateCache(t *testing.T) {
	py3Cached := []string{"--bindmount_ro", "/usr/bin/python3.11:/usr/bin/python3"}
	bashCached := []string{"--bindmount_ro", "/usr/bin/bash:/usr/bin/bash"}
	jsCached := []string{"--bindmount_ro", "/usr/bin/nodejs:/usr/bin/node"}

	copyOf := func(s []string) resolverFn {
		return func(context.Context) ([]string, error) { return append([]string(nil), s...), nil }
	}
	withStubbedResolvers(t, copyOf(py3Cached), copyOf(bashCached), copyOf(jsCached))

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	cases := []struct {
		cmd    string
		cached []string
		field  []string
	}{
		{pythonInterpreter, py3Cached, r.py3Mounts},
		{bashInterpreter, bashCached, r.bashMounts},
		{nodeInterpreter, jsCached, r.jsMounts},
	}
	for _, c := range cases {
		r.filesystemArgs(RunSpec{Cmd: c.cmd, WorkDir: "/tmp/a"})
		r.filesystemArgs(RunSpec{Cmd: c.cmd, WorkDir: "/tmp/b"})
		if !reflect.DeepEqual(c.field, c.cached) {
			t.Fatalf("%s: cached mounts were mutated: got %v, want %v", c.cmd, c.field, c.cached)
		}
	}
}

// TestNewNsjailRunnerFailsLoudlyOnResolveError confirms a resolution failure at
// startup surfaces as a construction error (so main can make it fatal) rather
// than being deferred to the first request — for each interpreted language.
func TestNewNsjailRunnerFailsLoudlyOnResolveError(t *testing.T) {
	fail := func(context.Context) ([]string, error) { return nil, fmt.Errorf("interpreter not found") }

	cases := []struct {
		name          string
		py3, bash, js resolverFn
	}{
		{name: "py3", py3: fail},
		{name: "bash", bash: fail},
		{name: "js", js: fail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStubbedResolvers(t, c.py3, c.bash, c.js)
			if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
				t.Fatalf("expected NewNsjailRunner to error when %s mounts cannot be resolved", c.name)
			}
		})
	}
}

// TestFilesystemArgsUnknownCommandFallsBack confirms the --disable_clone_newns
// fallback still applies to a genuinely unknown command — one that matches none of
// the seven language profiles. Every real language (py3, bash, js, g++, gcc, javac,
// java, iverilog, vvp and "./solution") now has its own isolated profile, so this
// default branch is only a safety net for an unrecognised command.
func TestFilesystemArgsUnknownCommandFallsBack(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	for _, cmd := range []string{"/usr/bin/some-unknown-tool", "/bin/cat"} {
		got := r.filesystemArgs(RunSpec{Cmd: cmd, WorkDir: "/tmp/x"})
		want := []string{"--disable_clone_newns"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s filesystemArgs = %v, want %v", cmd, got, want)
		}
	}
}

// TestCppBuildArgs verifies the g++ build step gets its cached build mounts, a
// writable tmpfs /tmp (g++ needs scratch space for intermediate files), and the
// per-request work directory mounted writable — in that order.
func TestCppBuildArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	buildCache := []string{
		"--bindmount_ro", "/usr/include",
		"--bindmount_ro", "/usr/lib/gcc/x86_64-linux-gnu/12",
		"--bindmount_ro", "/usr/bin/g++",
	}
	resolveCppBuildMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), buildCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: cppCompiler, WorkDir: "/tmp/build"})
	want := append(append([]string(nil), buildCache...),
		"--tmpfsmount", "/tmp",
		"--bindmount", "/tmp/build",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpp build filesystemArgs = %v, want %v", got, want)
	}
}

// TestCppRunArgs verifies the compiled-artifact run step ("./solution") gets its
// cached minimal library mounts plus the writable work directory, and crucially
// NO tmpfs /tmp (the untrusted binary should get nothing it does not need).
func TestCppRunArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	runCache := []string{
		"--bindmount_ro", "/usr/lib/x86_64-linux-gnu/libstdc++.so.6",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--bindmount_ro", "/lib64/ld-linux-x86-64.so.2",
	}
	resolveCppRunMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), runCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: "./solution", WorkDir: "/tmp/run"})
	want := append(append([]string(nil), runCache...), "--bindmount", "/tmp/run")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpp run filesystemArgs = %v, want %v", got, want)
	}
	for _, a := range got {
		if a == "--tmpfsmount" {
			t.Fatalf("run step must not mount a tmpfs /tmp: %v", got)
		}
	}
}

// TestCppMountsResolvedOnceAndReused confirms the two cpp profiles (which shell
// out to g++) are resolved exactly once, at construction, and reused across
// requests with only the per-request work dir appended.
func TestCppMountsResolvedOnceAndReused(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)

	var buildCalls, runCalls int
	buildCache := []string{"--bindmount_ro", "/usr/bin/g++"}
	runCache := []string{"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6"}
	resolveCppBuildMounts = func(context.Context) ([]string, error) {
		buildCalls++
		return append([]string(nil), buildCache...), nil
	}
	resolveCppRunMounts = func(context.Context) ([]string, error) {
		runCalls++
		return append([]string(nil), runCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if buildCalls != 1 || runCalls != 1 {
		t.Fatalf("expected each cpp resolver to run once at construction, got build=%d run=%d", buildCalls, runCalls)
	}

	for i := range 5 {
		workDir := fmt.Sprintf("/tmp/goboxd-%d", i)

		gotBuild := r.filesystemArgs(RunSpec{Cmd: cppCompiler, WorkDir: workDir})
		wantBuild := append(append([]string(nil), buildCache...), "--tmpfsmount", "/tmp", "--bindmount", workDir)
		if !reflect.DeepEqual(gotBuild, wantBuild) {
			t.Fatalf("build request %d: filesystemArgs = %v, want %v", i, gotBuild, wantBuild)
		}

		gotRun := r.filesystemArgs(RunSpec{Cmd: "./solution", WorkDir: workDir})
		wantRun := append(append([]string(nil), runCache...), "--bindmount", workDir)
		if !reflect.DeepEqual(gotRun, wantRun) {
			t.Fatalf("run request %d: filesystemArgs = %v, want %v", i, gotRun, wantRun)
		}
	}

	if buildCalls != 1 || runCalls != 1 {
		t.Fatalf("cpp resolver re-ran on later requests: build=%d run=%d", buildCalls, runCalls)
	}

	// The cached slices must not have been mutated by the per-request appends.
	if !reflect.DeepEqual(r.cppBuildMounts, buildCache) {
		t.Fatalf("cpp build cache mutated: got %v, want %v", r.cppBuildMounts, buildCache)
	}
	if !reflect.DeepEqual(r.cppRunMounts, runCache) {
		t.Fatalf("cpp run cache mutated: got %v, want %v", r.cppRunMounts, runCache)
	}
}

// TestCBuildArgs verifies the gcc build step gets its cached build mounts, a
// writable tmpfs /tmp (gcc, like g++, needs scratch space for intermediate files),
// and the per-request work directory mounted writable — in that order. It mirrors
// TestCppBuildArgs for the c build profile.
func TestCBuildArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	buildCache := []string{
		"--bindmount_ro", "/usr/include",
		"--bindmount_ro", "/usr/lib/gcc/x86_64-linux-gnu/12",
		"--bindmount_ro", "/usr/bin/gcc",
	}
	resolveCBuildMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), buildCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: cCompiler, WorkDir: "/tmp/build"})
	want := append(append([]string(nil), buildCache...),
		"--tmpfsmount", "/tmp",
		"--bindmount", "/tmp/build",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("c build filesystemArgs = %v, want %v", got, want)
	}
}

// TestCMountsResolvedOnceAndReused confirms the c build profile (which shells out
// to gcc) is resolved exactly once, at construction, and reused across requests
// with only the per-request work dir appended. The c run step has no separate
// resolver — it shares the cpp run profile — so only the build cache is checked
// here. Mirrors TestCppMountsResolvedOnceAndReused.
func TestCMountsResolvedOnceAndReused(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)

	var buildCalls int
	buildCache := []string{"--bindmount_ro", "/usr/bin/gcc"}
	resolveCBuildMounts = func(context.Context) ([]string, error) {
		buildCalls++
		return append([]string(nil), buildCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if buildCalls != 1 {
		t.Fatalf("expected c build resolver to run once at construction, got %d", buildCalls)
	}

	for i := range 5 {
		workDir := fmt.Sprintf("/tmp/goboxd-%d", i)
		got := r.filesystemArgs(RunSpec{Cmd: cCompiler, WorkDir: workDir})
		want := append(append([]string(nil), buildCache...), "--tmpfsmount", "/tmp", "--bindmount", workDir)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("build request %d: filesystemArgs = %v, want %v", i, got, want)
		}
	}

	if buildCalls != 1 {
		t.Fatalf("c build resolver re-ran on later requests: %d", buildCalls)
	}
	if !reflect.DeepEqual(r.cBuildMounts, buildCache) {
		t.Fatalf("c build cache mutated: got %v, want %v", r.cBuildMounts, buildCache)
	}
}

// TestNewNsjailRunnerFailsLoudlyOnCResolveError confirms a c build resolution
// failure at startup surfaces as a construction error, mirroring the cpp case.
func TestNewNsjailRunnerFailsLoudlyOnCResolveError(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	resolveCBuildMounts = func(context.Context) ([]string, error) {
		return nil, fmt.Errorf("gcc not found")
	}
	if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
		t.Fatal("expected NewNsjailRunner to error when c build mounts cannot be resolved")
	}
}

// TestJavaBuildArgs verifies the javac build step gets its cached build mounts, a
// writable tmpfs /tmp (javac runs on the JVM, which writes perf-data scratch under
// /tmp), and the per-request work directory mounted writable — in that order.
func TestJavaBuildArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	buildCache := []string{
		"--bindmount_ro", "/usr/lib/jvm/java-17-openjdk-amd64",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--symlink", "/usr/lib/jvm/java-17-openjdk-amd64/bin/javac:/usr/bin/javac",
	}
	resolveJavaBuildMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), buildCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: javacCompiler, WorkDir: "/tmp/build"})
	want := append(append([]string(nil), buildCache...),
		"--tmpfsmount", "/tmp",
		"--bindmount", "/tmp/build",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("java build filesystemArgs = %v, want %v", got, want)
	}
}

// TestJavaRunArgs verifies the java run step gets its cached mounts plus the
// writable work directory, and crucially NO tmpfs /tmp (the security-critical run
// step gets nothing it does not need; the JVM degrades gracefully without it).
func TestJavaRunArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	runCache := []string{
		"--bindmount_ro", "/usr/lib/jvm/java-17-openjdk-amd64",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--symlink", "/usr/lib/jvm/java-17-openjdk-amd64/bin/java:/usr/bin/java",
	}
	resolveJavaRunMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), runCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: javaRuntime, WorkDir: "/tmp/run"})
	want := append(append([]string(nil), runCache...), "--bindmount", "/tmp/run")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("java run filesystemArgs = %v, want %v", got, want)
	}
	for _, a := range got {
		if a == "--tmpfsmount" {
			t.Fatalf("java run step must not mount a tmpfs /tmp: %v", got)
		}
	}
}

// TestJavaMountsResolvedOnceAndReused confirms the two java profiles (which inspect
// the JDK on the host) are resolved exactly once, at construction, and reused
// across requests with only the per-request work dir (and, for the build step,
// the tmpfs /tmp) appended. Mirrors TestCppMountsResolvedOnceAndReused.
func TestJavaMountsResolvedOnceAndReused(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)

	var buildCalls, runCalls int
	buildCache := []string{"--bindmount_ro", "/usr/lib/jvm/java-17-openjdk-amd64", "--symlink", "/jdk/bin/javac:/usr/bin/javac"}
	runCache := []string{"--bindmount_ro", "/usr/lib/jvm/java-17-openjdk-amd64", "--symlink", "/jdk/bin/java:/usr/bin/java"}
	resolveJavaBuildMounts = func(context.Context) ([]string, error) {
		buildCalls++
		return append([]string(nil), buildCache...), nil
	}
	resolveJavaRunMounts = func(context.Context) ([]string, error) {
		runCalls++
		return append([]string(nil), runCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if buildCalls != 1 || runCalls != 1 {
		t.Fatalf("expected each java resolver to run once at construction, got build=%d run=%d", buildCalls, runCalls)
	}

	for i := range 5 {
		workDir := fmt.Sprintf("/tmp/goboxd-%d", i)

		gotBuild := r.filesystemArgs(RunSpec{Cmd: javacCompiler, WorkDir: workDir})
		wantBuild := append(append([]string(nil), buildCache...), "--tmpfsmount", "/tmp", "--bindmount", workDir)
		if !reflect.DeepEqual(gotBuild, wantBuild) {
			t.Fatalf("build request %d: filesystemArgs = %v, want %v", i, gotBuild, wantBuild)
		}

		gotRun := r.filesystemArgs(RunSpec{Cmd: javaRuntime, WorkDir: workDir})
		wantRun := append(append([]string(nil), runCache...), "--bindmount", workDir)
		if !reflect.DeepEqual(gotRun, wantRun) {
			t.Fatalf("run request %d: filesystemArgs = %v, want %v", i, gotRun, wantRun)
		}
	}

	if buildCalls != 1 || runCalls != 1 {
		t.Fatalf("java resolver re-ran on later requests: build=%d run=%d", buildCalls, runCalls)
	}
	if !reflect.DeepEqual(r.javaBuildMounts, buildCache) {
		t.Fatalf("java build cache mutated: got %v, want %v", r.javaBuildMounts, buildCache)
	}
	if !reflect.DeepEqual(r.javaRunMounts, runCache) {
		t.Fatalf("java run cache mutated: got %v, want %v", r.javaRunMounts, runCache)
	}
}

// TestNewNsjailRunnerFailsLoudlyOnJavaResolveError confirms a java resolution
// failure at startup surfaces as a construction error, for both java profiles.
func TestNewNsjailRunnerFailsLoudlyOnJavaResolveError(t *testing.T) {
	fail := func(context.Context) ([]string, error) { return nil, fmt.Errorf("jdk not found") }

	t.Run("build", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveJavaBuildMounts = fail
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when java build mounts cannot be resolved")
		}
	})
	t.Run("run", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveJavaRunMounts = fail
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when java run mounts cannot be resolved")
		}
	})
}

// TestVerilogBuildArgs verifies the iverilog build step gets its cached build
// mounts, a writable tmpfs /tmp (iverilog writes its intermediate command file
// there), and the per-request work directory mounted writable — in that order.
func TestVerilogBuildArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	buildCache := []string{
		"--bindmount_ro", "/usr/lib/ivl",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--bindmount_ro", "/usr/bin/iverilog:/usr/bin/iverilog",
	}
	resolveVerilogBuildMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), buildCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: iverilogCompiler, WorkDir: "/tmp/build"})
	want := append(append([]string(nil), buildCache...),
		"--tmpfsmount", "/tmp",
		"--bindmount", "/tmp/build",
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verilog build filesystemArgs = %v, want %v", got, want)
	}
}

// TestVerilogRunArgs verifies the vvp run step gets its cached mounts plus the
// writable work directory, and crucially NO tmpfs /tmp (the security-critical run
// step gets nothing it does not need).
func TestVerilogRunArgs(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	runCache := []string{
		"--bindmount_ro", "/usr/lib/ivl",
		"--bindmount_ro", "/lib/x86_64-linux-gnu/libc.so.6",
		"--bindmount_ro", "/usr/bin/vvp:/usr/bin/vvp",
	}
	resolveVerilogRunMounts = func(context.Context) ([]string, error) {
		return append([]string(nil), runCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	got := r.filesystemArgs(RunSpec{Cmd: vvpRuntime, WorkDir: "/tmp/run"})
	want := append(append([]string(nil), runCache...), "--bindmount", "/tmp/run")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verilog run filesystemArgs = %v, want %v", got, want)
	}
	for _, a := range got {
		if a == "--tmpfsmount" {
			t.Fatalf("verilog run step must not mount a tmpfs /tmp: %v", got)
		}
	}
}

// TestVerilogMountsResolvedOnceAndReused confirms the two verilog profiles (which
// inspect the Icarus install on the host) are resolved exactly once, at
// construction, and reused across requests with only the per-request work dir (and,
// for the build step, the tmpfs /tmp) appended. Mirrors the java/cpp cases.
func TestVerilogMountsResolvedOnceAndReused(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)

	var buildCalls, runCalls int
	buildCache := []string{"--bindmount_ro", "/usr/lib/ivl", "--bindmount_ro", "/usr/bin/iverilog:/usr/bin/iverilog"}
	runCache := []string{"--bindmount_ro", "/usr/lib/ivl", "--bindmount_ro", "/usr/bin/vvp:/usr/bin/vvp"}
	resolveVerilogBuildMounts = func(context.Context) ([]string, error) {
		buildCalls++
		return append([]string(nil), buildCache...), nil
	}
	resolveVerilogRunMounts = func(context.Context) ([]string, error) {
		runCalls++
		return append([]string(nil), runCache...), nil
	}

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if buildCalls != 1 || runCalls != 1 {
		t.Fatalf("expected each verilog resolver to run once at construction, got build=%d run=%d", buildCalls, runCalls)
	}

	for i := range 5 {
		workDir := fmt.Sprintf("/tmp/goboxd-%d", i)

		gotBuild := r.filesystemArgs(RunSpec{Cmd: iverilogCompiler, WorkDir: workDir})
		wantBuild := append(append([]string(nil), buildCache...), "--tmpfsmount", "/tmp", "--bindmount", workDir)
		if !reflect.DeepEqual(gotBuild, wantBuild) {
			t.Fatalf("build request %d: filesystemArgs = %v, want %v", i, gotBuild, wantBuild)
		}

		gotRun := r.filesystemArgs(RunSpec{Cmd: vvpRuntime, WorkDir: workDir})
		wantRun := append(append([]string(nil), runCache...), "--bindmount", workDir)
		if !reflect.DeepEqual(gotRun, wantRun) {
			t.Fatalf("run request %d: filesystemArgs = %v, want %v", i, gotRun, wantRun)
		}
	}

	if buildCalls != 1 || runCalls != 1 {
		t.Fatalf("verilog resolver re-ran on later requests: build=%d run=%d", buildCalls, runCalls)
	}
	if !reflect.DeepEqual(r.verilogBuildMounts, buildCache) {
		t.Fatalf("verilog build cache mutated: got %v, want %v", r.verilogBuildMounts, buildCache)
	}
	if !reflect.DeepEqual(r.verilogRunMounts, runCache) {
		t.Fatalf("verilog run cache mutated: got %v, want %v", r.verilogRunMounts, runCache)
	}
}

// TestNewNsjailRunnerFailsLoudlyOnVerilogResolveError confirms a verilog resolution
// failure at startup surfaces as a construction error, for both verilog profiles.
func TestNewNsjailRunnerFailsLoudlyOnVerilogResolveError(t *testing.T) {
	fail := func(context.Context) ([]string, error) { return nil, fmt.Errorf("icarus not found") }

	t.Run("build", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveVerilogBuildMounts = fail
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when verilog build mounts cannot be resolved")
		}
	})
	t.Run("run", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveVerilogRunMounts = fail
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when verilog run mounts cannot be resolved")
		}
	})
}

// seccompFlagIndex returns the index of "--seccomp_policy" in args, or -1 if it
// is absent. The flag's value is the following element.
func seccompFlagIndex(args []string) int {
	for i, a := range args {
		if a == "--seccomp_policy" {
			return i
		}
	}
	return -1
}

// TestSeccompPolicyResolvedAndStored confirms the policy path is resolved at
// construction and stored on the runner, and that a resolution failure makes
// construction fail loudly (so main can treat it as fatal) rather than deferring
// the error to the first request.
func TestSeccompPolicyResolvedAndStored(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	resolveSeccompPolicy = func(context.Context) (string, error) { return "/some/seccomp.policy", nil }

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}
	if r.SeccompPolicyPath != "/some/seccomp.policy" {
		t.Fatalf("SeccompPolicyPath = %q, want %q", r.SeccompPolicyPath, "/some/seccomp.policy")
	}

	t.Run("fails loudly", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveSeccompPolicy = func(context.Context) (string, error) {
			return "", fmt.Errorf("seccomp policy not found")
		}
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when the seccomp policy cannot be resolved")
		}
	})
}

// TestSeccompPolicyPassedToNsjailForEveryLanguage is the core seccomp test: it
// confirms the resolved policy file is handed to nsjail via --seccomp_policy for
// every one of the seven languages' command variants (interpreters, both compiler
// build steps, both compiled-artifact run steps, and the java/verilog launchers).
// The filter must be uniform — applied to all languages identically — and must
// target nsjail itself, i.e. appear before the "--" that separates nsjail's flags
// from the sandboxed command.
func TestSeccompPolicyPassedToNsjailForEveryLanguage(t *testing.T) {
	withStubbedResolvers(t, nil, nil, nil)
	const policy = "configs/seccomp.policy"
	resolveSeccompPolicy = func(context.Context) (string, error) { return policy, nil }

	r, err := NewNsjailRunner(context.Background(), "nsjail")
	if err != nil {
		t.Fatalf("NewNsjailRunner: %v", err)
	}

	// Every command the seven languages can invoke. Each must carry the seccomp
	// flag; none is exempt.
	commands := map[string]string{
		"py3":           pythonInterpreter,
		"bash":          bashInterpreter,
		"js":            nodeInterpreter,
		"cpp build":     cppCompiler,
		"c build":       cCompiler,
		"compiled run":  "./solution",
		"java build":    javacCompiler,
		"java run":      javaRuntime,
		"verilog build": iverilogCompiler,
		"verilog run":   vvpRuntime,
	}

	for name, cmd := range commands {
		t.Run(name, func(t *testing.T) {
			args := r.buildNsjailArgs(RunSpec{Cmd: cmd, WorkDir: "/tmp/work"}, 10)

			idx := seccompFlagIndex(args)
			if idx < 0 {
				t.Fatalf("%s: --seccomp_policy missing from nsjail args: %v", cmd, args)
			}
			if idx+1 >= len(args) || args[idx+1] != policy {
				t.Fatalf("%s: --seccomp_policy value = %q, want %q", cmd, args[idx+1], policy)
			}

			// The flag must configure nsjail, not be handed to the sandboxed
			// program, so it must come before the "--" separator.
			sep := -1
			for i, a := range args {
				if a == "--" {
					sep = i
					break
				}
			}
			if sep < 0 {
				t.Fatalf("%s: no \"--\" separator in args: %v", cmd, args)
			}
			if idx > sep {
				t.Fatalf("%s: --seccomp_policy (idx %d) appears after \"--\" (idx %d)", cmd, idx, sep)
			}
		})
	}

	// An unknown command (the --disable_clone_newns fallback) must still get the
	// seccomp filter — it is uniform across every invocation, not tied to a
	// recognised language profile.
	t.Run("unknown command still filtered", func(t *testing.T) {
		args := r.buildNsjailArgs(RunSpec{Cmd: "/bin/some-unknown-tool", WorkDir: "/tmp/work"}, 10)
		if seccompFlagIndex(args) < 0 {
			t.Fatalf("unknown command: --seccomp_policy missing: %v", args)
		}
	})
}

// TestParseIncludeDirs checks the header-search block is extracted from
// `g++ -E -v` output and surrounding noise is ignored.
func TestParseIncludeDirs(t *testing.T) {
	out := `ignored preamble
#include "..." search starts here:
 /should/be/ignored/quote/path
#include <...> search starts here:
 /usr/include/c++/12
 /usr/include/x86_64-linux-gnu/c++/12
 /usr/lib/gcc/x86_64-linux-gnu/12/include
 /usr/include
End of search list.
trailing noise that mentions /not/a/dir`

	got := parseIncludeDirs(out)
	want := []string{
		"/usr/include/c++/12",
		"/usr/include/x86_64-linux-gnu/c++/12",
		"/usr/lib/gcc/x86_64-linux-gnu/12/include",
		"/usr/include",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIncludeDirs = %v, want %v", got, want)
	}
}

// TestParseSearchDirs checks the install dir and library dirs are extracted and
// path-cleaned from `g++ -print-search-dirs` output.
func TestParseSearchDirs(t *testing.T) {
	out := `install: /usr/lib/gcc/x86_64-linux-gnu/12/
programs: =/usr/lib/gcc/x86_64-linux-gnu/12/:/usr/bin/
libraries: =/usr/lib/gcc/x86_64-linux-gnu/12/:/usr/lib/gcc/x86_64-linux-gnu/12/../../../x86_64-linux-gnu/:/lib/x86_64-linux-gnu/:/usr/lib/`

	libDirs, installDir := parseSearchDirs(out)
	if installDir != "/usr/lib/gcc/x86_64-linux-gnu/12" {
		t.Fatalf("installDir = %q, want %q", installDir, "/usr/lib/gcc/x86_64-linux-gnu/12")
	}
	want := []string{
		"/usr/lib/gcc/x86_64-linux-gnu/12",
		"/usr/lib/x86_64-linux-gnu",
		"/lib/x86_64-linux-gnu",
		"/usr/lib",
	}
	if !reflect.DeepEqual(libDirs, want) {
		t.Fatalf("parseSearchDirs libDirs = %v, want %v", libDirs, want)
	}
}

// TestIsIverilogBaseDir checks the content-based validation of an Icarus base
// directory: a dir with the ivl binary qualifies, a dir with only a .conf/.sft
// control file qualifies, and an unrelated dir (or a non-directory) does not.
func TestIsIverilogBaseDir(t *testing.T) {
	root := t.TempDir()
	mk := func(p string, isDir bool) string {
		full := filepath.Join(root, p)
		if isDir {
			if err := os.MkdirAll(full, 0755); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return full
	}

	withIvl := mk("ivldir", true)
	mk("ivldir/ivl", false)
	withConf := mk("confdir", true)
	mk("confdir/vvp.conf", false)
	empty := mk("emptydir", true)
	notDir := mk("afile", false)

	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"has ivl binary", withIvl, true},
		{"has conf file", withConf, true},
		{"empty dir", empty, false},
		{"not a directory", notDir, false},
		{"missing", filepath.Join(root, "nope"), false},
	}
	for _, c := range cases {
		if got := isIverilogBaseDir(c.dir); got != c.want {
			t.Errorf("%s: isIverilogBaseDir(%q) = %v, want %v", c.name, c.dir, got, c.want)
		}
	}
}

// TestIverilogModuleFiles checks that the ivl/ivlpp helper executables and the
// *.tgt / *.vpi modules are collected from the base directories, that a missing
// helper is simply omitted, and that plain config files are not returned.
func TestIverilogModuleFiles(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "ivl")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.WriteFile(p, []byte("x"), 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ivl := mk("ivl")
	vvpTgt := mk("vvp.tgt")
	systemVpi := mk("system.vpi")
	mk("vvp.conf") // config file: must NOT be returned
	// ivlpp and vhdlpp deliberately absent: they should just be skipped, not error.

	got := iverilogModuleFiles([]string{base})
	want := map[string]bool{ivl: true, vvpTgt: true, systemVpi: true}
	if len(got) != len(want) {
		t.Fatalf("iverilogModuleFiles = %v, want the 3 module files %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("iverilogModuleFiles returned unexpected %q (got %v)", p, got)
		}
	}
}

// TestMountSetDeduplicatesAndAvoidsOverlap exercises the mountSet overlap rules
// against a real temp tree: nested directories collapse to the most specific
// disjoint set, files inside a mounted directory are skipped, and a file outside
// any mounted directory is bound individually.
func TestMountSetDeduplicatesAndAvoidsOverlap(t *testing.T) {
	root := t.TempDir()
	mkdir := func(p string) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(full, 0755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	mkfile := func(p string) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		return full
	}

	libDir := mkdir("usr/lib")
	libChild := mkdir("usr/lib/x86_64") // nested under libDir
	incDir := mkdir("usr/include")      // disjoint
	libFile := mkfile("usr/lib/libc.so")
	binFile := mkfile("usr/bin/ld")

	m := newMountSet()
	m.addDirRO(libDir)
	m.addDirRO(libChild) // nested → skipped
	m.addDirRO(incDir)   // disjoint → kept
	m.addDirRO(libDir)   // duplicate → skipped
	m.addFileRO(libFile) // inside libDir → skipped
	m.addFileRO(binFile) // outside any dir → kept

	want := []string{
		"--bindmount_ro", libDir,
		"--bindmount_ro", incDir,
		"--bindmount_ro", binFile,
	}
	if !reflect.DeepEqual(m.args(), want) {
		t.Fatalf("mountSet args = %v, want %v", m.args(), want)
	}
}

// TestMountSetAncestorReplacesDescendant checks the reverse overlap case: adding a
// broad parent directory after a specific child is already mounted drops the child
// and keeps the parent, because the parent mount already exposes the child (and
// may expose sibling files the child does not). This is what makes /usr/include
// win over /usr/include/x86_64-linux-gnu so top-level headers like features.h stay
// reachable.
func TestMountSetAncestorReplacesDescendant(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "usr/include/x86_64")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "usr/include")

	m := newMountSet()
	m.addDirRO(child)  // specific
	m.addDirRO(parent) // ancestor of child → replaces it

	want := []string{"--bindmount_ro", parent}
	if !reflect.DeepEqual(m.args(), want) {
		t.Fatalf("mountSet args = %v, want %v", m.args(), want)
	}
}

// TestNewNsjailRunnerFailsLoudlyOnCppResolveError confirms a cpp resolution
// failure at startup surfaces as a construction error, for both cpp profiles.
func TestNewNsjailRunnerFailsLoudlyOnCppResolveError(t *testing.T) {
	fail := func(context.Context) ([]string, error) { return nil, fmt.Errorf("g++ not found") }

	t.Run("build", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveCppBuildMounts = fail
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when cpp build mounts cannot be resolved")
		}
	})
	t.Run("run", func(t *testing.T) {
		withStubbedResolvers(t, nil, nil, nil)
		resolveCppRunMounts = fail
		if _, err := NewNsjailRunner(context.Background(), "nsjail"); err == nil {
			t.Fatal("expected NewNsjailRunner to error when cpp run mounts cannot be resolved")
		}
	})
}
