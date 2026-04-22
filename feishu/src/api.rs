//! Feishu REST API calls: send/reply/update messages, download resources.

use serde::Deserialize;

use crate::auth::Auth;
use crate::types::{ApiResponse, Error, Result, SendMessageData};

/// Feishu REST API client. Shares auth with the WS client.
#[derive(Clone)]
pub struct Api {
    auth: Auth,
}

impl Api {
    pub fn new(auth: Auth) -> Self {
        Self { auth }
    }

    /// Send a new message to a chat.
    pub async fn send_message(
        &self,
        receive_id_type: &str,
        receive_id: &str,
        msg_type: &str,
        content: &str,
    ) -> Result<String> {
        let url = format!(
            "{}/open-apis/im/v1/messages?receive_id_type={}",
            self.auth.base_url(),
            receive_id_type
        );
        let token = self.auth.token().await?;
        let body = serde_json::json!({
            "receive_id": receive_id,
            "msg_type": msg_type,
            "content": content,
        });

        let resp: ApiResponse<SendMessageData> = self
            .auth
            .http()
            .post(&url)
            .bearer_auth(&token)
            .json(&body)
            .send()
            .await?
            .json()
            .await?;

        if resp.code != 0 {
            return Err(Error::Api {
                code: resp.code,
                msg: resp.msg,
            });
        }

        Ok(resp.data.and_then(|d| d.message_id).unwrap_or_default())
    }

    /// Reply to a specific message. Returns the reply message ID.
    pub async fn reply_message(
        &self,
        message_id: &str,
        msg_type: &str,
        content: &str,
    ) -> Result<String> {
        let url = format!(
            "{}/open-apis/im/v1/messages/{}/reply",
            self.auth.base_url(),
            message_id
        );
        let token = self.auth.token().await?;
        let body = serde_json::json!({
            "msg_type": msg_type,
            "content": content,
        });

        let resp: ApiResponse<SendMessageData> = self
            .auth
            .http()
            .post(&url)
            .bearer_auth(&token)
            .json(&body)
            .send()
            .await?
            .json()
            .await?;

        if resp.code != 0 {
            return Err(Error::Api {
                code: resp.code,
                msg: resp.msg,
            });
        }

        Ok(resp.data.and_then(|d| d.message_id).unwrap_or_default())
    }

    /// Update (PATCH) an existing message's content (e.g. update a card).
    pub async fn update_message(&self, message_id: &str, content: &str) -> Result<()> {
        let url = format!(
            "{}/open-apis/im/v1/messages/{}",
            self.auth.base_url(),
            message_id
        );
        let token = self.auth.token().await?;
        let body = serde_json::json!({
            "msg_type": "interactive",
            "content": content,
        });

        let resp: ApiResponse = self
            .auth
            .http()
            .patch(&url)
            .bearer_auth(&token)
            .json(&body)
            .send()
            .await?
            .json()
            .await?;

        if resp.code != 0 {
            return Err(Error::Api {
                code: resp.code,
                msg: resp.msg,
            });
        }
        Ok(())
    }

    /// Download a message resource (image, audio, file).
    pub async fn download_resource(
        &self,
        message_id: &str,
        file_key: &str,
        resource_type: &str,
    ) -> Result<Vec<u8>> {
        let url = format!(
            "{}/open-apis/im/v1/messages/{}/resources/{}?type={}",
            self.auth.base_url(),
            message_id,
            file_key,
            resource_type,
        );
        let token = self.auth.token().await?;

        let resp = self
            .auth
            .http()
            .get(&url)
            .bearer_auth(&token)
            .send()
            .await?;

        if !resp.status().is_success() {
            return Err(Error::Api {
                code: resp.status().as_u16() as i64,
                msg: format!("download failed: {}", resp.status()),
            });
        }

        Ok(resp.bytes().await?.to_vec())
    }

    /// Upload an image to Feishu. Returns the image_key.
    pub async fn upload_image(&self, png_data: &[u8]) -> Result<String> {
        let url = format!("{}/open-apis/im/v1/images", self.auth.base_url());
        let token = self.auth.token().await?;

        let part = reqwest::multipart::Part::bytes(png_data.to_vec())
            .file_name("image.png")
            .mime_str("image/png")
            .unwrap();
        let form = reqwest::multipart::Form::new()
            .text("image_type", "message")
            .part("image", part);

        let resp: ApiResponse<ImageUploadData> = self
            .auth
            .http()
            .post(&url)
            .bearer_auth(&token)
            .multipart(form)
            .send()
            .await?
            .json()
            .await?;

        if resp.code != 0 {
            return Err(Error::Api {
                code: resp.code,
                msg: resp.msg,
            });
        }

        Ok(resp.data.and_then(|d| d.image_key).unwrap_or_default())
    }
}

#[derive(Deserialize)]
struct ImageUploadData {
    image_key: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_api() -> Api {
        let auth = Auth::new(
            reqwest::Client::new(),
            "https://open.feishu.cn",
            "test_id",
            "test_secret",
        );
        Api::new(auth)
    }

    #[tokio::test]
    async fn send_message_fails_without_token() {
        let api = make_api();
        // Will fail at token refresh (no server).
        let result = api.send_message("chat_id", "oc_123", "text", "{}").await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn reply_message_fails_without_token() {
        let api = make_api();
        let result = api.reply_message("om_123", "interactive", "{}").await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn update_message_fails_without_token() {
        let api = make_api();
        let result = api.update_message("om_123", "{}").await;
        assert!(result.is_err());
    }
}
