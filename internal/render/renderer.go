package render

import (
	"log/slog"
)

// ImageUploader uploads an image and returns a key for embedding in messages.
type ImageUploader interface {
	UploadImage(imageData []byte) (imageKey string, err error)
}

// Renderer converts model output to Feishu format, handling LaTeX rendering.
type Renderer struct {
	uploader ImageUploader
}

// NewRenderer creates a renderer. uploader can be nil (LaTeX upload disabled).
func NewRenderer(uploader ImageUploader) *Renderer {
	return &Renderer{uploader: uploader}
}

// RenderForFeishu converts markdown to Feishu format, rendering LaTeX to images.
func (r *Renderer) RenderForFeishu(markdown string) (msgType string, contentJSON string) {
	if r.uploader != nil {
		markdown = r.replaceLatexWithImages(markdown)
	}

	return ForFeishu(markdown)
}

// replaceLatexWithImages finds LaTeX blocks, renders to PNG, uploads to Feishu.
func (r *Renderer) replaceLatexWithImages(text string) string {
	blocks := DetectLatex(text)
	if len(blocks) == 0 {
		return text
	}

	for _, block := range blocks {
		fontSize := 24.0
		if block.Display {
			fontSize = 28.0
		}

		pngData, err := RenderLatexPNG(block.Expr, fontSize)
		if err != nil {
			slog.Debug("latex render failed", "expr", block.Expr, "err", err)
			continue
		}

		imageKey, err := r.uploader.UploadImage(pngData)
		if err != nil {
			slog.Debug("latex upload failed", "expr", block.Expr, "err", err)
			continue
		}

		replacement := "[[IMG:" + imageKey + "]]"
		text = replaceFirst(text, block.Full, replacement)

		slog.Info("latex rendered", "expr", block.Expr, "image_key", imageKey)
	}

	return text
}

func replaceFirst(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
