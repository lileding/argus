package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSkillFile(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "test-skill")
	os.MkdirAll(skillDir, 0755)

	content := `---
name: test-skill
description: "A test skill for unit testing"
tools:
  - db
  - file
disable-model-invocation: false
---

## Test Instructions

Do something useful.
`
	path := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(path, []byte(content), 0644)

	entry, err := parseSkillFile(path, "test-skill")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if entry.Name != "test-skill" {
		t.Errorf("name = %q, want %q", entry.Name, "test-skill")
	}
	if entry.Description != "A test skill for unit testing" {
		t.Errorf("description = %q", entry.Description)
	}
	if len(entry.Tools) != 2 || entry.Tools[0] != "db" || entry.Tools[1] != "file" {
		t.Errorf("tools = %v", entry.Tools)
	}
	if entry.DisableModelInvocation {
		t.Error("expected disable-model-invocation = false")
	}
	if entry.Prompt != "## Test Instructions\n\nDo something useful." {
		t.Errorf("prompt = %q", entry.Prompt)
	}
}

func TestParseSkillFile_FallbackName(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	os.MkdirAll(skillDir, 0755)

	content := `---
description: "No name field"
---

Instructions here.
`
	path := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(path, []byte(content), 0644)

	entry, err := parseSkillFile(path, "my-skill")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if entry.Name != "my-skill" {
		t.Errorf("name = %q, want %q (fallback to dir name)", entry.Name, "my-skill")
	}
}

func TestFileLoader_LoadAll_IncludesBuiltins(t *testing.T) {
	dir := t.TempDir()
	loader := NewFileLoader(dir, time.Hour)
	if err := loader.LoadAll(); err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	// Built-in skills should be present even with empty skills dir.
	builtins := BuiltinSkills()
	if len(builtins) == 0 {
		t.Skip("no built-in skills on this platform")
	}

	for _, b := range builtins {
		if _, ok := loader.Index().Get(b.Name); !ok {
			t.Errorf("built-in skill %q not in index", b.Name)
		}
	}
}

func TestFileLoader_UserOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	builtins := BuiltinSkills()
	if len(builtins) == 0 {
		t.Skip("no built-in skills on this platform")
	}

	// Create a user skill with the same name as a builtin.
	name := builtins[0].Name
	skillDir := filepath.Join(dir, name)
	os.MkdirAll(skillDir, 0755)
	content := "---\nname: " + name + "\ndescription: \"User override\"\n---\n\nCustom instructions.\n"
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)

	loader := NewFileLoader(dir, time.Hour)
	loader.LoadAll()

	entry, ok := loader.Index().Get(name)
	if !ok {
		t.Fatalf("skill %q not found", name)
	}
	if entry.Description != "User override" {
		t.Errorf("expected user override, got description=%q", entry.Description)
	}
}

func TestFileLoader_LoadAll_UserSkills(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"alpha", "beta"} {
		skillDir := filepath.Join(dir, name)
		os.MkdirAll(skillDir, 0755)
		content := "---\nname: " + name + "\ndescription: \"Skill " + name + "\"\n---\n\nInstructions for " + name + ".\n"
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)
	}

	loader := NewFileLoader(dir, time.Hour)
	loader.LoadAll()

	for _, name := range []string{"alpha", "beta"} {
		if _, ok := loader.Index().Get(name); !ok {
			t.Errorf("user skill %q not loaded", name)
		}
	}
}

func TestSkillIndex_Catalog(t *testing.T) {
	idx := NewSkillIndex()
	idx.Put(&SkillEntry{
		Name:        "calorie",
		Description: "记录日常饮食热量",
		Prompt:      "...",
	})
	idx.Put(&SkillEntry{
		Name:                   "hidden",
		Description:            "Should not appear",
		DisableModelInvocation: true,
		Prompt:                 "...",
	})

	catalog := idx.Catalog()
	if !strings.Contains(catalog, "calorie") {
		t.Error("catalog should contain calorie skill")
	}
	if strings.Contains(catalog, "hidden") {
		t.Error("catalog should not contain hidden skill")
	}
}
