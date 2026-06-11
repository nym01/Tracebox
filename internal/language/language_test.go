package language

import (
	"log"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := LoadRegistry("../../configs/languages.yaml"); err != nil {
		log.Fatal(err)
	}
	os.Exit(m.Run())
}

func TestLookupPy3(t *testing.T) {
	lang, ok := Lookup("py3")
	if !ok {
		t.Fatal("py3 must be registered")
	}
	if lang.ID != "py3" {
		t.Errorf("ID: want py3, got %q", lang.ID)
	}
	if lang.Build != nil {
		t.Error("py3 must not have a build config")
	}
	if lang.Run.Cmd == "" {
		t.Error("py3 Run.Cmd must not be empty")
	}
	if lang.SourceFilename == "" {
		t.Error("py3 SourceFilename must not be empty")
	}
	if lang.Run.Limits.WallTimeS <= 0 {
		t.Error("py3 Run.Limits.WallTimeS must be positive")
	}
}

func TestLookupCpp(t *testing.T) {
	lang, ok := Lookup("cpp")
	if !ok {
		t.Fatal("cpp must be registered")
	}
	if lang.ID != "cpp" {
		t.Errorf("ID: want cpp, got %q", lang.ID)
	}
	if lang.Build == nil {
		t.Fatal("cpp must have a build config")
	}
	if lang.Build.Cmd == "" {
		t.Error("cpp Build.Cmd must not be empty")
	}
	if lang.Run.Cmd == "" {
		t.Error("cpp Run.Cmd must not be empty")
	}
	if lang.Build.Limits.WallTimeS <= 0 {
		t.Error("cpp Build.Limits.WallTimeS must be positive")
	}
}

func TestLookupC(t *testing.T) {
	lang, ok := Lookup("c")
	if !ok {
		t.Fatal("c must be registered")
	}
	if lang.ID != "c" {
		t.Errorf("ID: want c, got %q", lang.ID)
	}
	if lang.Build == nil {
		t.Fatal("c must have a build config")
	}
	if lang.Build.Cmd != "/usr/bin/gcc" {
		t.Errorf("Build.Cmd: want /usr/bin/gcc, got %q", lang.Build.Cmd)
	}
	if lang.SourceFilename != "solution.c" {
		t.Errorf("SourceFilename: want solution.c, got %q", lang.SourceFilename)
	}
	if lang.Build.Limits.WallTimeS != 3 {
		t.Errorf("Build.Limits.WallTimeS: want 3, got %v", lang.Build.Limits.WallTimeS)
	}
	if lang.Run.Limits.WallTimeS != 3 {
		t.Errorf("Run.Limits.WallTimeS: want 3, got %v", lang.Run.Limits.WallTimeS)
	}
	if lang.Run.Limits.MemoryKB != 524288 {
		t.Errorf("Run.Limits.MemoryKB: want 524288, got %v", lang.Run.Limits.MemoryKB)
	}
	if len(lang.Build.FlagAllowlist) == 0 {
		t.Error("c Build.FlagAllowlist must not be empty")
	}
}

func TestLookupBash(t *testing.T) {
	lang, ok := Lookup("bash")
	if !ok {
		t.Fatal("bash must be registered")
	}
	if lang.ID != "bash" {
		t.Errorf("ID: want bash, got %q", lang.ID)
	}
	if lang.Build != nil {
		t.Error("bash must not have a build config")
	}
	if lang.Run.Cmd != "/usr/bin/bash" {
		t.Errorf("Run.Cmd: want /usr/bin/bash, got %q", lang.Run.Cmd)
	}
	if lang.SourceFilename != "solution.sh" {
		t.Errorf("SourceFilename: want solution.sh, got %q", lang.SourceFilename)
	}
	if lang.Run.Limits.WallTimeS != 5 {
		t.Errorf("Run.Limits.WallTimeS: want 5, got %v", lang.Run.Limits.WallTimeS)
	}
	if lang.Run.Limits.MemoryKB != 65536 {
		t.Errorf("Run.Limits.MemoryKB: want 65536, got %v", lang.Run.Limits.MemoryKB)
	}
	if lang.Run.Limits.MaxProcesses != 10 {
		t.Errorf("Run.Limits.MaxProcesses: want 10, got %v", lang.Run.Limits.MaxProcesses)
	}
}

func TestLookupJS(t *testing.T) {
	lang, ok := Lookup("js")
	if !ok {
		t.Fatal("js must be registered")
	}
	if lang.ID != "js" {
		t.Errorf("ID: want js, got %q", lang.ID)
	}
	if lang.Build != nil {
		t.Error("js must not have a build config")
	}
	if lang.Run.Cmd != "/usr/bin/node" {
		t.Errorf("Run.Cmd: want /usr/bin/node, got %q", lang.Run.Cmd)
	}
	if lang.SourceFilename != "solution.js" {
		t.Errorf("SourceFilename: want solution.js, got %q", lang.SourceFilename)
	}
	if lang.Run.Limits.WallTimeS != 5 {
		t.Errorf("Run.Limits.WallTimeS: want 5, got %v", lang.Run.Limits.WallTimeS)
	}
	if lang.Run.Limits.MemoryKB != 262144 {
		t.Errorf("Run.Limits.MemoryKB: want 262144, got %v", lang.Run.Limits.MemoryKB)
	}
	if lang.Run.Limits.MaxProcesses != 50 {
		t.Errorf("Run.Limits.MaxProcesses: want 50, got %v", lang.Run.Limits.MaxProcesses)
	}
}

func TestLookupJava(t *testing.T) {
	lang, ok := Lookup("java")
	if !ok {
		t.Fatal("java must be registered")
	}
	if lang.ID != "java" {
		t.Errorf("ID: want java, got %q", lang.ID)
	}
	if lang.Build == nil {
		t.Fatal("java must have a build config")
	}
	if lang.SourceFilename != "" {
		t.Errorf("SourceFilename: want empty for from_request strategy, got %q", lang.SourceFilename)
	}
	if lang.SourceFilenameStrategy != "from_request" {
		t.Errorf("SourceFilenameStrategy: want from_request, got %q", lang.SourceFilenameStrategy)
	}
	if lang.ArtifactFilenameStrategy != "from_request" {
		t.Errorf("ArtifactFilenameStrategy: want from_request, got %q", lang.ArtifactFilenameStrategy)
	}
	if lang.Build.Cmd != "/usr/bin/javac" {
		t.Errorf("Build.Cmd: want /usr/bin/javac, got %q", lang.Build.Cmd)
	}
	if lang.Run.Cmd != "/usr/bin/java" {
		t.Errorf("Run.Cmd: want /usr/bin/java, got %q", lang.Run.Cmd)
	}
	if len(lang.Build.FlagAllowlist) == 0 {
		t.Error("java Build.FlagAllowlist must not be empty")
	}
	if lang.Build.Limits.WallTimeS != 6 {
		t.Errorf("Build.Limits.WallTimeS: want 6, got %v", lang.Build.Limits.WallTimeS)
	}
	if lang.Run.Limits.WallTimeS != 6 {
		t.Errorf("Run.Limits.WallTimeS: want 6, got %v", lang.Run.Limits.WallTimeS)
	}
}

func TestLookupVerilog(t *testing.T) {
	lang, ok := Lookup("verilog")
	if !ok {
		t.Fatal("verilog must be registered")
	}
	if lang.ID != "verilog" {
		t.Errorf("ID: want verilog, got %q", lang.ID)
	}
	if lang.Build == nil {
		t.Fatal("verilog must have a build config")
	}
	if lang.Build.Cmd != "/usr/bin/iverilog" {
		t.Errorf("Build.Cmd: want /usr/bin/iverilog, got %q", lang.Build.Cmd)
	}
	if lang.Run.Cmd != "/usr/bin/vvp" {
		t.Errorf("Run.Cmd: want /usr/bin/vvp, got %q", lang.Run.Cmd)
	}
	if lang.SourceFilename != "solution.v" {
		t.Errorf("SourceFilename: want solution.v, got %q", lang.SourceFilename)
	}
	if lang.Artifact != "solution.vvp" {
		t.Errorf("Artifact: want solution.vvp, got %q", lang.Artifact)
	}
	wantAllowlist := []string{"-g2012", "-Wall", "-Wno-timescale"}
	if len(lang.Build.FlagAllowlist) != len(wantAllowlist) {
		t.Errorf("Build.FlagAllowlist: want %v, got %v", wantAllowlist, lang.Build.FlagAllowlist)
	} else {
		for i, f := range wantAllowlist {
			if lang.Build.FlagAllowlist[i] != f {
				t.Errorf("Build.FlagAllowlist[%d]: want %q, got %q", i, f, lang.Build.FlagAllowlist[i])
			}
		}
	}
	if lang.Build.Limits.WallTimeS != 10 {
		t.Errorf("Build.Limits.WallTimeS: want 10, got %v", lang.Build.Limits.WallTimeS)
	}
	if lang.Build.Limits.MemoryKB != 262144 {
		t.Errorf("Build.Limits.MemoryKB: want 262144, got %v", lang.Build.Limits.MemoryKB)
	}
	if lang.Build.Limits.MaxProcesses != 50 {
		t.Errorf("Build.Limits.MaxProcesses: want 50, got %v", lang.Build.Limits.MaxProcesses)
	}
	if lang.Run.Limits.WallTimeS != 5 {
		t.Errorf("Run.Limits.WallTimeS: want 5, got %v", lang.Run.Limits.WallTimeS)
	}
	if lang.Run.Limits.MemoryKB != 131072 {
		t.Errorf("Run.Limits.MemoryKB: want 131072, got %v", lang.Run.Limits.MemoryKB)
	}
	if lang.Run.Limits.MaxProcesses != 50 {
		t.Errorf("Run.Limits.MaxProcesses: want 50, got %v", lang.Run.Limits.MaxProcesses)
	}
}

func TestLookupUnknown(t *testing.T) {
	_, ok := Lookup("unknown")
	if ok {
		t.Error("unknown language must not be in registry")
	}
}
