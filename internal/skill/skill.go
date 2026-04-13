package skill

// Skill is a pluggable capability that injects system prompt fragments
// and specifies which tools it needs. The agent assembles these at runtime.
type Skill struct {
	Name         string   // unique identifier
	Description  string   // human-readable description
	SystemPrompt string   // injected into the system prompt
	Tools        []string // tool names this skill requires
}

// Registry holds all registered skills.
type Registry struct {
	skills map[string]*Skill
}

func NewRegistry() *Registry {
	return &Registry{skills: make(map[string]*Skill)}
}

func (r *Registry) Register(s *Skill) {
	r.skills[s.Name] = s
}

func (r *Registry) Get(name string) (*Skill, bool) {
	s, ok := r.skills[name]
	return s, ok
}

func (r *Registry) All() []*Skill {
	result := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		result = append(result, s)
	}
	return result
}

// CombinedSystemPrompt returns a combined system prompt from all skills.
func (r *Registry) CombinedSystemPrompt() string {
	var prompt string
	for _, s := range r.skills {
		if s.SystemPrompt != "" {
			prompt += "\n\n---\n" + s.SystemPrompt
		}
	}
	return prompt
}
