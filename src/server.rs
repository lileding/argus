use std::sync::Arc;

/// Common lifecycle for any long-running service (agent, frontend, etc.).
#[async_trait::async_trait]
pub(crate) trait Server: Send + Sync {
    async fn run(self: Arc<Self>);
    async fn stop(&self);
}
