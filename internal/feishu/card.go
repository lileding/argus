package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var imgMarkerRe = regexp.MustCompile(`\[\[IMG:([^\]]+)\]\]`)

// MarkdownToCard builds a Feishu interactive card from markdown content.
func MarkdownToCard(md string) string {
	content := imgMarkerRe.ReplaceAllString(md, "![]($1)")
	return buildCard(content)
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
