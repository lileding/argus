use chrono::Utc;
use serde::Deserialize;
use serde_json::Value;

use super::Tool;

pub(super) struct CurrentTime;

#[derive(Deserialize)]
struct Args {
    timezone: Option<String>,
}

#[async_trait::async_trait]
impl Tool for CurrentTime {
    fn name(&self) -> &str {
        "current_time"
    }

    fn description(&self) -> &str {
        "Get the current date and time. Optionally specify a timezone (e.g. \"Asia/Shanghai\", \
         \"America/New_York\"). Defaults to UTC."
    }

    fn parameters(&self) -> Value {
        serde_json::json!({
            "type": "object",
            "properties": {
                "timezone": {
                    "type": "string",
                    "description": "IANA timezone name (e.g. Asia/Shanghai). Defaults to UTC."
                }
            },
            "required": []
        })
    }

    async fn execute(&self, args: &str) -> String {
        let parsed: Args = match serde_json::from_str(args) {
            Ok(a) => a,
            Err(e) => return format!("error: invalid arguments: {e}"),
        };

        let now = Utc::now();

        if let Some(ref tz_name) = parsed.timezone {
            let tz: chrono_tz::Tz = match tz_name.parse() {
                Ok(t) => t,
                Err(e) => return format!("error: invalid timezone \"{tz_name}\": {e}"),
            };
            let local = now.with_timezone(&tz);
            format!(
                "datetime: {}\ndate: {}\nday: {}\ntimezone: {}\nunix: {}",
                local.format("%Y-%m-%dT%H:%M:%S%:z"),
                local.format("%Y-%m-%d"),
                local.format("%A"),
                tz_name,
                now.timestamp(),
            )
        } else {
            format!(
                "datetime: {}\ndate: {}\nday: {}\ntimezone: UTC\nunix: {}",
                now.format("%Y-%m-%dT%H:%M:%S+00:00"),
                now.format("%Y-%m-%d"),
                now.format("%A"),
                now.timestamp(),
            )
        }
    }

    fn status_label(&self, _args: &str) -> String {
        "🕐 Checking time".into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn valid_timezone() {
        let tool = CurrentTime;
        let result = tool.execute(r#"{"timezone": "Asia/Shanghai"}"#).await;
        assert!(
            result.contains("timezone: Asia/Shanghai"),
            "expected timezone in output, got: {result}"
        );
        assert!(result.contains("unix:"));
    }

    #[tokio::test]
    async fn invalid_timezone() {
        let tool = CurrentTime;
        let result = tool.execute(r#"{"timezone": "Foo/Bar"}"#).await;
        assert!(
            result.starts_with("error:"),
            "expected error, got: {result}"
        );
    }

    #[tokio::test]
    async fn empty_args_defaults_to_utc() {
        let tool = CurrentTime;
        let result = tool.execute("{}").await;
        assert!(
            result.contains("timezone: UTC"),
            "expected UTC default, got: {result}"
        );
    }

    #[tokio::test]
    async fn default_contains_unix() {
        let tool = CurrentTime;
        let result = tool.execute("{}").await;
        assert!(
            result.contains("unix:"),
            "expected unix timestamp, got: {result}"
        );
    }
}
