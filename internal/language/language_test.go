package language

import "testing"

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

func TestLookupUnknown(t *testing.T) {
	_, ok := Lookup("unknown")
	if ok {
		t.Error("unknown language must not be in registry")
	}
}
