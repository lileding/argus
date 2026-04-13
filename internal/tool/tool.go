package tool

import (
	"context"
	"encoding/json"

	"argus/internal/model"
)

// Tool is the interface that all tools must implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, arguments string) (string, error)
}

// ToModelToolDef converts a Tool to the model's ToolDef format.
func ToModelToolDef(t Tool) model.ToolDef {
	return model.ToolDef{
		Type: "function",
		Function: model.FunctionDefn{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		},
	}
}
