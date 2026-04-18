package feishu

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarkdownToCard(t *testing.T) {
	card := MarkdownToCard("Hello **world**")
	var m map[string]any
	json.Unmarshal([]byte(card), &m)

	if m["schema"] != "2.0" {
		t.Errorf("expected schema 2.0")
	}
	cfg := m["config"].(map[string]any)
	if cfg["update_multi"] != true {
		t.Error("expected update_multi=true")
	}
}

func TestImageMarkerConversion(t *testing.T) {
	card := MarkdownToCard("Formula [[IMG:img_key_123]] here")
	if !strings.Contains(card, "![](img_key_123)") {
		t.Error("[[IMG:key]] not converted to ![](key)")
	}
}

func TestThinkingCard(t *testing.T) {
	zh := ThinkingCard("zh")
	if !strings.Contains(zh, "正在思考") {
		t.Error("expected Chinese thinking text")
	}
	en := ThinkingCard("en")
	if !strings.Contains(en, "Thinking") {
		t.Error("expected English thinking text")
	}
}

func TestToolStatusCard_Search(t *testing.T) {
	card := ToolStatusCard("search", `{"query":"普罗科菲耶夫"}`, "zh")
	if !strings.Contains(card, "正在搜索") || !strings.Contains(card, "普罗科菲耶夫") {
		t.Errorf("expected Chinese search status with query, got: %s", card)
	}
	card = ToolStatusCard("search", `{"query":"Prokofiev"}`, "en")
	if !strings.Contains(card, "Searching") || !strings.Contains(card, "Prokofiev") {
		t.Errorf("expected English search status with query, got: %s", card)
	}
}

func TestToolStatusCard_ReadFile(t *testing.T) {
	card := ToolStatusCard("read_file", `{"path":"report.pdf"}`, "zh")
	if !strings.Contains(card, "读取") || !strings.Contains(card, "report.pdf") {
		t.Errorf("expected read file status, got: %s", card)
	}
}

func TestSplitAtCodeBlocks(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantN    int // expected number of segments
		wantCode bool // at least one segment should contain ```
	}{
		{
			name:  "no code blocks",
			input: "just plain text",
			wantN: 1,
		},
		{
			name:     "one code block",
			input:    "before\n\n```python\nprint('hi')\n```\n\nafter",
			wantN:    3, // before, code, after
			wantCode: true,
		},
		{
			name:     "code block only",
			input:    "```go\nfmt.Println()\n```",
			wantN:    1,
			wantCode: true,
		},
		{
			name:     "two code blocks",
			input:    "text1\n```a\nx\n```\ntext2\n```b\ny\n```\ntext3",
			wantN:    5,
			wantCode: true,
		},
		{
			name:  "unclosed code block",
			input: "start\n```python\ncode without close",
			wantN: 2, // "start", rest as code
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs := splitAtCodeBlocks(tc.input)
			if len(segs) != tc.wantN {
				t.Errorf("got %d segments, want %d\nsegments: %v", len(segs), tc.wantN, segs)
			}
			if tc.wantCode {
				hasCode := false
				for _, s := range segs {
					if strings.Contains(s, "```") {
						hasCode = true
					}
				}
				if !hasCode {
					t.Error("expected at least one code segment")
				}
			}
		})
	}
}

func TestMarkdownToCard_CodeBlockSplit(t *testing.T) {
	md := "Hello\n\n```python\nprint('world')\nprint('foo')\nprint('bar')\n```\n\nDone"
	card := MarkdownToCard(md)
	var m map[string]any
	json.Unmarshal([]byte(card), &m)
	body := m["body"].(map[string]any)
	elements := body["elements"].([]any)
	// Expect: "Hello" (markdown), collapsible_panel (code), "Done" (markdown)
	if len(elements) != 3 {
		t.Fatalf("expected 3 elements, got %d\ncard: %s", len(elements), card)
	}

	// First element: markdown with "Hello"
	first := elements[0].(map[string]any)
	if first["tag"] != "markdown" || !strings.Contains(first["content"].(string), "Hello") {
		t.Errorf("first element should be markdown with Hello, got: %v", first)
	}

	// Second element: collapsible_panel with expanded=true
	panel := elements[1].(map[string]any)
	if panel["tag"] != "collapsible_panel" {
		t.Fatalf("second element should be collapsible_panel, got tag=%v", panel["tag"])
	}
	if panel["expanded"] != true {
		t.Error("collapsible_panel should have expanded=true")
	}
	// Panel header should contain language name
	header := panel["header"].(map[string]any)
	title := header["title"].(map[string]any)
	if title["content"] != "python" {
		t.Errorf("panel title should be 'python', got %v", title["content"])
	}
	// Panel elements should contain div+plain_text with code
	panelElements := panel["elements"].([]any)
	div := panelElements[0].(map[string]any)
	text := div["text"].(map[string]any)
	if !strings.Contains(text["content"].(string), "print('world')") {
		t.Errorf("panel content should contain code, got: %v", text["content"])
	}

	// Third element: markdown with "Done"
	last := elements[2].(map[string]any)
	if last["tag"] != "markdown" || !strings.Contains(last["content"].(string), "Done") {
		t.Errorf("third element should be markdown with Done, got: %v", last)
	}

	// Code should NOT appear as markdown triple-backtick
	if strings.Contains(card, "```python") {
		t.Error("code block should be in collapsible_panel, not left as markdown fenced block")
	}
}

func TestDetectLang(t *testing.T) {
	if detectLang("你好世界") != "zh" {
		t.Error("expected zh")
	}
	if detectLang("hello world") != "en" {
		t.Error("expected en")
	}
	if detectLang("混合 mixed") != "zh" {
		t.Error("expected zh for mixed")
	}
}
