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
	if msgType != "post" {
		t.Errorf("expected post, got %s", msgType)
	}

	// Verify it's valid JSON.
	var post postContent
	if err := json.Unmarshal([]byte(content), &post); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	body := post["zh_cn"]
	if body.Title != "Title" {
		t.Errorf("title = %q, want 'Title'", body.Title)
	}
	if len(body.Content) < 3 {
		t.Fatalf("expected at least 3 content lines, got %d", len(body.Content))
	}
}

func TestForFeishu_CodeBlock(t *testing.T) {
	md := "Here is code:\n\n```\nfmt.Println(\"hello\")\n```\n\nDone."
	msgType, content := ForFeishu(md)
	if msgType != "post" {
		t.Errorf("expected post, got %s", msgType)
	}

	var post postContent
	json.Unmarshal([]byte(content), &post)
	body := post["zh_cn"]

	// Should have code block as a text element somewhere.
	found := false
	for _, line := range body.Content {
		for _, elem := range line {
			if elem.Text == "fmt.Println(\"hello\")" {
				found = true
			}
		}
	}
	if !found {
		t.Error("code block content not found in post")
	}
}
