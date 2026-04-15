#ifndef RATEX_BRIDGE_H
#define RATEX_BRIDGE_H

#include <stddef.h>

typedef struct {
    unsigned char* data;
    size_t len;
    char* error;
} RatexPngResult;

RatexPngResult ratex_render_png(const char* latex, float font_size, float dpr, int display_mode);
void ratex_free_png(unsigned char* data, size_t len);
void ratex_free_string(char* s);

#endif
