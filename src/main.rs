use std::sync::Arc;

use clap::Parser;
use tracing::info;

use crate::agent::Agent;
use crate::config::Config;
use crate::frontend::Feishu;

mod agent;
mod config;
mod frontend;

#[derive(Parser)]
#[command(name = "argus", about = "Personal AI assistant")]
struct Cli {
    /// Path to config file.
    #[arg(long, default_value = "~/.config/argus/argus.toml")]
    config: String,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,feishu=debug".parse().unwrap()),
        )
        .init();

    let cli = Cli::parse();
    let config_path = config::expand_tilde(&cli.config);
    let config = Config::load(&config_path)?;

    info!(
        workspace = %config.workspace_dir.display(),
        frontends = config.frontend.len(),
        upstreams = config.upstream.len(),
        "argus starting"
    );

    let agent = Agent::new();

    // Start feishu frontend (if configured).
    let feishu = if let Some(fe_cfg) = config.frontend.get("feishu") {
        anyhow::ensure!(
            !fe_cfg.app_id.is_empty() && !fe_cfg.app_secret.is_empty(),
            "frontend 'feishu' requires non-empty app_id and app_secret"
        );
        let f = Feishu::new(Arc::clone(&agent), fe_cfg, &config.workspace_dir);
        let handle = {
            let f = Arc::clone(&f);
            tokio::spawn(async move { f.run().await })
        };
        Some((f, handle))
    } else {
        info!("no feishu frontend configured");
        None
    };

    let agent_handle = {
        let a = Arc::clone(&agent);
        tokio::spawn(async move { a.run().await })
    };

    info!("argus running, press Ctrl-C to stop");
    tokio::signal::ctrl_c().await.ok();
    info!("shutdown initiated");

    agent.stop().await;
    let _ = agent_handle.await;
    if let Some((f, handle)) = feishu {
        f.stop().await;
        let _ = handle.await;
    }

    info!("argus stopped");
    Ok(())
}
