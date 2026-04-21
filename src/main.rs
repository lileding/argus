use std::sync::Arc;
use tracing::info;

use crate::agent::Agent;
use crate::frontend::Feishu;

mod agent;
mod config;
mod frontend;

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,feishu=debug".parse().unwrap()),
        )
        .init();

    let app_id = std::env::var("FEISHU_APP_ID").expect("FEISHU_APP_ID required");
    let app_secret = std::env::var("FEISHU_APP_SECRET").expect("FEISHU_APP_SECRET required");

    info!("argus starting");

    let workspace_dir = std::env::var("ARGUS_WORKSPACE").unwrap_or_else(|_| ".".into());
    let workspace_dir = std::path::Path::new(&workspace_dir);

    let agent = Agent::new();
    let feishu = Feishu::new(Arc::clone(&agent), &app_id, &app_secret, workspace_dir);

    let agent_handle = {
        let a = Arc::clone(&agent);
        tokio::spawn(async move { a.run().await })
    };
    let feishu_handle = {
        let f = Arc::clone(&feishu);
        tokio::spawn(async move { f.run().await })
    };

    info!("argus running, press Ctrl-C to stop");
    tokio::signal::ctrl_c().await.ok();
    info!("shutdown initiated");

    // Shutdown order: agent first (finishes in-flight task + closes events),
    // then feishu (outbound drains remaining messages, then exits).
    agent.stop().await;
    let _ = agent_handle.await;
    feishu.stop().await;
    let _ = feishu_handle.await;

    info!("argus stopped");
}
