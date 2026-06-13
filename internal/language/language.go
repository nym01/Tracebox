package language

type Limits struct {
	WallTimeS    int `json:"wall_time_s,omitempty"`
	MemoryKB     int `json:"memory_kb,omitempty"`
	MaxProcesses int `json:"max_processes,omitempty"`
	// CPUMsPerSec is the cgroup v2 CPU bandwidth limit, in milliseconds of CPU
	// time the whole sandbox may consume per wall-clock second. 1000 == one full
	// core, 2000 == two cores, etc. Zero means "no explicit limit". It is the CPU
	// counterpart to MemoryKB/MaxProcesses: wall_time_s bounds *elapsed* time but
	// not CPU *consumed*, so a request spinning many threads can saturate every
	// host core for its whole wall window; this caps the per-request CPU draw so
	// concurrent requests cannot amplify into a host-wide CPU-exhaustion DoS.
	CPUMsPerSec int `json:"cpu_ms_per_sec,omitempty"`
}

type BuildConfig struct {
	Cmd           string   `json:"cmd,omitempty"`
	Args          []string `json:"args,omitempty"`
	Limits        Limits   `json:"limits,omitempty"`
	FlagAllowlist []string `json:"flag_allowlist,omitempty"`
}

type RunConfig struct {
	Cmd    string   `json:"cmd,omitempty"`
	Args   []string `json:"args,omitempty"`
	Limits Limits   `json:"limits,omitempty"`
}

type Language struct {
	ID                       string       `json:"id"`
	Name                     string       `json:"name"`
	SourceFilename           string       `json:"source_filename,omitempty"`
	Artifact                 string       `json:"artifact,omitempty"`
	VersionArgs              []string     `json:"version_args,omitempty"`
	SourceFilenameStrategy   string       `json:"source_filename_strategy,omitempty"`
	ArtifactFilenameStrategy string       `json:"artifact_filename_strategy,omitempty"`
	Build                    *BuildConfig `json:"build,omitempty"`
	Run                      RunConfig    `json:"run"`
}

var registry = map[string]*Language{}

func Register(lang *Language) {
	registry[lang.ID] = lang
}

func Lookup(id string) (*Language, bool) {
	l, ok := registry[id]
	return l, ok
}

func All() []*Language {
	out := make([]*Language, 0, len(registry))
	for _, l := range registry {
		out = append(out, l)
	}
	return out
}
