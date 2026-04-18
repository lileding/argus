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
	// Expect: "Hello" (markdown), "python:" (markdown label), code (div+plain_text), "Done" (markdown)
	if len(elements) < 3 {
		t.Fatalf("expected at least 3 elements, got %d", len(elements))
	}

	// First element should be "Hello"
	first := elements[0].(map[string]any)["content"].(string)
	if !strings.Contains(first, "Hello") {
		t.Errorf("first element should contain Hello, got: %s", first)
	}

	// There should be a div element with plain_text containing the code
	foundDiv := false
	for _, el := range elements {
		e := el.(map[string]any)
		if e["tag"] == "div" {
			text := e["text"].(map[string]any)
			if text["tag"] == "plain_text" && strings.Contains(text["content"].(string), "print") {
				foundDiv = true
			}
		}
	}
	if !foundDiv {
		t.Errorf("expected a div+plain_text element with code content\ncard: %s", card)
	}

	// Code should NOT appear as markdown triple-backtick (it's in plain_text now)
	if strings.Contains(card, "```python") {
		t.Error("code block should be converted to plain_text, not left as markdown fenced block")
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
