package skill

import (
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed seeds
var seedFS embed.FS

// FileLoader scans a skills directory, parses SKILL.md files, and maintains the index.
type FileLoader struct {
	skillsDir string
	index     *SkillIndex
	interval  time.Duration
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewFileLoader(skillsDir string, interval time.Duration) *FileLoader {
	return &FileLoader{
		skillsDir: skillsDir,
		index:     NewSkillIndex(),
		interval:  interval,
		stopCh:    make(chan struct{}),
	}
}

func (l *FileLoader) Index() *SkillIndex {
	return l.index
}

// EnsureSeeds copies seed skills to the skills directory if it's empty.
func (l *FileLoader) EnsureSeeds() error {
	if err := os.MkdirAll(l.skillsDir, 0755); err != nil {
		return err
	}

	// Check if skills dir already has content.
	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return nil // already has skills, don't overwrite
	}

	// Copy seeds.
	return fs.WalkDir(seedFS, "seeds", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Strip "seeds/" prefix to get relative path.
		relPath := strings.TrimPrefix(path, "seeds/")
		if relPath == "" || relPath == "seeds" {
			return nil
		}

		targetPath := filepath.Join(l.skillsDir, relPath)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}

		data, err := seedFS.ReadFile(path)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}

		slog.Info("seeding skill", "path", targetPath)
		return os.WriteFile(targetPath, data, 0644)
	})
}

// LoadAll scans the skills directory and populates the index.
func (l *FileLoader) LoadAll() error {
	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	seen := make(map[string]bool)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(l.skillsDir, entry.Name(), "SKILL.md")
		info, err := os.Stat(skillPath)
		if err != nil {
			continue // no SKILL.md in this directory
		}

		// Check if already loaded with same mod time.
		existing, ok := l.index.Get(entry.Name())
		if ok && existing.ModTime.Equal(info.ModTime()) {
			seen[entry.Name()] = true
			continue
		}

		skill, err := parseSkillFile(skillPath, entry.Name())
		if err != nil {
			slog.Warn("failed to parse skill", "path", skillPath, "err", err)
			continue
		}
		skill.ModTime = info.ModTime()

		l.index.Put(skill)
		seen[skill.Name] = true
		slog.Info("skill loaded", "name", skill.Name)
	}

	// Remove skills whose directories were deleted.
	for _, e := range l.index.All() {
		if !seen[e.Name] {
			l.index.Remove(e.Name)
			slog.Info("skill removed", "name", e.Name)
		}
	}

	return nil
}

// Start begins periodic re-scanning in the background.
func (l *FileLoader) Start() {
	l.wg.Add(1)
	go l.run()
	slog.Info("skill loader started", "dir", l.skillsDir, "interval", l.interval)
}

func (l *FileLoader) run() {
	defer l.wg.Done()
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := l.LoadAll(); err != nil {
				slog.Warn("skill rescan failed", "err", err)
			}
		case <-l.stopCh:
			return
		}
	}
}

// Rebuild forces an immediate reload. Called by save_skill tool.
func (l *FileLoader) Rebuild() {
	if err := l.LoadAll(); err != nil {
		slog.Warn("skill rebuild failed", "err", err)
	}
}

func (l *FileLoader) Stop() {
	close(l.stopCh)
	l.wg.Wait()
	slog.Info("skill loader stopped")
}

// frontmatter represents the YAML frontmatter of a SKILL.md file.
type frontmatter struct {
	Name                   string   `yaml:"name"`
	Description            string   `yaml:"description"`
	Tools                  []string `yaml:"tools"`
	DisableModelInvocation bool     `yaml:"disable-model-invocation"`
}

// parseSkillFile reads and parses a SKILL.md file.
func parseSkillFile(path, dirName string) (*SkillEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)

	// Split on "---" delimiters.
	// Expected format: ---\n<yaml>\n---\n<body>
	var meta frontmatter
	var body string

	if strings.HasPrefix(content, "---") {
		// Find the closing "---".
		rest := content[3:] // skip opening "---"
		idx := strings.Index(rest, "\n---")
		if idx >= 0 {
			yamlPart := strings.TrimSpace(rest[:idx])
			body = strings.TrimSpace(rest[idx+4:]) // skip "\n---"

			if err := yaml.Unmarshal([]byte(yamlPart), &meta); err != nil {
				return nil, err
			}
		} else {
			// No closing delimiter, treat entire content as body.
			body = content
		}
	} else {
		body = content
	}

	// Use directory name as fallback for name.
	name := meta.Name
	if name == "" {
		name = dirName
	}

	// Use first paragraph as fallback for description.
	description := meta.Description
	if description == "" && body != "" {
		lines := strings.SplitN(body, "\n\n", 2)
		description = strings.TrimSpace(lines[0])
	}

	return &SkillEntry{
		Name:                   name,
		Description:            description,
		Tools:                  meta.Tools,
		DisableModelInvocation: meta.DisableModelInvocation,
		Prompt:                 body,
		FilePath:               path,
	}, nil
}
