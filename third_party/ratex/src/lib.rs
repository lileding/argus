use std::ffi::{CStr, CString};
use std::os::raw::c_char;
use std::ptr;

use ratex_layout::{layout, to_display_list, LayoutOptions};
use ratex_parser::parser::parse;
use ratex_render::{render_to_png, RenderOptions};
use ratex_types::math_style::MathStyle;

/// Result of rendering LaTeX to PNG.
#[repr(C)]
pub struct RatexPngResult {
    pub data: *mut u8,
    pub len: usize,
    pub error: *mut c_char,
}

/// Render a LaTeX expression to PNG bytes.
///
/// # Safety
/// `latex` must be a valid null-terminated UTF-8 string.
#[no_mangle]
pub unsafe extern "C" fn ratex_render_png(
    latex: *const c_char,
    font_size: f32,
    display_mode: i32,
) -> RatexPngResult {
    let latex_str = match CStr::from_ptr(latex).to_str() {
        Ok(s) => s,
        Err(e) => return error_result(&format!("invalid UTF-8: {}", e)),
    };

    match render_inner(latex_str, font_size, display_mode != 0) {
        Ok(png_data) => {
            let mut boxed = png_data.into_boxed_slice();
            let data = boxed.as_mut_ptr();
            let len = boxed.len();
            std::mem::forget(boxed);
            RatexPngResult {
                data,
                len,
                error: ptr::null_mut(),
            }
        }
        Err(e) => error_result(&e),
    }
}

/// Free PNG data returned by ratex_render_png.
#[no_mangle]
pub unsafe extern "C" fn ratex_free_png(data: *mut u8, len: usize) {
    if !data.is_null() {
        let _ = Vec::from_raw_parts(data, len, len);
    }
}

/// Free a string returned in RatexPngResult.error.
#[no_mangle]
pub unsafe extern "C" fn ratex_free_string(s: *mut c_char) {
    if !s.is_null() {
        let _ = CString::from_raw(s);
    }
}

fn error_result(msg: &str) -> RatexPngResult {
    let error = CString::new(msg).unwrap_or_default();
    RatexPngResult {
        data: ptr::null_mut(),
        len: 0,
        error: error.into_raw(),
    }
}

fn render_inner(latex: &str, font_size: f32, display_mode: bool) -> Result<Vec<u8>, String> {
    let style = if display_mode { MathStyle::Display } else { MathStyle::Text };
    let layout_opts = LayoutOptions::default().with_style(style);

    let ast = parse(latex).map_err(|e| format!("parse: {}", e))?;
    let lbox = layout(&ast, &layout_opts);
    let display_list = to_display_list(&lbox);

    let render_opts = RenderOptions {
        font_size,
        padding: 2.0,
        font_dir: String::new(),
        device_pixel_ratio: 1.0,
    };

    render_to_png(&display_list, &render_opts)
}
