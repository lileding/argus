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

	// Post-process: transparent background + tight crop.
	return postProcess(pngData)
}

// postProcess makes white pixels transparent and tight-crops to content bounds.
func postProcess(pngData []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return pngData, nil
	}

	bounds := img.Bounds()
	rgba := image.NewNRGBA(bounds)

	// Pass 1: convert white to transparent, track content bounds.
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X, bounds.Min.Y

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r > 0xF000 && g > 0xF000 && b > 0xF000 {
				rgba.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 0})
			} else {
				rgba.Set(x, y, img.At(x, y))
				// Track bounding box of non-transparent content.
				if x < minX { minX = x }
				if y < minY { minY = y }
				if x > maxX { maxX = x }
				if y > maxY { maxY = y }
			}
		}
	}

	// No content found.
	if minX > maxX || minY > maxY {
		return pngData, nil
	}

	// Pass 2: crop to content bounds with 1px margin.
	margin := 1
	cropRect := image.Rect(
		max(minX-margin, bounds.Min.X),
		max(minY-margin, bounds.Min.Y),
		min(maxX+margin+1, bounds.Max.X),
		min(maxY+margin+1, bounds.Max.Y),
	)

	cropped := rgba.SubImage(cropRect)

	var buf bytes.Buffer
	if err := png.Encode(&buf, cropped); err != nil {
		return pngData, nil
	}
	return buf.Bytes(), nil
}
