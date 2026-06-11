package language

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type yamlLimits struct {
	WallTimeS    int `yaml:"wall_time_s"`
	MemoryKB     int `yaml:"memory_kb"`
	MaxProcesses int `yaml:"max_processes"`
}

type yamlBuildConfig struct {
	Cmd           string     `yaml:"cmd"`
	Args          []string   `yaml:"args"`
	Limits        yamlLimits `yaml:"limits"`
	FlagAllowlist []string   `yaml:"flag_allowlist"`
}

type yamlRunConfig struct {
	Cmd    string     `yaml:"cmd"`
	Args   []string   `yaml:"args"`
	Limits yamlLimits `yaml:"limits"`
}

type yamlLanguage struct {
	ID                       string           `yaml:"id"`
	Name                     string           `yaml:"name"`
	SourceFilename           string           `yaml:"source_filename"`
	Artifact                 string           `yaml:"artifact"`
	VersionArgs              []string         `yaml:"version_args"`
	SourceFilenameStrategy   string           `yaml:"source_filename_strategy"`
	ArtifactFilenameStrategy string           `yaml:"artifact_filename_strategy"`
	Build                    *yamlBuildConfig `yaml:"build"`
	Run                      yamlRunConfig    `yaml:"run"`
}

type yamlFile struct {
	Languages []yamlLanguage `yaml:"languages"`
}

// LoadRegistry reads path, parses it as the language YAML registry, and
// populates the in-memory registry. Returns a descriptive error if the file
// is missing, unreadable, invalid YAML, or contains no languages.
func LoadRegistry(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("language registry: cannot read %s: %w", path, err)
	}

	var f yamlFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("language registry: invalid YAML in %s: %w", path, err)
	}

	if len(f.Languages) == 0 {
		return fmt.Errorf("language registry: %s defines no languages", path)
	}

	for i, yl := range f.Languages {
		if yl.ID == "" {
			return fmt.Errorf("language registry: entry %d in %s is missing its id", i, path)
		}
		lang := &Language{
			ID:                       yl.ID,
			Name:                     yl.Name,
			SourceFilename:           yl.SourceFilename,
			Artifact:                 yl.Artifact,
			VersionArgs:              yl.VersionArgs,
			SourceFilenameStrategy:   yl.SourceFilenameStrategy,
			ArtifactFilenameStrategy: yl.ArtifactFilenameStrategy,
			Run: RunConfig{
				Cmd:  yl.Run.Cmd,
				Args: yl.Run.Args,
				Limits: Limits{
					WallTimeS:    yl.Run.Limits.WallTimeS,
					MemoryKB:     yl.Run.Limits.MemoryKB,
					MaxProcesses: yl.Run.Limits.MaxProcesses,
				},
			},
		}
		if yl.Build != nil {
			lang.Build = &BuildConfig{
				Cmd:           yl.Build.Cmd,
				Args:          yl.Build.Args,
				FlagAllowlist: yl.Build.FlagAllowlist,
				Limits: Limits{
					WallTimeS:    yl.Build.Limits.WallTimeS,
					MemoryKB:     yl.Build.Limits.MemoryKB,
					MaxProcesses: yl.Build.Limits.MaxProcesses,
				},
			}
		}
		Register(lang)
	}

	return nil
}
