package render

import (
	"encoding/json"
	"regexp"
)

var imgMarkerRe = regexp.MustCompile(`\[\[IMG:([^\]]+)\]\]`)

// ForFeishu converts model output to a Feishu interactive card (always).
// Returns ("interactive", cardJSON).
// All output uses cards for: markdown rendering, inline images, and future
// support for status updates ("thinking...", tool usage, etc.).
func ForFeishu(markdown string) (string, string) {
	return "interactive", markdownToCard(markdown)
}

// markdownToCard wraps content in a Feishu card with a markdown element.
// [[IMG:key]] markers are converted to ![](key) for Feishu inline images.
func markdownToCard(md string) string {
	// Convert [[IMG:key]] → ![](key)
	content := imgMarkerRe.ReplaceAllString(md, "![]($1)")

	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag":     "markdown",
					"content": content,
				},
			},
		},
	}

	data, _ := json.Marshal(card)
	return string(data)
}
