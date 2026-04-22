use std::time::Instant;

use sqlx::{PgPool, Row};

use crate::upstream::types::Usage;

/// Trace persistence for orchestrator runs.
pub(crate) struct Traces {
    pool: PgPool,
}

impl Traces {
    pub(super) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Access the underlying connection pool.
    pub(crate) fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// Begin a new trace. Inserts a row immediately so tool_calls can reference it.
    pub(crate) async fn begin(
        &self,
        message_id: i64,
        chat_id: &str,
        orch_model: &str,
        synth_model: &str,
    ) -> super::DbResult<TraceBuilder> {
        let row = sqlx::query(
            "INSERT INTO traces (message_id, chat_id, orchestrator_model, synthesizer_model) \
             VALUES ($1, $2, $3, $4) RETURNING id",
        )
        .bind(message_id)
        .bind(chat_id)
        .bind(orch_model)
        .bind(synth_model)
        .fetch_one(&self.pool)
        .await?;

        Ok(TraceBuilder {
            trace_id: row.get("id"),
            pool: self.pool.clone(),
            start: Instant::now(),
            orch_prompt_tokens: 0,
            orch_completion_tokens: 0,
        })
    }
}

/// Builder for a single trace. Created at the start of the orchestrator loop,
/// finalized after synthesis.
pub(crate) struct TraceBuilder {
    pub(crate) trace_id: i64,
    pool: PgPool,
    start: Instant,
    orch_prompt_tokens: u32,
    orch_completion_tokens: u32,
}

impl TraceBuilder {
    /// Accumulate orchestrator token usage.
    pub(crate) fn add_usage(&mut self, usage: &Usage) {
        self.orch_prompt_tokens += usage.prompt_tokens;
        self.orch_completion_tokens += usage.completion_tokens;
    }

    /// Record a single tool call execution.
    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn record_tool_call(
        &self,
        iteration: i32,
        seq: i32,
        tool_name: &str,
        arguments: &str,
        normalized_args: &str,
        result: &str,
        is_error: bool,
        duration_ms: i32,
    ) -> super::DbResult<()> {
        sqlx::query(
            "INSERT INTO tool_calls \
             (trace_id, iteration, seq, tool_name, arguments, normalized_args, result, is_error, duration_ms) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
        )
        .bind(self.trace_id)
        .bind(iteration)
        .bind(seq)
        .bind(tool_name)
        .bind(arguments)
        .bind(normalized_args)
        .bind(result)
        .bind(is_error)
        .bind(duration_ms)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// Finalize the trace with summary stats.
    pub(crate) async fn finalize(
        self,
        iterations: i32,
        summary: &str,
        reply_id: Option<i64>,
        synth_prompt_tokens: u32,
        synth_completion_tokens: u32,
    ) -> super::DbResult<()> {
        let duration_ms = self.start.elapsed().as_millis() as i32;
        sqlx::query(
            "UPDATE traces SET \
             iterations = $1, summary = $2, reply_id = $3, \
             total_prompt_tokens = $4, total_completion_tokens = $5, \
             synth_prompt_tokens = $6, synth_completion_tokens = $7, \
             duration_ms = $8 \
             WHERE id = $9",
        )
        .bind(iterations)
        .bind(summary)
        .bind(reply_id)
        .bind((self.orch_prompt_tokens + synth_prompt_tokens) as i32)
        .bind((self.orch_completion_tokens + synth_completion_tokens) as i32)
        .bind(synth_prompt_tokens as i32)
        .bind(synth_completion_tokens as i32)
        .bind(duration_ms)
        .bind(self.trace_id)
        .execute(&self.pool)
        .await?;
        Ok(())
    }
}
