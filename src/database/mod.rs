mod conversation;
mod documents;
mod memories;
pub(crate) mod messages;
mod notifications;
pub(crate) mod traces;

use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Row};
use tracing::info;

use crate::config::DatabaseConfig;

#[derive(Debug, thiserror::Error)]
pub(crate) enum DatabaseError {
    #[error(transparent)]
    Sql(#[from] sqlx::Error),
    #[error("{0}")]
    InvalidState(String),
}

pub(crate) type DbResult<T> = Result<T, DatabaseError>;

pub(crate) use documents::Documents;
pub(crate) use memories::Memories;
pub(crate) use messages::{InboundMessage, Messages};
pub(crate) use notifications::Notifications;

/// Database handle. Sub-objects group operations by table/feature.
/// PgPool is internally Arc'd — clone is zero-cost.
pub(crate) struct Database {
    pub(crate) messages: messages::Messages,
    pub(crate) notifications: notifications::Notifications,
    pub(crate) conversation: conversation::Conversation,
    pub(crate) documents: documents::Documents,
    pub(crate) memories: memories::Memories,
    pub(crate) traces: traces::Traces,
}

impl Database {
    /// Access the underlying connection pool for raw SQL queries.
    pub(crate) fn pool(&self) -> &PgPool {
        // All sub-objects share the same underlying pool (PgPool is Arc-based).
        self.traces.pool()
    }

    pub(crate) async fn connect(config: &DatabaseConfig) -> DbResult<Self> {
        if config.dsn.is_empty() {
            return Err(DatabaseError::InvalidState(
                "database.dsn is required".into(),
            ));
        };

        let pool = PgPoolOptions::new()
            .max_connections(10)
            .connect(&config.dsn)
            .await?;

        info!("database connected");

        // Run migrations. Uses Go's schema_migrations table for compatibility.
        migrate(&pool).await?;

        Ok(Self {
            messages: messages::Messages::new(pool.clone()),
            notifications: notifications::Notifications::new(pool.clone()),
            conversation: conversation::Conversation::new(pool.clone()),
            documents: documents::Documents::new(pool.clone()),
            memories: memories::Memories::new(pool.clone()),
            traces: traces::Traces::new(pool),
        })
    }
}

/// Run SQL migrations from internal/store/migrations/, tracking applied
/// versions in the `schema_migrations` table (compatible with Go migrator).
async fn migrate(pool: &PgPool) -> DbResult<()> {
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

/// Embedded migration files from migrations/*.sql.
const MIGRATIONS: &[(&str, &str)] = &[
    (
        "001_init.sql",
        include_str!("../../migrations/001_init.sql"),
    ),
    (
        "002_memory_system.sql",
        include_str!("../../migrations/002_memory_system.sql"),
    ),
    (
        "003_message_queue.sql",
        include_str!("../../migrations/003_message_queue.sql"),
    ),
    (
        "004_traces.sql",
        include_str!("../../migrations/004_traces.sql"),
    ),
    (
        "005_message_summary.sql",
        include_str!("../../migrations/005_message_summary.sql"),
    ),
    (
        "006_reply_content.sql",
        include_str!("../../migrations/006_reply_content.sql"),
    ),
    (
        "007_conversation_view.sql",
        include_str!("../../migrations/007_conversation_view.sql"),
    ),
];
