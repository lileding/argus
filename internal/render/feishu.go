package render

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ForFeishu converts a markdown string into Feishu message format.
// Returns (msgType, contentJSON).
// Plain text → ("text", `{"text":"..."}`)
// Markdown  → ("post", `{"post":{"zh_cn":{"title":"...","content":[...]}}}`)
func ForFeishu(markdown string) (string, string) {
	if !hasMarkdown(markdown) {
		content, _ := json.Marshal(map[string]string{"text": markdown})
		return "text", string(content)
	}
	return "post", markdownToPost(markdown)
}

var mdIndicators = []string{"**", "```", "## ", "# ", "- ", "* ", "[", "!["}

func hasMarkdown(s string) bool {
	for _, ind := range mdIndicators {
		if strings.Contains(s, ind) {
			return true
		}
	}
	return false
}

// Feishu post types.
type postContent struct {
	Post map[string]postBody `json:"post"`
}

type postBody struct {
	Title   string      `json:"title"`
	Content [][]element `json:"content"`
}

type element struct {
	Tag   string   `json:"tag"`
	Text  string   `json:"text,omitempty"`
	Href  string   `json:"href,omitempty"`
	Style []string `json:"style,omitempty"`
}

var (
	linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	boldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

func markdownToPost(md string) string {
	var title string
	var content [][]element

	// Split into blocks by code fences and regular lines.
	lines := strings.Split(md, "\n")
	inCodeBlock := false
	var codeLines []string

	for _, line := range lines {
		// Code block toggle.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inCodeBlock {
				// End code block — emit as single text element.
				code := strings.Join(codeLines, "\n")
				content = append(content, []element{{Tag: "text", Text: code}})
				codeLines = nil
			}
			inCodeBlock = !inCodeBlock
			continue
		}

		if inCodeBlock {
			codeLines = append(codeLines, line)
			continue
		}

		// Skip empty lines.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Heading → title (first h1) or bold line.
		if strings.HasPrefix(trimmed, "# ") {
			heading := strings.TrimPrefix(trimmed, "# ")
			if title == "" {
				title = heading
			} else {
				content = append(content, []element{{Tag: "text", Text: heading, Style: []string{"bold"}}})
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
			heading := strings.TrimLeft(trimmed, "# ")
			content = append(content, []element{{Tag: "text", Text: heading, Style: []string{"bold"}}})
			continue
		}

		// List items — treat as regular lines with bullet.
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			trimmed = "• " + trimmed[2:]
		}

		// Parse inline elements.
		content = append(content, parseInline(trimmed))
	}

	// Flush unclosed code block.
	if inCodeBlock && len(codeLines) > 0 {
		code := strings.Join(codeLines, "\n")
		content = append(content, []element{{Tag: "text", Text: code}})
	}

	post := postContent{
		Post: map[string]postBody{
			"zh_cn": {
				Title:   title,
				Content: content,
			},
		},
	}

	data, _ := json.Marshal(post)
	return string(data)
}

// parseInline converts a line with **bold** and [links](url) into Feishu elements.
func parseInline(line string) []element {
	var elems []element
	remaining := line

	for len(remaining) > 0 {
		// Find the earliest match of bold or link.
		boldLoc := boldRe.FindStringIndex(remaining)
		linkLoc := linkRe.FindStringIndex(remaining)

		// No more matches.
		if boldLoc == nil && linkLoc == nil {
			if remaining != "" {
				elems = append(elems, element{Tag: "text", Text: remaining})
			}
			break
		}

		// Pick the earliest match.
		var loc []int
		isBold := false
		if boldLoc != nil && (linkLoc == nil || boldLoc[0] <= linkLoc[0]) {
			loc = boldLoc
			isBold = true
		} else {
			loc = linkLoc
		}

		// Text before the match.
		if loc[0] > 0 {
			elems = append(elems, element{Tag: "text", Text: remaining[:loc[0]]})
		}

		matched := remaining[loc[0]:loc[1]]
		if isBold {
			sub := boldRe.FindStringSubmatch(matched)
			elems = append(elems, element{Tag: "text", Text: sub[1], Style: []string{"bold"}})
		} else {
			sub := linkRe.FindStringSubmatch(matched)
			elems = append(elems, element{Tag: "a", Text: sub[1], Href: sub[2]})
		}

		remaining = remaining[loc[1]:]
	}

	return elems
}
