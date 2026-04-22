use clap::Parser;
use tokio_util::sync::CancellationToken;
use tracing::info;

use crate::agent::Agent;
use crate::config::Config;
use crate::database::Database;
use crate::embedder::Embedder;
use crate::gateway::Gateway;
use crate::recovery::Recovery;
use crate::upstream::Upstream;

mod agent;
mod config;
mod database;
mod embedder;
mod gateway;
mod recovery;
mod render;
mod upstream;

#[derive(Debug, thiserror::Error)]
enum AppError {
    #[error("config: {0}")]
    Config(#[from] config::ConfigError),
    #[error("database: {0}")]
    Database(#[from] database::DatabaseError),
    #[error("embedder: {0}")]
    Embedder(#[from] embedder::EmbedderError),
    #[error("upstream: {0}")]
    Upstream(#[from] upstream::types::ClientError),
}

#[derive(Parser)]
#[command(name = "argus", about = "Personal AI assistant")]
struct Cli {
    /// Path to config file.
    #[arg(long, default_value = "~/.config/argus/argus.toml")]
    config: String,
}

async fn shutdown_signal(cancel: &CancellationToken) {
    info!("argus running, press Ctrl-C to stop");
    tokio::signal::ctrl_c().await.ok();
    info!("shutdown initiated");
    cancel.cancel();
}

#[tokio::main]
async fn main() -> Result<(), AppError> {
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

    let db = Database::connect(&config.database).await?;
    let upstream = Upstream::new(&config.upstream);
    let embedder = Embedder::new(&config.embedder, &upstream, &db)?;
    let agent = Agent::new(
        &config.agent,
        &upstream,
        &db,
        &embedder,
        &config.workspace_dir,
    )?;
    let gateway = Gateway::new(
        &config.gateway,
        agent.port(),
        &upstream,
        &db,
        &config.workspace_dir,
    );
    let recovery = Recovery::new(&db, &gateway);

    let cancel = CancellationToken::new();

    tokio::join!(
        gateway.run(&cancel),
        agent.run(&cancel),
        embedder.run(&cancel),
        recovery.run(&cancel),
        shutdown_signal(&cancel),
    );

    info!("argus stopped");
    Ok(())
}
