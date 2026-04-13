package skill

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// SkillEntry represents a parsed SKILL.md file.
type SkillEntry struct {
	Name                   string
	Description            string
	Tools                  []string
	DisableModelInvocation bool
	Builtin                bool      // compiled into binary, always injected into system prompt
	Prompt                 string    // markdown body (after frontmatter)
	FilePath               string    // absolute path to SKILL.md
	ModTime                time.Time // for change detection
}

// SkillIndex is a thread-safe in-memory index of all loaded skills.
type SkillIndex struct {
	mu      sync.RWMutex
	entries map[string]*SkillEntry
}

func NewSkillIndex() *SkillIndex {
	return &SkillIndex{entries: make(map[string]*SkillEntry)}
}

func (idx *SkillIndex) Put(entry *SkillEntry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.entries[entry.Name] = entry
}

func (idx *SkillIndex) Get(name string) (*SkillEntry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	e, ok := idx.entries[name]
	return e, ok
}

func (idx *SkillIndex) Remove(name string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	delete(idx.entries, name)
}

// All returns a snapshot of all entries.
func (idx *SkillIndex) All() []*SkillEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	result := make([]*SkillEntry, 0, len(idx.entries))
	for _, e := range idx.entries {
		result = append(result, e)
	}
	return result
}

// BuiltinPrompts returns the full prompts of all builtin skills, to be injected directly
// into the system prompt (not behind activate_skill).
func (idx *SkillIndex) BuiltinPrompts() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var sb strings.Builder
	for _, e := range idx.entries {
		if e.Builtin && e.Prompt != "" {
			sb.WriteString("\n")
			sb.WriteString(e.Prompt)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// Catalog returns a compact listing of all model-invocable skills for the system prompt.
// The LLM uses this to understand what skills are available and decide when to activate them.
func (idx *SkillIndex) Catalog() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.entries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	for _, e := range idx.entries {
		if e.DisableModelInvocation {
			continue
		}
		desc := e.Description
		if len(desc) > 250 {
			desc = desc[:250] + "..."
		}
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", e.Name, desc))
	}
	sb.WriteString("\nWhen the user's request relates to a skill, use the activate_skill tool to load its full instructions.\n")
	return sb.String()
}
