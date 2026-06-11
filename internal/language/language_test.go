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

func TestLookupUnknown(t *testing.T) {
	_, ok := Lookup("unknown")
	if ok {
		t.Error("unknown language must not be in registry")
	}
}
