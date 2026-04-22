use serde::Deserialize;
use serde_json::Value;
use tracing::warn;

use super::Tool;

pub(super) struct Search<'a> {
    http: &'a reqwest::Client,
    tavily_api_key: &'a str,
}

#[derive(Deserialize)]
struct Args {
    query: String,
}

#[derive(Deserialize)]
struct TavilyResponse {
    answer: Option<String>,
    results: Option<Vec<TavilyResult>>,
}

#[derive(Deserialize)]
struct TavilyResult {
    title: String,
    url: String,
    content: String,
}

impl<'a> Search<'a> {
    pub(super) fn new(http: &'a reqwest::Client, tavily_api_key: &'a str) -> Self {
        Self {
            http,
            tavily_api_key,
        }
    }

    async fn tavily_search(&self, query: &str) -> Result<String, String> {
        let body = serde_json::json!({
            "api_key": self.tavily_api_key,
            "query": query,
            "max_results": 5,
            "include_answer": true,
        });

        let resp = self
            .http
            .post("https://api.tavily.com/search")
            .json(&body)
            .timeout(std::time::Duration::from_secs(15))
            .send()
            .await
            .map_err(|e| format!("tavily request failed: {e}"))?;

        if !resp.status().is_success() {
            return Err(format!("tavily returned status {}", resp.status()));
        }

        let data: TavilyResponse = resp
            .json()
            .await
            .map_err(|e| format!("tavily response parse error: {e}"))?;

        let mut output = String::new();
        if let Some(answer) = &data.answer
            && !answer.is_empty()
        {
            output.push_str(&format!("Answer: {answer}\n\n"));
        }
        if let Some(results) = &data.results {
            for (i, r) in results.iter().enumerate() {
                output.push_str(&format!(
                    "{}. {}\n   URL: {}\n   {}\n\n",
                    i + 1,
                    r.title,
                    r.url,
                    r.content,
                ));
            }
        }
        if output.is_empty() {
            return Err("tavily returned no results".into());
        }
        Ok(output)
    }

    async fn ddg_search(&self, query: &str) -> String {
        let url = format!("https://html.duckduckgo.com/html/?q={}", urlencoding(query));

        let resp = match self
            .http
            .get(&url)
            .header("User-Agent", "Mozilla/5.0 (compatible; Argus/1.0)")
            .timeout(std::time::Duration::from_secs(15))
            .send()
            .await
        {
            Ok(r) => r,
            Err(e) => return format!("error: DuckDuckGo request failed: {e}"),
        };

        let html = match resp.text().await {
            Ok(t) => t,
            Err(e) => return format!("error: failed to read DuckDuckGo response: {e}"),
        };

        let document = scraper::Html::parse_document(&html);
        let link_sel = match scraper::Selector::parse(".result__a") {
            Ok(s) => s,
            Err(_) => return "error: failed to parse DDG HTML selector".into(),
        };
        let snippet_sel = match scraper::Selector::parse(".result__snippet") {
            Ok(s) => s,
            Err(_) => return "error: failed to parse DDG HTML selector".into(),
        };

        let links: Vec<_> = document.select(&link_sel).collect();
        let snippets: Vec<_> = document.select(&snippet_sel).collect();

        if links.is_empty() {
            return "No results found.".into();
        }

        let mut output = String::new();
        let count = links.len().min(5);
        for i in 0..count {
            let title = links[i].text().collect::<String>();
            let href = links[i].value().attr("href").unwrap_or("");
            let snippet = if i < snippets.len() {
                snippets[i].text().collect::<String>()
            } else {
                String::new()
            };
            output.push_str(&format!(
                "{}. {}\n   URL: {}\n   {}\n\n",
                i + 1,
                title.trim(),
                href,
                snippet.trim(),
            ));
        }
        output
    }
}

#[async_trait::async_trait]
impl<'a> Tool for Search<'a> {
    fn name(&self) -> &str {
        "search"
    }

    fn description(&self) -> &str {
        "Search the web for current information. Use this when you need up-to-date facts, news, \
         or information not in your training data."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "query": {
                    "type": "string",
                    "description": "The search query."
                }
            },
            "required": ["query"]
        })
    }

    async fn execute(&self, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        // Try Tavily first if key is configured.
        if !self.tavily_api_key.is_empty() {
            match self.tavily_search(&parsed.query).await {
                Ok(result) => return result,
                Err(e) => {
                    warn!(error = %e, "tavily search failed, falling back to DuckDuckGo");
                }
            }
        }

        // Fallback to DuckDuckGo.
        self.ddg_search(&parsed.query).await
    }

    fn status_label(&self, args: &str) -> String {
        let query = serde_json::from_str::<Value>(args)
            .ok()
            .and_then(|v| v.get("query")?.as_str().map(String::from))
            .unwrap_or_default();
        if query.is_empty() {
            return "🔍 Searching".into();
        }
        format!("🔍 Searching: {}", super::truncate_display(&query, 40))
    }
}

/// Simple percent-encoding for URL query parameters.
fn urlencoding(s: &str) -> String {
    let mut result = String::with_capacity(s.len() * 2);
    for byte in s.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                result.push(byte as char);
            }
            b' ' => result.push('+'),
            _ => {
                result.push_str(&format!("%{byte:02X}"));
            }
        }
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urlencoding_ascii_spaces() {
        // This implementation uses form-encoding: space → '+'
        assert_eq!(urlencoding("hello world"), "hello+world");
    }

    #[test]
    fn urlencoding_special_chars() {
        assert_eq!(urlencoding("a+b=c"), "a%2Bb%3Dc");
    }

    #[test]
    fn urlencoding_already_safe() {
        assert_eq!(urlencoding("abc123"), "abc123");
    }

    #[test]
    fn urlencoding_unicode() {
        let encoded = urlencoding("你好");
        // "你" = E4 BD A0, "好" = E5 A5 BD
        assert_eq!(encoded, "%E4%BD%A0%E5%A5%BD");
    }

    #[test]
    fn urlencoding_empty() {
        assert_eq!(urlencoding(""), "");
    }
}
