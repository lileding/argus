mod messages;
mod notifications;

use std::sync::Arc;

use sqlx::postgres::PgPoolOptions;
use tracing::info;

use crate::config::DatabaseConfig;

pub(crate) use messages::InboundMessage;

/// Database handle. Sub-objects group operations by table/feature.
/// PgPool is internally Arc'd — clone is zero-cost.
pub(crate) struct Database {
    pub messages: messages::Messages,
    pub notifications: notifications::Notifications,
}

impl Database {
    pub(crate) async fn connect(config: &DatabaseConfig) -> anyhow::Result<Arc<Self>> {
        anyhow::ensure!(!config.dsn.is_empty(), "database.dsn is required");

        let pool = PgPoolOptions::new()
            .max_connections(10)
            .connect(&config.dsn)
            .await?;

        info!("database connected");

        Ok(Arc::new(Self {
            messages: messages::Messages::new(pool.clone()),
            notifications: notifications::Notifications::new(pool),
        }))
    }
}
