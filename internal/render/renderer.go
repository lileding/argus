package render

import (
	"log/slog"
)

// ImageUploader uploads an image and returns a key for embedding in messages.
type ImageUploader interface {
	UploadImage(imageData []byte) (imageKey string, err error)
}

// Processor handles IM-agnostic markdown processing (e.g. LaTeX → image replacement).
type Processor struct {
	uploader ImageUploader
}

// NewProcessor creates a processor. uploader can be nil (LaTeX rendering disabled).
func NewProcessor(uploader ImageUploader) *Processor {
	return &Processor{uploader: uploader}
}

// ProcessMarkdown processes markdown text: renders display LaTeX to images,
// replaces them with [[IMG:key]] markers. The IM adapter converts these
// markers to the appropriate format (e.g. ![](key) for Feishu).
func (p *Processor) ProcessMarkdown(markdown string) string {
	if p.uploader == nil {
		return markdown
	}
	return p.replaceLatexWithImages(markdown)
}

func (p *Processor) replaceLatexWithImages(text string) string {
	blocks := DetectLatex(text)
	if len(blocks) == 0 {
		return text
	}

	for _, block := range blocks {
		if !block.Display {
			continue
		}

		pngData, err := RenderLatexPNG(block.Expr, block.Display)
		if err != nil {
			slog.Debug("latex render failed", "expr", block.Expr, "err", err)
			continue
		}

		imageKey, err := p.uploader.UploadImage(pngData)
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
