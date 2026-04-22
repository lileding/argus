//! Feishu tenant access token management with auto-refresh.

use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::{Mutex, Notify};

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
    state: Mutex<TokenState>,
    /// Notified when a refresh completes. Waiters check the token after wake.
    refreshed: Notify,
}

struct TokenState {
    token: String,
    expires_at: Instant,
    /// True while an HTTP refresh is in flight. Other callers wait on `refreshed`.
    refreshing: bool,
}

impl Auth {
    pub fn new(http: reqwest::Client, base_url: &str, app_id: &str, app_secret: &str) -> Self {
        Self {
            inner: Arc::new(Inner {
                http,
                base_url: base_url.to_string(),
                app_id: app_id.to_string(),
                app_secret: app_secret.to_string(),
                state: Mutex::new(TokenState {
                    token: String::new(),
                    expires_at: Instant::now(),
                    refreshing: false,
                }),
                refreshed: Notify::new(),
            }),
        }
    }

    /// Get a valid tenant access token, refreshing if needed.
    /// Never holds a lock across an HTTP call — safe for concurrent use
    /// within a single-task select loop.
    pub async fn token(&self) -> Result<String> {
        loop {
            let notified = {
                let state = self.inner.state.lock().await;
                // Fast path: token valid.
                if !state.token.is_empty()
                    && Instant::now() + Duration::from_secs(60) < state.expires_at
                {
                    return Ok(state.token.clone());
                }
                // Another task is already refreshing — register for notification
                // BEFORE dropping the lock to prevent lost wakeups.
                if state.refreshing {
                    Some(self.inner.refreshed.notified())
                } else {
                    None
                }
            };
            if let Some(notified) = notified {
                notified.await;
                continue; // re-check token
            }
            // We're the one to refresh.
            return self.refresh().await;
        }
    }

    async fn refresh(&self) -> Result<String> {
        // Mark refreshing (short lock, no I/O).
        {
            let mut state = self.inner.state.lock().await;
            // Double-check: someone else may have refreshed while we waited for the lock.
            if !state.token.is_empty()
                && Instant::now() + Duration::from_secs(60) < state.expires_at
            {
                // Don't set refreshing — we're not actually refreshing.
                return Ok(state.token.clone());
            }
            state.refreshing = true;
        }

        tracing::debug!("refreshing tenant access token");

        let url = format!("{}{}", self.inner.base_url, TOKEN_URL);
        let body = serde_json::json!({
            "app_id": self.inner.app_id,
            "app_secret": self.inner.app_secret,
        });

        // HTTP call WITHOUT holding any lock.
        let result = self
            .inner
            .http
            .post(&url)
            .json(&body)
            .send()
            .await
            .map_err(Error::from);

        let result = match result {
            Ok(resp) => resp.json::<TokenResponse>().await.map_err(Error::from),
            Err(e) => Err(e),
        };

        // Write back result (short lock, no I/O).
        let mut state = self.inner.state.lock().await;
        state.refreshing = false;

        match result {
            Ok(resp) => {
                if resp.code != 0 {
                    self.inner.refreshed.notify_waiters();
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

                // Wake all waiters so they pick up the new token.
                self.inner.refreshed.notify_waiters();
                Ok(token)
            }
            Err(e) => {
                self.inner.refreshed.notify_waiters();
                Err(e)
            }
        }
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
        let rt = tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap();
        rt.block_on(async {
            let state = auth.inner.state.lock().await;
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
