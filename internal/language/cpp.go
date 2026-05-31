package language

func init() {
	Register(&Language{
		ID:             "cpp",
		Name:           "C++",
		SourceFilename: "solution.cpp",
		Build: &BuildConfig{
			Cmd:  "g++",
			Args: []string{"-o", "{{artifact}}", "{{source}}"},
			Limits: Limits{
				WallTimeS:    30,
				MemoryKB:     524288,
				MaxProcesses: 4,
			},
		},
		Run: RunConfig{
			Cmd:  "./{{artifact}}",
			Args: []string{},
			Limits: Limits{
				WallTimeS:    10,
				MemoryKB:     262144,
				MaxProcesses: 32,
			},
		},
	})
}
