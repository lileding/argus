package skill

// Built-in skills are compiled into the binary and always available.
// They are injected into the SkillIndex at startup, before loading user skills.
// User skills with the same name take precedence (override).

// BuiltinSkills returns all platform-appropriate built-in skills.
func BuiltinSkills() []*SkillEntry {
	return builtinSkills()
}
