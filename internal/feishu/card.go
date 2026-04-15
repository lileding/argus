package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var imgMarkerRe = regexp.MustCompile(`\[\[IMG:([^\]]+)\]\]`)

// MarkdownToCard builds a Feishu interactive card from markdown content.
// All cards include update_multi for PATCH support.
func MarkdownToCard(md string) string {
	content := imgMarkerRe.ReplaceAllString(md, "![]($1)")
	return buildCard(content)
}

// ThinkingCard builds a "thinking" status card in the appropriate language.
func ThinkingCard(lang string) string {
	text := "Thinking..."
	if lang == "zh" {
		text = "正在思考..."
	}
	return buildCard(text)
}

// ToolStatusCard builds a card showing which tool is being used.
func ToolStatusCard(toolName, lang string) string {
	var text string
	if lang == "zh" {
		text = fmt.Sprintf("正在使用 **%s** ...", toolName)
	} else {
		text = fmt.Sprintf("Using **%s** ...", toolName)
	}
	return buildCard(text)
}

func buildCard(markdownContent string) string {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
		},
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag":     "markdown",
					"content": markdownContent,
				},
			},
		},
	}
	data, _ := json.Marshal(card)
	return string(data)
}
