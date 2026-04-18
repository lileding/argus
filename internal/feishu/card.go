package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var imgMarkerRe = regexp.MustCompile(`\[\[IMG:([^\]]+)\]\]`)

// MarkdownToCard builds a Feishu interactive card from markdown content.
// Code blocks are split into separate markdown elements to work around a
// Feishu client bug where code blocks in a single markdown element are
// collapsed to ~5 lines with a broken expand button (especially after
// multiple PATCH updates via update_multi).
func MarkdownToCard(md string) string {
	content := imgMarkerRe.ReplaceAllString(md, "![]($1)")
	segments := splitAtCodeBlocks(content)
	if len(segments) <= 1 {
		return buildCard(content)
	}
	return buildCardMulti(segments)
}

// ThinkingCard builds a "thinking" status card in the appropriate language.
func ThinkingCard(lang string) string {
	text := "💭 Thinking..."
	if lang == "zh" {
		text = "💭 正在思考..."
	}
	return buildCard(text)
}

// ComposingCard builds a "composing answer" status card.
func ComposingCard(lang string) string {
	text := "✍️ Composing answer..."
	if lang == "zh" {
		text = "✍️ 正在撰写回复..."
	}
	return buildCard(text)
}

// ToolStatusCard builds a human-readable status card for a tool call.
// Parses the tool arguments to show specific, meaningful status text.
func ToolStatusCard(toolName, argsJSON, lang string) string {
	return buildCard(humanizeToolCall(toolName, argsJSON, lang))
}

// humanizeToolCall produces a human-readable description of an in-flight tool call.
func humanizeToolCall(toolName, argsJSON, lang string) string {
	args := parseArgs(argsJSON)
	zh := lang == "zh"

	switch toolName {
	case "search":
		q := truncate(args["query"], 60)
		if zh {
			return fmt.Sprintf("🔍 正在搜索: **%s**", q)
		}
		return fmt.Sprintf("🔍 Searching: **%s**", q)
	case "fetch":
		u := truncate(args["url"], 80)
		if zh {
			return fmt.Sprintf("🌐 正在读取网页: %s", u)
		}
		return fmt.Sprintf("🌐 Fetching: %s", u)
	case "read_file":
		p := args["path"]
		if zh {
			return fmt.Sprintf("📖 正在读取文件: `%s`", p)
		}
		return fmt.Sprintf("📖 Reading: `%s`", p)
	case "write_file":
		p := args["path"]
		if zh {
			return fmt.Sprintf("✍️ 正在写入文件: `%s`", p)
		}
		return fmt.Sprintf("✍️ Writing: `%s`", p)
	case "cli":
		cmd := truncate(args["command"], 60)
		if zh {
			return fmt.Sprintf("⚙️ 正在执行: `%s`", cmd)
		}
		return fmt.Sprintf("⚙️ Running: `%s`", cmd)
	case "current_time":
		if zh {
			return "🕐 获取当前时间"
		}
		return "🕐 Getting current time"
	case "save_skill":
		n := args["name"]
		if zh {
			return fmt.Sprintf("💾 正在保存技能: **%s**", n)
		}
		return fmt.Sprintf("💾 Saving skill: **%s**", n)
	case "activate_skill":
		n := args["name"]
		if zh {
			return fmt.Sprintf("🎯 加载技能: **%s**", n)
		}
		return fmt.Sprintf("🎯 Loading skill: **%s**", n)
	case "remember":
		c := truncate(args["content"], 50)
		if zh {
			return fmt.Sprintf("🧠 记住: %s", c)
		}
		return fmt.Sprintf("🧠 Remembering: %s", c)
	case "forget":
		return fmt.Sprintf("🗑️ forget id=%s", args["id"])
	case "db":
		sql := truncate(args["sql"], 60)
		if zh {
			return fmt.Sprintf("🔎 查询数据库: `%s`", sql)
		}
		return fmt.Sprintf("🔎 DB query: `%s`", sql)
	case "db_exec":
		sql := truncate(args["sql"], 60)
		if zh {
			return fmt.Sprintf("💾 写入数据库: `%s`", sql)
		}
		return fmt.Sprintf("💾 DB write: `%s`", sql)
	case "finish_task":
		if zh {
			return "✅ 整理回复中..."
		}
		return "✅ Composing answer..."
	default:
		if zh {
			return fmt.Sprintf("🔧 使用 **%s**", toolName)
		}
		return fmt.Sprintf("🔧 Using **%s**", toolName)
	}
}

func parseArgs(argsJSON string) map[string]string {
	raw := map[string]any{}
	json.Unmarshal([]byte(argsJSON), &raw)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// splitAtCodeBlocks splits markdown at triple-backtick code fence
// boundaries, returning alternating text/code segments. Each segment
// becomes a separate card element so Feishu's client renders code
// blocks independently (avoiding the 5-line collapse bug).
func splitAtCodeBlocks(md string) []string {
	var segments []string
	rest := md
	for {
		// Find opening ```
		idx := strings.Index(rest, "```")
		if idx < 0 {
			break
		}
		// Text before the code block.
		before := strings.TrimSpace(rest[:idx])
		if before != "" {
			segments = append(segments, before)
		}
		// Find closing ``` (after the opening line).
		afterOpen := rest[idx+3:]
		// Skip to end of opening line (language tag).
		nlIdx := strings.Index(afterOpen, "\n")
		if nlIdx < 0 {
			// Malformed: no newline after opening ```. Treat rest as text.
			break
		}
		closeIdx := strings.Index(afterOpen[nlIdx+1:], "```")
		if closeIdx < 0 {
			// Unclosed code block — include everything remaining.
			segments = append(segments, rest[idx:])
			rest = ""
			break
		}
		// closeIdx is relative to afterOpen[nlIdx+1:].
		endPos := idx + 3 + nlIdx + 1 + closeIdx + 3
		segments = append(segments, rest[idx:endPos])
		rest = rest[endPos:]
	}
	trailing := strings.TrimSpace(rest)
	if trailing != "" {
		segments = append(segments, trailing)
	}
	return segments
}

func buildCardMulti(segments []string) string {
	elements := make([]any, len(segments))
	for i, seg := range segments {
		elements[i] = map[string]any{
			"tag":     "markdown",
			"content": seg,
		}
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
		},
		"body": map[string]any{
			"elements": elements,
		},
	}
	data, _ := json.Marshal(card)
	return string(data)
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
