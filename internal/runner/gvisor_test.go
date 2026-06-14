package runner

import (
	"slices"
	"strings"
	"testing"
)

// Compile-time assertion that GvisorRunner satisfies the Runner interface, so a
// drift from the interface fails the build (mirrors the assertions for
// NsjailRunner / SubprocessRunner in nsjail_test.go).
var _ Runner = GvisorRunner{}

// testGvisorRunner returns a zero-value-ish GvisorRunner with a fixed rootfs path
// for spec-construction tests. NewGvisorRunner is not used because it probes the
// real runsc binary and on-disk rootfs, which a unit test must not require.
func testGvisorRunner() GvisorRunner {
	return GvisorRunner{Py3RootfsPath: "/opt/gvisor/rootfs/py3"}
}

func TestGvisorLanguageGate(t *testing.T) {
	// Only the python3 interpreter is supported this stage; every other language's
	// command must be rejected so the runner never silently runs an unsupported
	// language under an unprepared rootfs.
	supported := []string{pythonInterpreter}
	for _, cmd := range supported {
		if !gvisorLanguageSupported(cmd) {
			t.Errorf("gvisorLanguageSupported(%q) = false, want true", cmd)
		}
	}
	unsupported := []string{
		bashInterpreter, nodeInterpreter, cppCompiler, cCompiler,
		javacCompiler, javaRuntime, iverilogCompiler, vvpRuntime,
		"./solution", "/usr/bin/perl",
	}
	for _, cmd := range unsupported {
		if gvisorLanguageSupported(cmd) {
			t.Errorf("gvisorLanguageSupported(%q) = true, want false", cmd)
		}
	}
}

func TestGvisorRunRejectsUnsupportedLanguage(t *testing.T) {
	// Run must return a clear error (not crash, not fall back) for a non-py3
	// command, BEFORE attempting to build a bundle or invoke runsc. A non-existent
	// runsc path proves runsc is never reached: the language gate fires first.
	r := GvisorRunner{RunscPath: "/nonexistent/runsc", Py3RootfsPath: "/opt/gvisor/rootfs/py3"}
	_, err := r.Run(t.Context(), RunSpec{Cmd: cppCompiler, Args: []string{"-o", "x", "x.cpp"}})
	if err == nil {
		t.Fatal("Run(cpp) returned nil error, want an unsupported-language error")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("Run(cpp) error = %q, want it to mention the language is not yet supported", err)
	}
}

func TestGvisorBuildOCISpecPy3(t *testing.T) {
	r := testGvisorRunner()
	spec := RunSpec{
		Cmd:         pythonInterpreter,
		Args:        []string{"solution.py"},
		WorkDir:     "/tmp/goboxd-abc",
		WallTimeSec: 9,
		MemoryKB:    102400, // py3 default
		CPUMsPerSec: 1000,
	}
	oci := r.buildOCISpec(spec, "goboxd-gvisor-123")

	// Process args = Cmd + Args, cwd = the work-dir mount point.
	wantArgs := []string{pythonInterpreter, "solution.py"}
	if len(oci.Process.Args) != len(wantArgs) {
		t.Fatalf("process args = %v, want %v", oci.Process.Args, wantArgs)
	}
	for i := range wantArgs {
		if oci.Process.Args[i] != wantArgs[i] {
			t.Fatalf("process args = %v, want %v", oci.Process.Args, wantArgs)
		}
	}
	if oci.Process.Cwd != gvisorWorkDir {
		t.Errorf("cwd = %q, want %q", oci.Process.Cwd, gvisorWorkDir)
	}

	// Root points at the shared rootfs, read-only.
	if oci.Root.Path != r.Py3RootfsPath {
		t.Errorf("root.path = %q, want %q", oci.Root.Path, r.Py3RootfsPath)
	}
	if !oci.Root.Readonly {
		t.Error("root.readonly = false, want true (shared read-only rootfs)")
	}

	// A synthesised /proc mount is what fixes Finding F; assert it is present and
	// that the work dir is bound writable at gvisorWorkDir.
	var haveProc, haveWork bool
	for _, m := range oci.Mounts {
		if m.Destination == "/proc" && m.Type == "proc" {
			haveProc = true
		}
		if m.Destination == gvisorWorkDir {
			haveWork = true
			if m.Source != spec.WorkDir {
				t.Errorf("work mount source = %q, want %q", m.Source, spec.WorkDir)
			}
			if !slices.Contains(m.Options, "rw") {
				t.Errorf("work mount options = %v, want it to include rw", m.Options)
			}
		}
	}
	if !haveProc {
		t.Error("no synthesised /proc mount in spec — Finding F fix depends on it")
	}
	if !haveWork {
		t.Errorf("no %s bind mount in spec", gvisorWorkDir)
	}

	// Per-run cgroup path includes the container id.
	if !strings.Contains(oci.Linux.CgroupsPath, "goboxd-gvisor-123") {
		t.Errorf("cgroupsPath = %q, want it to include the container id", oci.Linux.CgroupsPath)
	}
}

func TestGvisorResourcesTranslation(t *testing.T) {
	spec := RunSpec{MemoryKB: 102400, CPUMsPerSec: 1000, MaxProcesses: 100}
	res := gvisorResources(spec)
	if res == nil {
		t.Fatal("gvisorResources returned nil for a spec with limits")
	}

	// Memory: guest budget + sentry headroom, in bytes, with swap pinned to the
	// same value (hard cap).
	if res.Memory == nil || res.Memory.Limit == nil {
		t.Fatal("memory limit not set")
	}
	wantBytes := int64(102400+gvisorSentryMemHeadroomKB) * 1024
	if *res.Memory.Limit != wantBytes {
		t.Errorf("memory limit = %d, want %d (guest 102400KB + %dKB headroom)", *res.Memory.Limit, wantBytes, gvisorSentryMemHeadroomKB)
	}
	if res.Memory.Swap == nil || *res.Memory.Swap != wantBytes {
		t.Errorf("memory swap = %v, want %d (pinned == limit for a hard cap)", res.Memory.Swap, wantBytes)
	}

	// CPU: CPUMsPerSec ms per second -> quota µs over a 1_000_000 µs period.
	if res.CPU == nil || res.CPU.Quota == nil || res.CPU.Period == nil {
		t.Fatal("cpu quota/period not set")
	}
	if *res.CPU.Quota != 1000*1000 {
		t.Errorf("cpu quota = %d, want %d", *res.CPU.Quota, 1000*1000)
	}
	if *res.CPU.Period != 1_000_000 {
		t.Errorf("cpu period = %d, want 1000000", *res.CPU.Period)
	}
}

// TestGvisorResourcesNoPidsLimit pins the deliberate decision (POC §4/§6) that the
// pids limit is NOT translated in this stage: setting an OCI pids limit trips a
// sentry-startup failure in the WSL2 env, so MaxProcesses is intentionally dropped.
// The ociResources type therefore has no Pids field; this test documents that
// MaxProcesses has no effect on the produced spec.
func TestGvisorResourcesNoPidsLimit(t *testing.T) {
	withPids := gvisorResources(RunSpec{MemoryKB: 102400, CPUMsPerSec: 1000, MaxProcesses: 100})
	withoutPids := gvisorResources(RunSpec{MemoryKB: 102400, CPUMsPerSec: 1000})
	// Memory and CPU must be identical regardless of MaxProcesses.
	if *withPids.Memory.Limit != *withoutPids.Memory.Limit {
		t.Error("MaxProcesses changed the memory limit; it should have no effect this stage")
	}
	if *withPids.CPU.Quota != *withoutPids.CPU.Quota {
		t.Error("MaxProcesses changed the cpu quota; it should have no effect this stage")
	}
}

func TestGvisorResourcesNilWhenUnlimited(t *testing.T) {
	if res := gvisorResources(RunSpec{}); res != nil {
		t.Errorf("gvisorResources(no limits) = %+v, want nil", res)
	}
	// MaxProcesses alone (no mem/cpu) must NOT conjure a resources block, since pids
	// is not translated this stage.
	if res := gvisorResources(RunSpec{MaxProcesses: 50}); res != nil {
		t.Errorf("gvisorResources(only MaxProcesses) = %+v, want nil (pids not translated)", res)
	}
}
