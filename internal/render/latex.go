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

type LatexBlock struct {
	Full    string
	Expr    string
	Display bool
}

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

// RenderLatexPNG renders LaTeX to a tight PNG with transparent background.
// Inline formulas target 30px height (12px × 2.5x retina).
// Display formulas target 50px height (20px × 2.5x retina).
func RenderLatexPNG(latex string, displayMode bool) ([]byte, error) {
	targetPx := float64(30) // inline: 12px at 2.5x
	if displayMode {
		targetPx = 50 // display: 20px at 2.5x
	}

	cLatex := C.CString(latex)
	defer C.free(unsafe.Pointer(cLatex))

	cDisplay := C.int(0)
	if displayMode {
		cDisplay = 1
	}

	result := C.ratex_render_png(cLatex, C.float(targetPx), cDisplay)

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

	return whiteToTransparent(pngData)
}

// whiteToTransparent converts white background to transparent using luminance-to-alpha.
// No cropping needed — RaTeX output with padding=0 and exact font_size is already tight.
func whiteToTransparent(pngData []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return pngData, nil
	}

	bounds := img.Bounds()
	rgba := image.NewNRGBA(bounds)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			lum := uint8((r*299 + g*587 + b*114) / 1000 >> 8)
			alpha := 255 - lum
			if alpha < 4 {
				rgba.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 0})
			} else {
				rgba.SetNRGBA(x, y, color.NRGBA{0, 0, 0, alpha})
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return pngData, nil
	}
	return buf.Bytes(), nil
}
