//go:build unix

package skill

func builtinSkills() []*SkillEntry {
	return []*SkillEntry{
		{
			Name:        "posix-cli",
			Description: "Use POSIX command-line tools to process files, text, and data. Covers grep, find, awk, sed, sort, xargs, head, tail, wc, curl, jq, and pipelines. Use when the user asks to search, transform, analyze, or process files and data.",
			Tools:       []string{"cli"},
			Prompt: `## POSIX CLI Toolkit

You have access to standard POSIX command-line tools via the cli tool. Use these to efficiently process files, text, and data.

### File Discovery
- ` + "`find . -name '*.py'`" + ` — find files by pattern
- ` + "`find . -type f -mtime -1`" + ` — files modified in last 24h
- ` + "`ls -la`" + ` — list with details

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
- ` + "`jq '[.[] | select(.status == \"active\")]'`" + ` — filter array

### Best Practices
- Always pipe to head when output might be large
- Use wc -l to check size before processing
- Prefer grep -l over grep when you only need filenames
- Use set -e in multi-line scripts to fail fast`,
		},
	}
}
