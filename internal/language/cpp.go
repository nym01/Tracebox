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
				WallTimeSec: 30,
				MemoryMB:    512,
				MaxProcs:    4,
			},
		},
		Run: RunConfig{
			Cmd:  "./{{artifact}}",
			Args: []string{},
			Limits: Limits{
				WallTimeSec: 10,
				MemoryMB:    256,
				MaxProcs:    32,
			},
		},
	})
}
