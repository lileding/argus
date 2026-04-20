//! Top-level Feishu client combining WS event stream and REST API.

use crate::api::Api;
use crate::auth::Auth;
use crate::types::Result;
use crate::ws::EventStream;

const FEISHU_BASE_URL: &str = "https://open.feishu.cn";

/// Unified Feishu client. Shares auth between WS and REST API paths.
pub struct Client {
    auth: Auth,
}

impl Client {
    /// Create a new Feishu client for the China (feishu.cn) endpoint.
    pub fn new(app_id: &str, app_secret: &str) -> Self {
        Self::with_base_url(app_id, app_secret, FEISHU_BASE_URL)
    }

    /// Create a client with a custom base URL (e.g. for Lark international).
    pub fn with_base_url(app_id: &str, app_secret: &str, base_url: &str) -> Self {
        let http = reqwest::Client::new();
        Self {
            auth: Auth::new(http, base_url, app_id, app_secret),
        }
    }

    /// Connect to the Feishu WebSocket event stream.
    pub async fn connect_ws(&self) -> Result<EventStream> {
        EventStream::connect(&self.auth).await
    }

    /// Get the REST API client for sending messages, updating cards, etc.
    pub fn api(&self) -> Api {
        Api::new(self.auth.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn client_creates_api_without_network() {
        let client = Client::new("test_id", "test_secret");
        let _api = client.api(); // Should not panic.
    }
}
