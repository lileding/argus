package skill

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// FileLoader scans a skills directory, parses SKILL.md files, and maintains the index.
// Built-in skills are injected first; user skills with the same name override them.
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

// LoadAll loads built-in skills, then scans the skills directory.
// User skills override built-in skills with the same name.
func (l *FileLoader) LoadAll() error {
	// Ensure skills directory exists.
	os.MkdirAll(l.skillsDir, 0755)

	// Start with built-in skills.
	builtins := make(map[string]bool)
	for _, s := range BuiltinSkills() {
		l.index.Put(s)
		builtins[s.Name] = true
	}

	// Scan user skills from disk.
	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	seen := make(map[string]bool)
	for k := range builtins {
		seen[k] = true
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(l.skillsDir, entry.Name(), "SKILL.md")
		info, err := os.Stat(skillPath)
		if err != nil {
			continue
		}

		// Check if already loaded with same mod time (skip builtins — always reload from disk).
		if !builtins[entry.Name()] {
			existing, ok := l.index.Get(entry.Name())
			if ok && existing.ModTime.Equal(info.ModTime()) {
				seen[entry.Name()] = true
				continue
			}
		}

		s, err := parseSkillFile(skillPath, entry.Name())
		if err != nil {
			slog.Warn("failed to parse skill", "path", skillPath, "err", err)
			continue
		}
		s.ModTime = info.ModTime()

		l.index.Put(s) // user skill overrides builtin
		seen[s.Name] = true
		slog.Info("skill loaded", "name", s.Name, "source", "file")
	}

	// Remove skills that are no longer on disk and not builtin.
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

	var meta frontmatter
	var body string

	if strings.HasPrefix(content, "---") {
		rest := content[3:]
		idx := strings.Index(rest, "\n---")
		if idx >= 0 {
			yamlPart := strings.TrimSpace(rest[:idx])
			body = strings.TrimSpace(rest[idx+4:])

			if err := yaml.Unmarshal([]byte(yamlPart), &meta); err != nil {
				return nil, err
			}
		} else {
			body = content
		}
	} else {
		body = content
	}

	name := meta.Name
	if name == "" {
		name = dirName
	}

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
