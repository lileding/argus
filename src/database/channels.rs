//! Channels table — the tenant unit of the channel system.
//!
//! Currently a stub: no CRUD is exposed yet because the application
//! always writes channel_id = NULL (default channel). Will gain
//! create/list/lookup operations once the channel config is wired up.

use sqlx::PgPool;

#[allow(dead_code)] // reserved for the upcoming channel configuration layer
pub(crate) struct Channels {
    pool: PgPool,
}

impl Channels {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }
}
