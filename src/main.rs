use std::sync::Arc;

use clap::Parser;
use tracing::info;

use crate::agent::Agent;
use crate::config::Config;
use crate::gateway::Gateway;
use crate::server::Server;
use crate::upstream::Upstream;

mod agent;
mod config;
mod gateway;
mod server;
mod upstream;

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

    info!(workspace = %config.workspace_dir.display(), "argus starting");

    let upstream = Upstream::new(&config.upstream);
    let agent = Agent::new(&config.agent, &upstream)?;
    let gateway = Gateway::new(&config.gateway, &agent, &config.workspace_dir);

    let gateway_handles = gateway.spawn_all();
    let agent_handle = {
        let a = Arc::clone(&agent);
        tokio::spawn(async move { a.run().await })
    };

    info!("argus running, press Ctrl-C to stop");
    tokio::signal::ctrl_c().await.ok();
    info!("shutdown initiated");

    agent.stop().await;
    let _ = agent_handle.await;
    gateway.stop().await;
    for handle in gateway_handles {
        let _ = handle.await;
    }

    info!("argus stopped");
    Ok(())
}
