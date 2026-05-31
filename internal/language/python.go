package language

func init() {
	Register(&Language{
		ID:             "py3",
		Name:           "Python 3",
		SourceFilename: "solution.py",
		Run: RunConfig{
			Cmd:  "python3",
			Args: []string{"{{source}}"},
			Limits: Limits{
				WallTimeS:    10,
				MemoryKB:     131072,
				MaxProcesses: 32,
			},
		},
	})
}
