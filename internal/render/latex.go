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

// RenderLatexPNG renders a display LaTeX formula to a high-res PNG.
// Uses fixed font_size=20pt with 2.5x DPI for retina-quality output.
func RenderLatexPNG(latex string, displayMode bool) ([]byte, error) {
	cLatex := C.CString(latex)
	defer C.free(unsafe.Pointer(cLatex))

	cDisplay := C.int(0)
	if displayMode {
		cDisplay = 1
	}

	// font_size=20pt, dpr=2.5 → effective 50px per em, crisp on retina.
	result := C.ratex_render_png(cLatex, C.float(20), C.float(2.5), cDisplay)

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
func whiteToTransparent(pngData []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return pngData, nil
	}

	bounds := img.Bounds()
	rgba := image.NewNRGBA(bounds)

	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X, bounds.Min.Y

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			lum := uint8((r*299 + g*587 + b*114) / 1000 >> 8)
			alpha := 255 - lum
			if alpha < 4 {
				rgba.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 0})
			} else {
				rgba.SetNRGBA(x, y, color.NRGBA{0, 0, 0, alpha})
				if x < minX { minX = x }
				if y < minY { minY = y }
				if x > maxX { maxX = x }
				if y > maxY { maxY = y }
			}
		}
	}

	if minX > maxX || minY > maxY {
		return pngData, nil
	}

	// Tight crop to content.
	cropped := rgba.SubImage(image.Rect(minX, minY, maxX+1, maxY+1))

	var buf bytes.Buffer
	if err := png.Encode(&buf, cropped); err != nil {
		return pngData, nil
	}
	return buf.Bytes(), nil
}
