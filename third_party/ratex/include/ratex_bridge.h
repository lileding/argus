#ifndef RATEX_BRIDGE_H
#define RATEX_BRIDGE_H

#include <stddef.h>

typedef struct {
    unsigned char* data;  /* PNG bytes on success, NULL on error */
    size_t len;
    char* error;          /* error message on failure, NULL on success */
} RatexPngResult;

/*
 * Render LaTeX to PNG with a target pixel height.
 * The font size is computed internally so the output image height matches target_height_px.
 * display_mode: 0 = inline, 1 = display (block).
 */
RatexPngResult ratex_render_png(const char* latex, float target_height_px, int display_mode);
void ratex_free_png(unsigned char* data, size_t len);
void ratex_free_string(char* s);

#endif
