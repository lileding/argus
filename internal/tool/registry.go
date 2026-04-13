package tool

import "argus/internal/model"

// Registry holds all registered tools.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) AllToolDefs() []model.ToolDef {
	defs := make([]model.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, ToModelToolDef(t))
	}
	return defs
}

func (r *Registry) Len() int {
	return len(r.tools)
}
