package language

type Limits struct {
	WallTimeSec int `json:"wall_time_sec,omitempty"`
	MemoryMB    int `json:"memory_mb,omitempty"`
	MaxProcs    int `json:"max_procs,omitempty"`
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
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	SourceFilename string       `json:"source_filename,omitempty"`
	Build          *BuildConfig `json:"build,omitempty"`
	Run            RunConfig    `json:"run"`
}

var registry = map[string]*Language{}

func Register(lang *Language) {
	registry[lang.ID] = lang
}

func Lookup(id string) (*Language, bool) {
	l, ok := registry[id]
	return l, ok
}
