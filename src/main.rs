use std::sync::Arc;

use clap::Parser;
use tracing::info;

use crate::agent::Agent;
use crate::config::Config;
use crate::server::Server;

mod agent;
mod config;
mod frontend;
mod server;
#[allow(dead_code)] // Not yet wired to agent; will be consumed when echo → real model.
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

    info!(
        workspace = %config.workspace_dir.display(),
        frontends = config.frontend.len(),
        upstreams = config.upstream.len(),
        "argus starting"
    );

    let agent = Agent::new();

    // Create all frontends from config and spawn them.
    let frontends = frontend::create_all(&config, &agent);
    let mut frontend_handles = Vec::new();
    for (name, f) in &frontends {
        let f = Arc::clone(f);
        let name = name.clone();
        frontend_handles.push((name, tokio::spawn(async move { f.run().await })));
    }

    // Spawn agent.
    let agent_handle = {
        let a = Arc::clone(&agent);
        tokio::spawn(async move { a.run().await })
    };

    info!("argus running, press Ctrl-C to stop");
    tokio::signal::ctrl_c().await.ok();
    info!("shutdown initiated");

    // Shutdown: agent first (finishes in-flight), then frontends.
    agent.stop().await;
    let _ = agent_handle.await;
    for (name, f) in &frontends {
        info!(frontend = name, "stopping frontend");
        f.stop().await;
    }
    for (name, handle) in frontend_handles {
        let _ = handle.await;
        info!(frontend = name, "frontend stopped");
    }

    info!("argus stopped");
    Ok(())
}
