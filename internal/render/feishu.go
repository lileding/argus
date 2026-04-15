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
// The content JSON for msg_type "post" is {"zh_cn": {"title": ..., "content": ...}}
// (no outer "post" wrapper — the API msg_type already indicates it).
type postContent map[string]postBody

type postBody struct {
	Title   string      `json:"title"`
	Content [][]element `json:"content"`
}

type element struct {
	Tag      string   `json:"tag"`
	Text     string   `json:"text,omitempty"`
	Href     string   `json:"href,omitempty"`
	ImageKey string   `json:"image_key,omitempty"`
	Style    []string `json:"style,omitempty"`
}

var (
	linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	boldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	imgRe  = regexp.MustCompile(`\[\[IMG:([^\]]+)\]\]`)
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
		"zh_cn": {
			Title:   title,
			Content: content,
		},
	}

	data, _ := json.Marshal(post)
	return string(data)
}

// parseInline converts a line with **bold**, [links](url), and [[IMG:key]] into Feishu elements.
func parseInline(line string) []element {
	var elems []element
	remaining := line

	for len(remaining) > 0 {
		// Find the earliest match of bold, link, or image.
		boldLoc := boldRe.FindStringIndex(remaining)
		linkLoc := linkRe.FindStringIndex(remaining)
		imgLoc := imgRe.FindStringIndex(remaining)

		// No more matches.
		if boldLoc == nil && linkLoc == nil && imgLoc == nil {
			if remaining != "" {
				elems = append(elems, element{Tag: "text", Text: remaining})
			}
			break
		}

		// Pick the earliest match.
		type match struct {
			loc  []int
			kind string
		}
		var best match
		for _, m := range []match{
			{boldLoc, "bold"}, {linkLoc, "link"}, {imgLoc, "img"},
		} {
			if m.loc == nil {
				continue
			}
			if best.loc == nil || m.loc[0] < best.loc[0] {
				best = m
			}
		}

		// Text before the match.
		if best.loc[0] > 0 {
			elems = append(elems, element{Tag: "text", Text: remaining[:best.loc[0]]})
		}

		matched := remaining[best.loc[0]:best.loc[1]]
		switch best.kind {
		case "bold":
			sub := boldRe.FindStringSubmatch(matched)
			elems = append(elems, element{Tag: "text", Text: sub[1], Style: []string{"bold"}})
		case "link":
			sub := linkRe.FindStringSubmatch(matched)
			elems = append(elems, element{Tag: "a", Text: sub[1], Href: sub[2]})
		case "img":
			sub := imgRe.FindStringSubmatch(matched)
			elems = append(elems, element{Tag: "img", ImageKey: sub[1]})
		}

		remaining = remaining[best.loc[1]:]
	}

	return elems
}
