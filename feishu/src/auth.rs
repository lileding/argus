//! Feishu tenant access token management with auto-refresh.

use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::RwLock;
use tracing;

use crate::types::{Error, Result, TokenResponse};

const TOKEN_URL: &str = "/open-apis/auth/v3/tenant_access_token/internal";

/// Shared token state — acquired once, refreshed before expiry.
#[derive(Clone)]
pub struct Auth {
    inner: Arc<Inner>,
}

struct Inner {
    http: reqwest::Client,
    base_url: String,
    app_id: String,
    app_secret: String,
    state: RwLock<TokenState>,
}

struct TokenState {
    token: String,
    expires_at: Instant,
}

impl Auth {
    pub fn new(http: reqwest::Client, base_url: &str, app_id: &str, app_secret: &str) -> Self {
        Self {
            inner: Arc::new(Inner {
                http,
                base_url: base_url.to_string(),
                app_id: app_id.to_string(),
                app_secret: app_secret.to_string(),
                state: RwLock::new(TokenState {
                    token: String::new(),
                    expires_at: Instant::now(),
                }),
            }),
        }
    }

    /// Get a valid tenant access token, refreshing if needed.
    pub async fn token(&self) -> Result<String> {
        // Fast path: token still valid (with 60s margin).
        {
            let state = self.inner.state.read().await;
            if !state.token.is_empty()
                && Instant::now() + Duration::from_secs(60) < state.expires_at
            {
                return Ok(state.token.clone());
            }
        }
        // Slow path: refresh.
        self.refresh().await
    }

    async fn refresh(&self) -> Result<String> {
        let mut state = self.inner.state.write().await;
        // Double-check after acquiring write lock.
        if !state.token.is_empty() && Instant::now() + Duration::from_secs(60) < state.expires_at {
            return Ok(state.token.clone());
        }

        let url = format!("{}{}", self.inner.base_url, TOKEN_URL);
        let body = serde_json::json!({
            "app_id": self.inner.app_id,
            "app_secret": self.inner.app_secret,
        });

        tracing::debug!("refreshing tenant access token");

        let resp: TokenResponse = self
            .inner
            .http
            .post(&url)
            .json(&body)
            .send()
            .await?
            .json()
            .await?;

        if resp.code != 0 {
            return Err(Error::Auth {
                code: resp.code,
                msg: resp.msg,
            });
        }

        let token = resp.tenant_access_token.ok_or_else(|| Error::Auth {
            code: resp.code,
            msg: "missing token in response".into(),
        })?;
        let expire_secs = resp.expire.unwrap_or(7200);

        state.token = token.clone();
        state.expires_at = Instant::now() + Duration::from_secs(expire_secs);

        tracing::info!(
            expires_in_secs = expire_secs,
            "tenant access token refreshed"
        );
        Ok(token)
    }

    /// Get app credentials (for WS endpoint which uses app_id/app_secret directly).
    pub fn app_id(&self) -> &str {
        &self.inner.app_id
    }

    pub fn app_secret(&self) -> &str {
        &self.inner.app_secret
    }

    pub fn base_url(&self) -> &str {
        &self.inner.base_url
    }

    pub fn http(&self) -> &reqwest::Client {
        &self.inner.http
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn token_state_starts_expired() {
        let auth = Auth::new(
            reqwest::Client::new(),
            "https://open.feishu.cn",
            "test_id",
            "test_secret",
        );
        // Sync check: initial state should be expired.
        let rt = tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap();
        rt.block_on(async {
            let state = auth.inner.state.read().await;
            assert!(state.token.is_empty());
            assert!(state.expires_at <= Instant::now());
        });
    }

    #[tokio::test]
    async fn refresh_fails_without_server() {
        let auth = Auth::new(
            reqwest::Client::new(),
            "http://localhost:1", // unreachable
            "test_id",
            "test_secret",
        );
        let result = auth.token().await;
        assert!(result.is_err());
    }
}
