package render

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"argus/internal/sandbox"
)

var (
	displayLatexRe = regexp.MustCompile(`\$\$(.+?)\$\$`)
	inlineLatexRe  = regexp.MustCompile(`\$([^$\n]+?)\$`)
)

// LatexBlock represents a detected LaTeX expression in text.
type LatexBlock struct {
	Full    string // original text including delimiters
	Expr    string // LaTeX expression without delimiters
	Display bool   // true for $$ (display), false for $ (inline)
}

// DetectLatex finds all LaTeX expressions in the text.
func DetectLatex(text string) []LatexBlock {
	var blocks []LatexBlock

	// Display math first ($$...$$).
	for _, m := range displayLatexRe.FindAllStringSubmatch(text, -1) {
		blocks = append(blocks, LatexBlock{Full: m[0], Expr: m[1], Display: true})
	}

	// Inline math ($...$), excluding already-matched display blocks.
	cleaned := displayLatexRe.ReplaceAllString(text, "")
	for _, m := range inlineLatexRe.FindAllStringSubmatch(cleaned, -1) {
		blocks = append(blocks, LatexBlock{Full: m[0], Expr: m[1], Display: false})
	}

	return blocks
}

// RenderLatex renders a LaTeX expression to a PNG image using matplotlib via sandbox.
// Returns the PNG bytes, or an error if rendering is unavailable.
func RenderLatex(ctx context.Context, latex string, sb sandbox.Sandbox) ([]byte, error) {
	outPath := "/tmp/argus_latex.png"

	// Escape single quotes for shell.
	escaped := strings.ReplaceAll(latex, "'", "\\'")

	script := fmt.Sprintf(
		`python3 -c "import matplotlib; matplotlib.use('Agg'); import matplotlib.pyplot as plt; fig=plt.figure(figsize=(8,1.5)); fig.text(0.5,0.5,r'$%s$',fontsize=18,ha='center',va='center'); plt.axis('off'); plt.savefig('%s',bbox_inches='tight',dpi=150,transparent=True,pad_inches=0.1)"`,
		escaped, outPath,
	)

	_, err := sb.Exec(ctx, script, "")
	if err != nil {
		return nil, fmt.Errorf("render latex: %w", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read rendered image: %w", err)
	}
	os.Remove(outPath)

	return data, nil
}
