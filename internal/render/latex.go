package render

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/go-latex/latex/drawtex/drawimg"
	"github.com/go-latex/latex/mtex"
)

var (
	displayLatexRe = regexp.MustCompile(`\$\$(.+?)\$\$`)
	inlineLatexRe  = regexp.MustCompile(`\$([^$\n]+?)\$`)
)

// LatexBlock represents a detected LaTeX expression in text.
type LatexBlock struct {
	Full    string
	Expr    string
	Display bool
}

// DetectLatex finds all LaTeX expressions in the text.
func DetectLatex(text string) []LatexBlock {
	var blocks []LatexBlock

	for _, m := range displayLatexRe.FindAllStringSubmatch(text, -1) {
		blocks = append(blocks, LatexBlock{Full: m[0], Expr: m[1], Display: true})
	}

	cleaned := displayLatexRe.ReplaceAllString(text, "")
	for _, m := range inlineLatexRe.FindAllStringSubmatch(cleaned, -1) {
		blocks = append(blocks, LatexBlock{Full: m[0], Expr: m[1], Display: false})
	}

	return blocks
}

// RenderLatexPNG renders a LaTeX math expression to PNG bytes using pure Go (go-latex).
// Returns nil, error if the expression uses unsupported LaTeX features.
func RenderLatexPNG(latex string, fontSize float64) (pngBytes []byte, err error) {
	// go-latex panics on unsupported AST nodes (e.g. Sup, Sub).
	// Recover gracefully instead of crashing the process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("latex render panic: %v", r)
			pngBytes = nil
		}
	}()

	if fontSize == 0 {
		fontSize = 24
	}

	var buf bytes.Buffer
	renderer := drawimg.NewRenderer(&buf)

	if err := mtex.Render(renderer, latex, fontSize, 150, nil); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
