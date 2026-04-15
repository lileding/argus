package render

import (
	"encoding/json"
	"testing"
)

func TestForFeishu_PlainText(t *testing.T) {
	msgType, content := ForFeishu("hello world")
	if msgType != "text" {
		t.Errorf("expected text, got %s", msgType)
	}
	var m map[string]string
	json.Unmarshal([]byte(content), &m)
	if m["text"] != "hello world" {
		t.Errorf("expected 'hello world', got %q", m["text"])
	}
}

func TestForFeishu_Markdown(t *testing.T) {
	md := "# Title\n\nHello **world** and [link](https://example.com)\n\n- item one\n- item two"
	msgType, content := ForFeishu(md)
	if msgType != "interactive" {
		t.Errorf("expected interactive, got %s", msgType)
	}

	var card map[string]interface{}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if card["schema"] != "2.0" {
		t.Errorf("expected schema 2.0, got %v", card["schema"])
	}
}

func TestForFeishu_ImageMarker(t *testing.T) {
	md := "Here is a formula [[IMG:img_key_123]] in text"
	msgType, content := ForFeishu(md)
	if msgType != "interactive" {
		t.Errorf("expected interactive, got %s", msgType)
	}

	var card map[string]interface{}
	json.Unmarshal([]byte(content), &card)

	// The [[IMG:key]] should be converted to ![](key) in the markdown content.
	body := card["body"].(map[string]interface{})
	elements := body["elements"].([]interface{})
	elem := elements[0].(map[string]interface{})
	mdContent := elem["content"].(string)

	if mdContent != "Here is a formula ![](img_key_123) in text" {
		t.Errorf("image marker not converted: %q", mdContent)
	}
}
