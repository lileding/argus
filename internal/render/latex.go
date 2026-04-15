package render

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/ratex/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/ratex/target/release -lratex_bridge -lm -ldl -framework Security -framework CoreFoundation
#include "ratex_bridge.h"
#include <stdlib.h>
*/
import "C"
import (
	"fmt"
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

	return pngData, nil
}
