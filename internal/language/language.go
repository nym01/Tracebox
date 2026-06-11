package language

type Limits struct {
	WallTimeS    int `json:"wall_time_s,omitempty"`
	MemoryKB     int `json:"memory_kb,omitempty"`
	MaxProcesses int `json:"max_processes,omitempty"`
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
