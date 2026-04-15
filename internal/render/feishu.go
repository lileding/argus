package render

import (
	"encoding/json"
	"regexp"
	"strings"
)

var imgMarkerRe = regexp.MustCompile(`\[\[IMG:([^\]]+)\]\]`)

// ForFeishu converts model output to Feishu message format.
// Returns (msgType, contentJSON).
//
// Plain text (no markdown) → ("text", `{"text":"..."}`)
// Markdown or LaTeX images → ("interactive", card JSON with markdown element)
//
// Using interactive cards instead of post format because:
// - Card markdown supports inline images via ![text](image_key)
// - Post format requires img elements to be block-level (separate paragraph)
func ForFeishu(markdown string) (string, string) {
	if !hasMarkdown(markdown) && !imgMarkerRe.MatchString(markdown) {
		content, _ := json.Marshal(map[string]string{"text": markdown})
		return "text", string(content)
	}

	return "interactive", markdownToCard(markdown)
}

var mdIndicators = []string{"**", "```", "## ", "# ", "- ", "* ", "[", "![", "$$"}

func hasMarkdown(s string) bool {
	for _, ind := range mdIndicators {
		if strings.Contains(s, ind) {
			return true
		}
	}
	return false
}

// markdownToCard converts markdown to a Feishu interactive card JSON.
// The card uses a single markdown element which natively supports:
// - Bold, italic, strikethrough
// - Links
// - Code blocks
// - Lists
// - Inline images via ![text](image_key)
func markdownToCard(md string) string {
	// Convert [[IMG:key]] markers to Feishu markdown image syntax ![](key)
	content := imgMarkerRe.ReplaceAllString(md, "![]($1)")

	// Escape special characters that conflict with Feishu markdown.
	// (Feishu markdown is close to standard markdown, minimal escaping needed)

	card := map[string]interface{}{
		"schema": "2.0",
		"body": map[string]interface{}{
			"elements": []interface{}{
				map[string]interface{}{
					"tag":     "markdown",
					"content": content,
				},
			},
		},
	}

	data, _ := json.Marshal(card)
	return string(data)
}
