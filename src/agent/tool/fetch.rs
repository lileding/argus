use std::time::Duration;

use serde::Deserialize;
use serde_json::Value;

use super::Tool;

/// Maximum characters to return from a fetched page.
const MAX_CONTENT_LEN: usize = 16_000;

pub(super) struct Fetch<'a> {
    http: &'a reqwest::Client,
}

#[derive(Deserialize)]
struct Args {
    url: String,
}

impl<'a> Fetch<'a> {
    pub(super) fn new(http: &'a reqwest::Client) -> Self {
        Self { http }
    }
}

#[async_trait::async_trait]
impl<'a> Tool for Fetch<'a> {
    fn name(&self) -> &str {
        "fetch"
    }

    fn description(&self) -> &str {
        "Fetch the content of a web page. Returns the text content with HTML tags stripped. \
         Use this to read articles, documentation, or other web content."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "url": {
                    "type": "string",
                    "description": "The URL to fetch."
                }
            },
            "required": ["url"]
        })
    }

    async fn execute(&self, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let resp = match self
            .http
            .get(&parsed.url)
            .timeout(Duration::from_secs(15))
            .header("User-Agent", "Mozilla/5.0 (compatible; Argus/1.0)")
            .send()
            .await
        {
            Ok(r) => r,
            Err(e) => return format!("error: request failed: {e}"),
        };

        let status = resp.status();
        if !status.is_success() {
            return format!("error: HTTP {status}");
        }

        let content_type = resp
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_lowercase();

        let body = match resp.text().await {
            Ok(t) => t,
            Err(e) => return format!("error: failed to read response body: {e}"),
        };

        let text = if content_type.contains("text/html") {
            strip_html(&body)
        } else {
            body
        };

        if text.len() > MAX_CONTENT_LEN {
            let truncated: String = text.chars().take(MAX_CONTENT_LEN).collect();
            format!("{truncated}\n\n[truncated at {MAX_CONTENT_LEN} characters]")
        } else {
            text
        }
    }

    fn status_label(&self, args: &str) -> String {
        let url = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("url")?.as_str().map(String::from))
            .unwrap_or_default();
        if url.is_empty() {
            return "🌐 Fetching".into();
        }
        // Extract domain without pulling in the `url` crate.
        let domain = url
            .strip_prefix("https://")
            .or_else(|| url.strip_prefix("http://"))
            .map(|rest| rest.split('/').next().unwrap_or(rest).to_string());
        match domain {
            Some(d) => format!("🌐 Fetching: {d}"),
            None => format!("🌐 Fetching: {}", super::truncate_display(&url, 40)),
        }
    }
}

/// Strip HTML tags and extract readable text content.
fn strip_html(html: &str) -> String {
    use scraper::{Html, Selector};

    let document = Html::parse_document(html);
    let mut output = String::new();

    // Selectors for elements to remove entirely.
    let remove_selectors = ["script", "style", "noscript", "head"];
    let mut skip_ids = std::collections::HashSet::new();

    for sel_str in &remove_selectors {
        if let Ok(sel) = Selector::parse(sel_str) {
            for el in document.select(&sel) {
                skip_ids.insert(el.id());
            }
        }
    }

    // Block elements that should produce line breaks.
    let block_tags: &[&str] = &[
        "p",
        "div",
        "br",
        "hr",
        "h1",
        "h2",
        "h3",
        "h4",
        "h5",
        "h6",
        "li",
        "tr",
        "blockquote",
        "pre",
        "section",
        "article",
        "header",
        "footer",
        "nav",
        "main",
        "aside",
        "dt",
        "dd",
    ];

    for node in document.tree.nodes() {
        if let Some(element) = node.value().as_element() {
            // Skip children of removed elements.
            let mut ancestor = node.parent();
            let mut should_skip = false;
            while let Some(parent) = ancestor {
                if skip_ids.contains(&parent.id()) {
                    should_skip = true;
                    break;
                }
                ancestor = parent.parent();
            }
            if should_skip {
                continue;
            }

            let tag = element.name();
            if block_tags.contains(&tag) && !output.ends_with('\n') {
                output.push('\n');
            }
        }

        if let Some(text) = node.value().as_text() {
            // Skip text inside removed elements.
            let mut ancestor = node.parent();
            let mut should_skip = false;
            while let Some(parent) = ancestor {
                if skip_ids.contains(&parent.id()) {
                    should_skip = true;
                    break;
                }
                ancestor = parent.parent();
            }
            if should_skip {
                continue;
            }

            let t = text.trim();
            if !t.is_empty() {
                if !output.is_empty() && !output.ends_with('\n') && !output.ends_with(' ') {
                    output.push(' ');
                }
                output.push_str(t);
            }
        }
    }

    // Collapse multiple blank lines.
    let mut result = String::new();
    let mut blank_count = 0;
    for line in output.lines() {
        if line.trim().is_empty() {
            blank_count += 1;
            if blank_count <= 1 {
                result.push('\n');
            }
        } else {
            blank_count = 0;
            result.push_str(line);
            result.push('\n');
        }
    }

    result.trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strip_html_simple_paragraph() {
        let result = strip_html("<p>hello</p>");
        assert_eq!(result, "hello");
    }

    #[test]
    fn strip_html_script_removal() {
        let result = strip_html("<p>text</p><script>evil()</script>");
        assert_eq!(result, "text");
    }

    #[test]
    fn strip_html_style_removal() {
        let result = strip_html("<style>body{color:red}</style><div>content</div>");
        assert_eq!(result, "content");
    }

    #[test]
    fn strip_html_nested_elements() {
        let result = strip_html("<div><p>one</p><p>two</p></div>");
        assert!(result.contains("one"), "expected 'one' in: {result}");
        assert!(result.contains("two"), "expected 'two' in: {result}");
    }

    #[test]
    fn strip_html_empty_body() {
        let result = strip_html("<html><body></body></html>");
        assert_eq!(result, "");
    }

    #[test]
    fn strip_html_plain_text() {
        let result = strip_html("just text");
        assert_eq!(result, "just text");
    }

    #[test]
    fn strip_html_complex_page() {
        let html = r#"
        <html>
        <head><title>Test</title></head>
        <script>var x = 1;</script>
        <style>.foo { display: none; }</style>
        <body>
            <h1>Title</h1>
            <p>Body text here.</p>
            <div>More content.</div>
        </body>
        </html>
        "#;
        let result = strip_html(html);
        assert!(result.contains("Title"), "expected 'Title' in: {result}");
        assert!(
            result.contains("Body text here."),
            "expected body text in: {result}"
        );
        assert!(
            result.contains("More content."),
            "expected more content in: {result}"
        );
        // Script and style content must not appear.
        assert!(
            !result.contains("var x"),
            "script leaked into output: {result}"
        );
        assert!(
            !result.contains("display: none"),
            "style leaked into output: {result}"
        );
    }
}
