package render

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/ratex/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/ratex/target/release -lratex_bridge -lm -ldl -framework Security -framework CoreFoundation
#include "ratex_bridge.h"
#include <stdlib.h>
*/
import "C"
import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"unsafe"
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

// RenderLatexPNG renders a LaTeX math expression to PNG bytes using RaTeX (Rust, via CGo).
// 99.5% KaTeX syntax coverage.
func RenderLatexPNG(latex string, fontSize float64, displayMode bool) ([]byte, error) {
	if fontSize == 0 {
		fontSize = 16
	}

	cLatex := C.CString(latex)
	defer C.free(unsafe.Pointer(cLatex))

	cDisplay := C.int(0)
	if displayMode {
		cDisplay = 1
	}

	result := C.ratex_render_png(cLatex, C.float(fontSize), cDisplay)

	if result.error != nil {
		errMsg := C.GoString(result.error)
		C.ratex_free_string(result.error)
		return nil, fmt.Errorf("ratex: %s", errMsg)
	}

	if result.data == nil || result.len == 0 {
		return nil, fmt.Errorf("ratex: empty result")
	}

	pngData := C.GoBytes(unsafe.Pointer(result.data), C.int(result.len))
	C.ratex_free_png(result.data, result.len)

	// Post-process: make white background transparent.
	return makeTransparent(pngData)
}

// makeTransparent converts white pixels to transparent in a PNG.
func makeTransparent(pngData []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return pngData, nil // fallback to original
	}

	bounds := img.Bounds()
	rgba := image.NewNRGBA(bounds)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			// If pixel is white or near-white, make transparent.
			if r > 0xF000 && g > 0xF000 && b > 0xF000 {
				rgba.Set(x, y, color.NRGBA{0, 0, 0, 0})
			} else {
				rgba.Set(x, y, img.At(x, y))
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return pngData, nil
	}
	return buf.Bytes(), nil
}
