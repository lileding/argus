use std::path::PathBuf;
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
    /// Workspace directory (contains config.toml and .files/).
    #[arg(long, default_value = "~/.argus")]
    workspace: String,
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

    // Resolve workspace path (expand ~ to home dir).
    let workspace_dir = expand_tilde(&cli.workspace);
    std::fs::create_dir_all(&workspace_dir)?;

    // Load config from workspace/config.toml.
    let config_path = workspace_dir.join("config.toml");
    let config = Config::load(&config_path, workspace_dir)?;

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
            fe_cfg.frontend_type == "feishu",
            "frontend 'feishu' has wrong type: '{}'",
            fe_cfg.frontend_type
        );
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

/// Expand leading ~ to the user's home directory.
fn expand_tilde(path: &str) -> PathBuf {
    if let Some(rest) = path.strip_prefix("~/") {
        if let Some(home) = dirs_home() {
            return home.join(rest);
        }
    } else if path == "~"
        && let Some(home) = dirs_home()
    {
        return home;
    }
    PathBuf::from(path)
}

fn dirs_home() -> Option<PathBuf> {
    std::env::var_os("HOME").map(PathBuf::from)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expand_tilde_with_subpath() {
        let result = expand_tilde("~/foo/bar");
        // Should expand ~ to $HOME.
        assert!(result.to_str().unwrap().ends_with("/foo/bar"));
        assert!(!result.to_str().unwrap().starts_with("~"));
    }

    #[test]
    fn expand_tilde_bare() {
        let result = expand_tilde("~");
        assert!(!result.to_str().unwrap().starts_with("~"));
    }

    #[test]
    fn expand_tilde_absolute_passthrough() {
        assert_eq!(expand_tilde("/abs/path"), PathBuf::from("/abs/path"));
    }

    #[test]
    fn expand_tilde_relative_passthrough() {
        assert_eq!(expand_tilde("./relative"), PathBuf::from("./relative"));
    }
}
