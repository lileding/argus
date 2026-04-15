package render

import (
	"encoding/json"
	"testing"
)

func TestForFeishu_AlwaysCard(t *testing.T) {
	// Even plain text should now be a card.
	msgType, content := ForFeishu("hello world")
	if msgType != "interactive" {
		t.Errorf("expected interactive, got %s", msgType)
	}

	var card map[string]any
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if card["schema"] != "2.0" {
		t.Errorf("expected schema 2.0, got %v", card["schema"])
	}
}

func TestForFeishu_Markdown(t *testing.T) {
	md := "# Title\n\nHello **world**\n\n- item one"
	msgType, _ := ForFeishu(md)
	if msgType != "interactive" {
		t.Errorf("expected interactive, got %s", msgType)
	}
}

func TestForFeishu_ImageMarker(t *testing.T) {
	md := "Formula: [[IMG:img_key_123]] here"
	_, content := ForFeishu(md)

	var card map[string]any
	json.Unmarshal([]byte(content), &card)

	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	elem := elements[0].(map[string]any)
	mdContent := elem["content"].(string)

	if mdContent != "Formula: ![](img_key_123) here" {
		t.Errorf("image marker not converted: %q", mdContent)
	}
}
