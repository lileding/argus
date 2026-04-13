//go:build unix

package skill

func builtinSkills() []*SkillEntry {
	return []*SkillEntry{
		{
			Name:        "posix-cli",
			Description: "Use POSIX command-line tools to process files, text, and data.",
			Tools:       []string{"cli"},
			Builtin:     true,
			Prompt: `## POSIX CLI Toolkit

You have access to standard POSIX command-line tools via the cli tool. Use these to efficiently process files, text, and data.

### IMPORTANT: Command Selection Rules
- NEVER use ` + "`ls -R`" + ` — it is extremely slow on large directories and will timeout. Use ` + "`find`" + ` instead.
- To locate a file, ALWAYS use ` + "`find <dir> -maxdepth <N> -name '<pattern>'`" + ` first. Start with the most specific directory and increase scope only if needed.
- To read file content, use ` + "`cat <absolute_path>`" + ` directly once you know the path. Don't waste iterations listing directories.
- Avoid exploratory commands on the home directory (` + "`ls ~/`" + `, ` + "`ls -R ~/`" + `, ` + "`du ~/`" + `). They are slow and produce too much output.
- When a command might produce large output, always pipe to ` + "`head -50`" + ` or ` + "`wc -l`" + ` first.

### File Discovery
- ` + "`find /path -maxdepth 3 -name '*.py'`" + ` — find files by pattern (always limit depth first)
- ` + "`find /path -maxdepth 2 -name '*keyword*' -type f`" + ` — find files containing keyword in name
- ` + "`find . -type f -mtime -1`" + ` — files modified in last 24h
- ` + "`ls -la`" + ` — list current directory only (never recursive)

### Text Search
- ` + "`grep -r 'pattern' .`" + ` — recursive search
- ` + "`grep -rl 'pattern' .`" + ` — list matching files only
- ` + "`grep -c 'pattern' file`" + ` — count matches
- ` + "`grep -n 'pattern' file`" + ` — show line numbers

### Text Transformation
- ` + "`sed 's/old/new/g' file`" + ` — find and replace
- ` + "`sed -n '10,20p' file`" + ` — extract line range
- ` + "`awk -F, '{print $1, $3}' file.csv`" + ` — extract columns
- ` + "`awk '{sum+=$1} END {print sum}'`" + ` — compute sum

### Sorting & Deduplication
- ` + "`sort file`" + ` — sort lines
- ` + "`sort -t, -k2 -n file.csv`" + ` — sort by numeric column
- ` + "`sort | uniq -c | sort -rn`" + ` — frequency count

### Data Slicing
- ` + "`head -20 file`" + ` — first 20 lines
- ` + "`tail -50 file`" + ` — last 50 lines
- ` + "`wc -l file`" + ` — count lines

### Pipelines
Chain commands with | for complex operations:
- ` + "`find . -name '*.log' | xargs grep 'ERROR' | wc -l`" + ` — count errors across logs
- ` + "`cat data.csv | awk -F, 'NR>1 {print $2}' | sort -u`" + ` — unique values from column
- ` + "`du -sh * | sort -rh | head -10`" + ` — top 10 largest items

### Network & Data
- ` + "`curl -s URL`" + ` — fetch URL content
- ` + "`curl -s URL | jq '.key'`" + ` — fetch JSON and extract field

### JSON Processing (jq)
- ` + "`jq '.' file.json`" + ` — pretty print
- ` + "`jq '.items[] | .name' file.json`" + ` — extract nested field
- ` + "`jq '.[] | select(.active)' file.json`" + ` — filter array

### Scripting
- ` + "`python3 -c 'import json; ...'`" + ` — inline Python for complex data processing
- ` + "`bash -c 'for f in *.txt; do echo $f; done'`" + ` — inline bash loops

### Best Practices
- ALWAYS use find with -maxdepth to locate files. NEVER ls -R.
- Pipe to head when output might be large.
- Use wc -l to check size before processing large files.
- Prefer grep -l over grep when you only need filenames.
- Use absolute paths once you've found a file, don't re-search.
- Use set -e in multi-line scripts to fail fast.`,
		},
	}
}
