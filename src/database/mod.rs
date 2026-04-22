mod conversation;
mod documents;
mod messages;
mod notifications;

use std::sync::Arc;

use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Row};
use tracing::info;

use crate::config::DatabaseConfig;

pub(crate) use documents::Documents;
pub(crate) use messages::{InboundMessage, Messages};
pub(crate) use notifications::Notifications;

/// Database handle. Sub-objects group operations by table/feature.
/// PgPool is internally Arc'd — clone is zero-cost.
pub(crate) struct Database {
    pub(crate) messages: messages::Messages,
    pub(crate) notifications: notifications::Notifications,
    pub(crate) conversation: conversation::Conversation,
    pub(crate) documents: documents::Documents,
}

impl Database {
    pub(crate) async fn connect(config: &DatabaseConfig) -> anyhow::Result<Arc<Self>> {
        anyhow::ensure!(!config.dsn.is_empty(), "database.dsn is required");

        let pool = PgPoolOptions::new()
            .max_connections(10)
            .connect(&config.dsn)
            .await?;

        info!("database connected");

        // Run migrations. Uses Go's schema_migrations table for compatibility.
        migrate(&pool).await?;

        Ok(Arc::new(Self {
            messages: messages::Messages::new(pool.clone()),
            notifications: notifications::Notifications::new(pool.clone()),
            conversation: conversation::Conversation::new(pool.clone()),
            documents: documents::Documents::new(pool),
        }))
    }
}

/// Run SQL migrations from internal/store/migrations/, tracking applied
/// versions in the `schema_migrations` table (compatible with Go migrator).
async fn migrate(pool: &PgPool) -> anyhow::Result<()> {
    // Ensure tracking table exists.
    sqlx::query(
        "CREATE TABLE IF NOT EXISTS schema_migrations (\
            version TEXT PRIMARY KEY, \
            applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
    )
    .execute(pool)
    .await?;

    // Read migration files (embedded at compile time).
    let mut migrations: Vec<(&str, &str)> = MIGRATIONS.to_vec();
    migrations.sort_by_key(|(name, _)| *name);

    for (name, sql) in migrations {
        let applied: bool =
            sqlx::query("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)")
                .bind(name)
                .fetch_one(pool)
                .await?
                .get(0);

        if applied {
            continue;
        }

        info!(migration = name, "applying migration");

        let mut tx = pool.begin().await?;
        // raw_sql supports multiple statements in one call (unlike query()).
        sqlx::raw_sql(sql).execute(&mut *tx).await?;
        sqlx::query("INSERT INTO schema_migrations (version) VALUES ($1)")
            .bind(name)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;

        info!(migration = name, "migration applied");
    }

    Ok(())
}

/// Embedded migration files. Must match internal/store/migrations/*.sql.
const MIGRATIONS: &[(&str, &str)] = &[
    (
        "001_init.sql",
        include_str!("../../internal/store/migrations/001_init.sql"),
    ),
    (
        "002_memory_system.sql",
        include_str!("../../internal/store/migrations/002_memory_system.sql"),
    ),
    (
        "003_message_queue.sql",
        include_str!("../../internal/store/migrations/003_message_queue.sql"),
    ),
    (
        "004_traces.sql",
        include_str!("../../internal/store/migrations/004_traces.sql"),
    ),
    (
        "005_message_summary.sql",
        include_str!("../../internal/store/migrations/005_message_summary.sql"),
    ),
    (
        "006_reply_content.sql",
        include_str!("../../internal/store/migrations/006_reply_content.sql"),
    ),
    (
        "007_conversation_view.sql",
        include_str!("../../internal/store/migrations/007_conversation_view.sql"),
    ),
];
