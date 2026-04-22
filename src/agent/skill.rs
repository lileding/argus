//! Skill index: loads SKILL.md files from workspace/skills/*/SKILL.md.

use std::collections::HashMap;
use std::path::Path;

use tracing::{debug, info, warn};

use crate::config::SKILLS_DIR;

pub(crate) struct SkillEntry {
    pub(crate) name: String,
    pub(crate) description: String,
    pub(crate) prompt: String,
}

/// In-memory skill index loaded from disk at startup.
pub(crate) struct SkillIndex {
    entries: HashMap<String, SkillEntry>,
}

impl SkillIndex {
    /// Load all skills from `{workspace_dir}/skills/*/SKILL.md`.
    pub(crate) fn load(workspace_dir: &Path) -> Self {
        let skills_dir = workspace_dir.join(SKILLS_DIR);
        let mut entries = HashMap::new();

        let read_dir = match std::fs::read_dir(&skills_dir) {
            Ok(d) => d,
            Err(_) => {
                debug!(path = %skills_dir.display(), "skills directory not found, skipping");
                return Self { entries };
            }
        };

        for entry in read_dir.flatten() {
            if !entry.path().is_dir() {
                continue;
            }
            let skill_file = entry.path().join("SKILL.md");
            if !skill_file.exists() {
                continue;
            }
            match parse_skill_file(&skill_file, &entry.file_name().to_string_lossy()) {
                Ok(skill) => {
                    debug!(name = skill.name, "skill loaded");
                    entries.insert(skill.name.clone(), skill);
                }
                Err(e) => {
                    warn!(path = %skill_file.display(), error = %e, "failed to parse skill");
                }
            }
        }

        let names: Vec<&str> = entries.keys().map(|s| s.as_str()).collect();
        info!(count = entries.len(), ?names, "skills loaded");
        Self { entries }
    }

    pub(crate) fn get(&self, name: &str) -> Option<&SkillEntry> {
        self.entries.get(name)
    }

    /// Generate a catalog string for the orchestrator prompt.
    /// Lists all skills with their short descriptions.
    pub(crate) fn catalog(&self) -> String {
        if self.entries.is_empty() {
            return String::new();
        }
        let mut lines = vec!["## Available Skills".to_string()];
        let mut names: Vec<&String> = self.entries.keys().collect();
        names.sort();
        for name in names {
            let entry = &self.entries[name];
            let desc = if entry.description.len() > 250 {
                let t: String = entry.description.chars().take(250).collect();
                format!("{t}...")
            } else {
                entry.description.clone()
            };
            lines.push(format!("- **{name}**: {desc}"));
        }
        lines.push(
            "\nWhen the user's request relates to a skill, use the activate_skill tool to load its full instructions.".into()
        );
        lines.join("\n")
    }
}

/// Parse a SKILL.md file with optional YAML frontmatter.
fn parse_skill_file(path: &Path, dir_name: &str) -> Result<SkillEntry, String> {
    let content = std::fs::read_to_string(path).map_err(|e| format!("read: {e}"))?;

    let (name, description, prompt) = if let Some(after_prefix) = content.strip_prefix("---\n") {
        // Has YAML frontmatter.
        let end = after_prefix
            .find("\n---\n")
            .ok_or("unterminated YAML frontmatter")?;
        let yaml_str = &after_prefix[..end];
        let body = after_prefix[end + 5..].trim().to_string();

        let mut name = dir_name.to_string();
        let mut description = String::new();

        for line in yaml_str.lines() {
            let line = line.trim();
            if let Some(val) = line.strip_prefix("name:") {
                name = val.trim().trim_matches('"').to_string();
            } else if let Some(val) = line.strip_prefix("description:") {
                description = val.trim().trim_matches('"').to_string();
            }
        }

        // Fallback: use first paragraph as description.
        if description.is_empty() {
            description = body
                .lines()
                .find(|l| !l.is_empty() && !l.starts_with('#'))
                .unwrap_or("")
                .to_string();
        }

        (name, description, body)
    } else {
        // No frontmatter — use dir name and first paragraph.
        let description = content
            .lines()
            .find(|l| !l.is_empty() && !l.starts_with('#'))
            .unwrap_or("")
            .to_string();
        (dir_name.to_string(), description, content)
    };

    Ok(SkillEntry {
        name,
        description,
        prompt,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_with_frontmatter() {
        let dir = tempfile::tempdir().unwrap();
        let skill_dir = dir.path().join("test-skill");
        std::fs::create_dir(&skill_dir).unwrap();
        std::fs::write(
            skill_dir.join("SKILL.md"),
            "---\nname: my-skill\ndescription: A test skill\n---\n\n# Instructions\n\nDo things.",
        )
        .unwrap();
        let entry = parse_skill_file(&skill_dir.join("SKILL.md"), "test-skill").unwrap();
        assert_eq!(entry.name, "my-skill");
        assert_eq!(entry.description, "A test skill");
        assert!(entry.prompt.contains("Do things."));
    }

    #[test]
    fn parse_without_frontmatter() {
        let dir = tempfile::tempdir().unwrap();
        let skill_dir = dir.path().join("simple");
        std::fs::create_dir(&skill_dir).unwrap();
        std::fs::write(skill_dir.join("SKILL.md"), "# My Skill\n\nJust do it.").unwrap();
        let entry = parse_skill_file(&skill_dir.join("SKILL.md"), "simple").unwrap();
        assert_eq!(entry.name, "simple");
        assert_eq!(entry.description, "Just do it.");
    }

    #[test]
    fn load_from_workspace() {
        let dir = tempfile::tempdir().unwrap();
        let skills = dir.path().join("skills");
        let s1 = skills.join("alpha");
        std::fs::create_dir_all(&s1).unwrap();
        std::fs::write(s1.join("SKILL.md"), "Alpha skill instructions.").unwrap();
        let index = SkillIndex::load(dir.path());
        assert!(index.get("alpha").is_some());
        assert!(index.get("nonexistent").is_none());
    }

    #[test]
    fn catalog_format() {
        let dir = tempfile::tempdir().unwrap();
        let skills = dir.path().join("skills");
        let s1 = skills.join("cooking");
        std::fs::create_dir_all(&s1).unwrap();
        std::fs::write(
            s1.join("SKILL.md"),
            "---\nname: cooking\ndescription: Help with recipes\n---\nCook stuff.",
        )
        .unwrap();
        let index = SkillIndex::load(dir.path());
        let catalog = index.catalog();
        assert!(catalog.contains("**cooking**"));
        assert!(catalog.contains("Help with recipes"));
    }

    #[test]
    fn empty_skills_dir() {
        let dir = tempfile::tempdir().unwrap();
        let index = SkillIndex::load(dir.path());
        assert!(index.catalog().is_empty());
    }
}
