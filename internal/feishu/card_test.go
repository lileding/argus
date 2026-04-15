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
