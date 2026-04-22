//! Document indexing: extract text from files, chunk, save to DB.

use std::path::Path;

use tracing::{info, warn};

use crate::config::MEDIA_DIR;
use crate::database::Database;

/// Max file size for inline processing (during upload). Larger files are queued.
const INLINE_SIZE_LIMIT: u64 = 1_000_000; // 1 MB

/// Chunk size in chars.
const CHUNK_SIZE: usize = 1500;
/// Overlap between chunks in chars.
const CHUNK_OVERLAP: usize = 300;

/// Process a file upload: extract text, chunk, save to DB.
/// Small files (≤ 1MB) are processed inline. Large files are saved as pending.
pub(crate) async fn process_upload(
    db: &Database,
    workspace_dir: &Path,
    filename: &str,
    media_filename: &str, // filename in MEDIA_DIR (without prefix)
) {
    let abs_path = workspace_dir.join(MEDIA_DIR).join(media_filename);

    // Save document record.
    let doc_id = match db
        .documents
        .save_document(filename, abs_path.to_str().unwrap_or(""))
        .await
    {
        Ok(id) => id,
        Err(e) => {
            warn!(filename, error = %e, "save_document failed");
            return;
        }
    };

    // Check file size — large files go to background worker.
    let file_size = tokio::fs::metadata(&abs_path)
        .await
        .map(|m| m.len())
        .unwrap_or(0);

    if file_size > INLINE_SIZE_LIMIT {
        info!(
            filename,
            file_size, doc_id, "large file queued for background processing"
        );
        return; // stays as 'pending', worker picks it up
    }

    // Small file: process inline.
    if let Err(e) = ingest_document(db, doc_id, filename, &abs_path).await {
        warn!(filename, doc_id, error = %e, "inline document ingestion failed");
        let _ = db
            .documents
            .update_status(doc_id, "error", &e.to_string())
            .await;
    }
}

/// Ingest a document: extract text → chunk → save chunks → mark ready.
/// Used both inline (small files) and by the background worker (large files).
pub(super) async fn ingest_document(
    db: &Database,
    doc_id: i64,
    filename: &str,
    abs_path: &Path,
) -> anyhow::Result<()> {
    db.documents.update_status(doc_id, "processing", "").await?;

    let text = extract_text(filename, abs_path).await?;
    let trimmed = text.trim();
    if trimmed.is_empty() {
        anyhow::bail!("no text content extracted");
    }

    let chunks = chunk_text(trimmed, CHUNK_SIZE, CHUNK_OVERLAP);
    db.documents.save_chunks(doc_id, &chunks).await?;
    db.documents.update_status(doc_id, "ready", "").await?;

    info!(filename, doc_id, chunks = chunks.len(), "document indexed");
    Ok(())
}

/// Extract text from a file based on extension.
async fn extract_text(filename: &str, path: &Path) -> anyhow::Result<String> {
    let lower = filename.to_lowercase();
    let path_str = path.to_str().unwrap_or("");

    if lower.ends_with(".pdf") {
        run_command("pdftotext", &[path_str, "-"]).await
    } else if lower.ends_with(".docx") {
        let script = r#"import sys; from docx import Document; d=Document(sys.argv[1]); print("\n".join(p.text for p in d.paragraphs))"#;
        run_command("python3", &["-c", script, path_str]).await
    } else {
        // txt, md, csv, json, yaml, xml, log, etc.
        Ok(tokio::fs::read_to_string(path).await?)
    }
}

/// Run a shell command and return its stdout.
async fn run_command(program: &str, args: &[&str]) -> anyhow::Result<String> {
    let output = tokio::process::Command::new(program)
        .args(args)
        .output()
        .await?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        anyhow::bail!("{program} failed: {stderr}");
    }

    Ok(String::from_utf8_lossy(&output.stdout).to_string())
}

/// Split text into overlapping chunks.
fn chunk_text(text: &str, chunk_size: usize, overlap: usize) -> Vec<String> {
    let chars: Vec<char> = text.chars().collect();
    if chars.len() <= chunk_size {
        return vec![text.to_string()];
    }

    let mut chunks = Vec::new();
    let mut start = 0;
    while start < chars.len() {
        let end = (start + chunk_size).min(chars.len());
        let chunk: String = chars[start..end].iter().collect();
        let trimmed = chunk.trim();
        if !trimmed.is_empty() {
            chunks.push(trimmed.to_string());
        }
        if end >= chars.len() {
            break;
        }
        start += chunk_size - overlap;
    }

    chunks
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn chunk_short_text() {
        let chunks = chunk_text("hello world", 100, 20);
        assert_eq!(chunks, vec!["hello world"]);
    }

    #[test]
    fn chunk_splits_with_overlap() {
        let text = "a".repeat(3000);
        let chunks = chunk_text(&text, 1500, 300);
        assert_eq!(chunks.len(), 3); // 0-1500, 1200-2700, 2400-3000
        assert_eq!(chunks[0].len(), 1500);
        assert_eq!(chunks[1].len(), 1500);
    }

    #[test]
    fn chunk_exact_boundary() {
        let text = "a".repeat(1500);
        let chunks = chunk_text(&text, 1500, 300);
        assert_eq!(chunks.len(), 1);
    }
}
