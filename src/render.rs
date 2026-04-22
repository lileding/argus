//! Markdown processing: LaTeX rendering + Feishu card building.

use regex::Regex;
use std::sync::LazyLock;
use tracing::{debug, warn};

use feishu::api::Api;

// --- LaTeX detection ---

static DISPLAY_RE: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\$\$(.+?)\$\$").unwrap());
// Inline $...$ — we detect after removing $$...$$ blocks, so no look-around needed.
static INLINE_RE: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\$([^\$\n]+?)\$").unwrap());

struct LatexBlock {
    full: String, // original text including delimiters
    expr: String, // expression without delimiters
    display: bool,
}

fn detect_latex(text: &str) -> Vec<LatexBlock> {
    let mut blocks = Vec::new();

    // Display mode: $$...$$
    for cap in DISPLAY_RE.captures_iter(text) {
        blocks.push(LatexBlock {
            full: cap[0].to_string(),
            expr: cap[1].to_string(),
            display: true,
        });
    }

    // Inline mode: $...$  (exclude already-matched $$)
    let without_display = DISPLAY_RE.replace_all(text, "");
    for cap in INLINE_RE.captures_iter(&without_display) {
        blocks.push(LatexBlock {
            full: cap[0].to_string(),
            expr: cap[1].to_string(),
            display: false,
        });
    }

    blocks
}

// --- LaTeX rendering ---

fn render_latex_png(expr: &str, display: bool) -> Result<Vec<u8>, String> {
    use ratex_layout::{LayoutOptions, layout, to_display_list};
    use ratex_parser::parser::parse;
    use ratex_render::{RenderOptions, render_to_png};
    use ratex_types::math_style::MathStyle;

    let style = if display {
        MathStyle::Display
    } else {
        MathStyle::Text
    };
    let layout_opts = LayoutOptions::default().with_style(style);

    let ast = parse(expr).map_err(|e| format!("parse: {e}"))?;
    let lbox = layout(&ast, &layout_opts);
    let display_list = to_display_list(&lbox);

    let render_opts = RenderOptions {
        font_size: 20.0,
        padding: 0.0,
        font_dir: String::new(),
        device_pixel_ratio: 2.5,
    };

    render_to_png(&display_list, &render_opts)
}

// --- Markdown processing ---

/// Process markdown: render LaTeX to images, return processed markdown.
pub(crate) async fn process_markdown(md: &str, api: &Api) -> String {
    let blocks = detect_latex(md);
    if blocks.is_empty() {
        return md.to_string();
    }

    let mut result = md.to_string();

    // Only render display-mode LaTeX (inline is too small for images).
    for block in &blocks {
        if !block.display {
            continue;
        }

        let png = match render_latex_png(&block.expr, block.display) {
            Ok(data) => data,
            Err(e) => {
                warn!(expr = block.expr, error = e, "LaTeX render failed");
                continue;
            }
        };

        let image_key = match api.upload_image(&png).await {
            Ok(key) => key,
            Err(e) => {
                warn!(error = %e, "LaTeX image upload failed");
                continue;
            }
        };

        debug!(expr = block.expr, image_key, "LaTeX rendered");
        result = result.replace(&block.full, &format!("![]({})", image_key));
    }

    result
}

// --- Feishu card building ---

/// Build a Feishu interactive card from markdown.
/// Splits code blocks into separate collapsible panels.
pub(crate) fn markdown_to_card(md: &str) -> String {
    let segments = split_at_code_blocks(md);
    if segments.len() <= 1 {
        return build_card(md);
    }
    build_card_multi(&segments)
}

/// Build a simple card with one markdown element.
fn build_card(content: &str) -> String {
    serde_json::json!({
        "schema": "2.0",
        "config": {"update_multi": true},
        "body": {
            "elements": [{"tag": "markdown", "content": content}]
        }
    })
    .to_string()
}

/// Build a card with multiple elements (markdown + collapsible code blocks).
fn build_card_multi(segments: &[CodeSegment]) -> String {
    let elements: Vec<serde_json::Value> = segments
        .iter()
        .map(|seg| match seg {
            CodeSegment::Text(text) => {
                serde_json::json!({"tag": "markdown", "content": text})
            }
            CodeSegment::Code { lang, content } => {
                let header = if lang.is_empty() {
                    "Code".to_string()
                } else {
                    lang.clone()
                };
                serde_json::json!({
                    "tag": "collapsible_panel",
                    "expanded": true,
                    "header": {
                        "title": {
                            "tag": "plain_text",
                            "content": header
                        }
                    },
                    "border": {"color": "grey"},
                    "background_color": "grey",
                    "elements": [{
                        "tag": "div",
                        "text": {
                            "tag": "plain_text",
                            "content": content
                        }
                    }]
                })
            }
        })
        .collect();

    serde_json::json!({
        "schema": "2.0",
        "config": {"update_multi": true},
        "body": {"elements": elements}
    })
    .to_string()
}

enum CodeSegment {
    Text(String),
    Code { lang: String, content: String },
}

/// Split markdown at fenced code block boundaries.
fn split_at_code_blocks(md: &str) -> Vec<CodeSegment> {
    let mut segments = Vec::new();
    let mut current_text = String::new();
    let mut in_code = false;
    let mut code_lang = String::new();
    let mut code_content = String::new();

    for line in md.lines() {
        if !in_code && line.starts_with("```") {
            // Start of code block.
            if !current_text.trim().is_empty() {
                segments.push(CodeSegment::Text(current_text.trim().to_string()));
            }
            current_text.clear();
            in_code = true;
            code_lang = line[3..].trim().to_string();
            code_content.clear();
        } else if in_code && line.starts_with("```") {
            // End of code block.
            segments.push(CodeSegment::Code {
                lang: code_lang.clone(),
                content: code_content.trim_end().to_string(),
            });
            in_code = false;
            code_lang.clear();
        } else if in_code {
            if !code_content.is_empty() {
                code_content.push('\n');
            }
            code_content.push_str(line);
        } else {
            if !current_text.is_empty() {
                current_text.push('\n');
            }
            current_text.push_str(line);
        }
    }

    // Trailing text or unclosed code block.
    if in_code {
        // Unclosed code block — treat as text.
        current_text.push_str(&format!("```{code_lang}\n{code_content}"));
    }
    if !current_text.trim().is_empty() {
        segments.push(CodeSegment::Text(current_text.trim().to_string()));
    }

    segments
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detect_display_latex() {
        let blocks = detect_latex("Hello $$x^2$$ world");
        assert_eq!(blocks.len(), 1);
        assert_eq!(blocks[0].expr, "x^2");
        assert!(blocks[0].display);
    }

    #[test]
    fn detect_inline_latex() {
        let blocks = detect_latex("Hello $x^2$ world");
        assert_eq!(blocks.len(), 1);
        assert_eq!(blocks[0].expr, "x^2");
        assert!(!blocks[0].display);
    }

    #[test]
    fn detect_both() {
        let blocks = detect_latex("Inline $a$ and display $$b$$");
        assert_eq!(blocks.len(), 2);
    }

    #[test]
    fn no_latex() {
        let blocks = detect_latex("No math here, just $100 dollars");
        // "$100 dollars" has no closing $, so no match
        assert!(blocks.is_empty());
    }

    #[test]
    fn split_no_code() {
        let segs = split_at_code_blocks("hello world");
        assert_eq!(segs.len(), 1);
        assert!(matches!(&segs[0], CodeSegment::Text(t) if t == "hello world"));
    }

    #[test]
    fn split_with_code() {
        let md = "text before\n```rust\nfn main() {}\n```\ntext after";
        let segs = split_at_code_blocks(md);
        assert_eq!(segs.len(), 3);
        assert!(matches!(&segs[0], CodeSegment::Text(t) if t == "text before"));
        assert!(
            matches!(&segs[1], CodeSegment::Code { lang, content } if lang == "rust" && content == "fn main() {}")
        );
        assert!(matches!(&segs[2], CodeSegment::Text(t) if t == "text after"));
    }

    #[test]
    fn split_unclosed_code() {
        let md = "text\n```python\nprint('hi')";
        let segs = split_at_code_blocks(md);
        // "text" segment + unclosed code block appended as text.
        assert_eq!(segs.len(), 2);
        assert!(matches!(&segs[0], CodeSegment::Text(t) if t == "text"));
    }

    #[test]
    fn render_simple_latex() {
        let result = render_latex_png("x^2", true);
        assert!(result.is_ok());
        let png = result.unwrap();
        assert!(!png.is_empty());
        // PNG magic bytes.
        assert_eq!(&png[..4], b"\x89PNG");
    }
}
