package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"argus/internal/config"
)

// localCLI creates a CLITool in local mode for testing.
func localCLI(t *testing.T) (*CLITool, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DockerConfig{
		Image:   "local",
		Timeout: 10 * time.Second,
	}
	return NewCLITool(cfg, dir), dir
}

func call(t *testing.T, cli *CLITool, command string) string {
	t.Helper()
	args, _ := json.Marshal(cliArgs{Command: command})
	result, err := cli.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("execute %q: %v", command, err)
	}
	return result
}

// --- Test: grep searches file content ---
// Scenario: model needs to find which file contains a keyword.
func TestCLI_Grep(t *testing.T) {
	cli, dir := localCLI(t)

	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("buy milk\ncall dentist\npay rent\n"), 0644)
	os.WriteFile(filepath.Join(dir, "todo.txt"), []byte("fix bike\nbuy milk\nread book\n"), 0644)

	// grep -rl: find files containing "dentist"
	result := call(t, cli, "grep -rl 'dentist' .")
	if !strings.Contains(result, "notes.txt") {
		t.Errorf("expected notes.txt in grep result, got: %s", result)
	}

	// grep -c: count occurrences of "buy milk"
	result = call(t, cli, "grep -c 'buy milk' notes.txt todo.txt")
	if !strings.Contains(result, "1") {
		t.Errorf("expected count of 1, got: %s", result)
	}
}

// --- Test: find locates files by pattern ---
// Scenario: model needs to discover files matching a glob.
func TestCLI_Find(t *testing.T) {
	cli, dir := localCLI(t)

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.py"), []byte("print('hello')"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "utils.py"), []byte("def add(a,b): return a+b"), 0644)
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# Project"), 0644)

	result := call(t, cli, "find . -name '*.py' | sort")
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 .py files, got %d: %s", len(lines), result)
	}
}

// --- Test: wc counts lines/words ---
// Scenario: model needs to report file statistics.
func TestCLI_Wc(t *testing.T) {
	cli, dir := localCLI(t)

	os.WriteFile(filepath.Join(dir, "data.csv"), []byte("a,b,c\n1,2,3\n4,5,6\n7,8,9\n"), 0644)

	result := call(t, cli, "wc -l data.csv")
	if !strings.Contains(result, "4") {
		t.Errorf("expected 4 lines, got: %s", result)
	}
}

// --- Test: sed transforms text ---
// Scenario: model does a find-and-replace in a file.
func TestCLI_Sed(t *testing.T) {
	cli, dir := localCLI(t)

	os.WriteFile(filepath.Join(dir, "config.txt"), []byte("host=localhost\nport=3000\nhost=localhost\n"), 0644)

	call(t, cli, "sed -i '' 's/localhost/0.0.0.0/g' config.txt 2>/dev/null || sed -i 's/localhost/0.0.0.0/g' config.txt")

	data, _ := os.ReadFile(filepath.Join(dir, "config.txt"))
	if !strings.Contains(string(data), "0.0.0.0") {
		t.Errorf("sed replacement failed, content: %s", data)
	}
	if strings.Contains(string(data), "localhost") {
		t.Errorf("sed should have replaced all localhost, content: %s", data)
	}
}

// --- Test: awk extracts columns ---
// Scenario: model parses structured data.
func TestCLI_Awk(t *testing.T) {
	cli, dir := localCLI(t)

	os.WriteFile(filepath.Join(dir, "sales.csv"), []byte("product,qty,price\napple,10,1.5\nbanana,20,0.8\norange,15,2.0\n"), 0644)

	// Extract product names (column 1)
	result := call(t, cli, "awk -F, 'NR>1 {print $1}' sales.csv")
	if !strings.Contains(result, "apple") || !strings.Contains(result, "banana") {
		t.Errorf("awk column extraction failed: %s", result)
	}

	// Sum qty column
	result = call(t, cli, "awk -F, 'NR>1 {s+=$2} END {print s}' sales.csv")
	if strings.TrimSpace(result) != "45" {
		t.Errorf("expected sum 45, got: %s", result)
	}
}

// --- Test: sort + uniq deduplicates ---
// Scenario: model needs unique values from a list.
func TestCLI_SortUniq(t *testing.T) {
	cli, dir := localCLI(t)

	os.WriteFile(filepath.Join(dir, "tags.txt"), []byte("go\npython\ngo\nrust\npython\ngo\n"), 0644)

	result := call(t, cli, "sort tags.txt | uniq -c | sort -rn")
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 unique tags, got %d: %s", len(lines), result)
	}
	// "go" should be first (count 3)
	if !strings.Contains(lines[0], "go") {
		t.Errorf("expected 'go' as most frequent, got: %s", lines[0])
	}
}

// --- Test: pipeline combines multiple commands ---
// Scenario: model chains commands to answer a complex question.
// "How many unique file extensions are in the project?"
func TestCLI_Pipeline(t *testing.T) {
	cli, dir := localCLI(t)

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	for _, name := range []string{"main.go", "util.go", "test.py", "index.js", "style.css", "app.js"} {
		os.WriteFile(filepath.Join(dir, "src", name), []byte("content"), 0644)
	}

	result := call(t, cli, "find . -type f -name '*.*' | sed 's/.*\\.//' | sort -u")
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 4 { // go, py, js, css
		t.Errorf("expected 4 unique extensions, got %d: %v", len(lines), lines)
	}
}

// --- Test: xargs processes multiple inputs ---
// Scenario: model needs to batch-process files.
func TestCLI_Xargs(t *testing.T) {
	cli, dir := localCLI(t)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		os.WriteFile(filepath.Join(dir, name), []byte("hello from "+name+"\n"), 0644)
	}

	result := call(t, cli, "ls *.txt | xargs grep -l 'hello'")
	if !strings.Contains(result, "a.txt") || !strings.Contains(result, "c.txt") {
		t.Errorf("xargs grep failed: %s", result)
	}
}

// --- Test: head/tail extracts portions ---
// Scenario: model reads the first/last N lines of a log.
func TestCLI_HeadTail(t *testing.T) {
	cli, dir := localCLI(t)

	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, strings.Repeat("x", i))
	}
	os.WriteFile(filepath.Join(dir, "log.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0644)

	result := call(t, cli, "head -3 log.txt | wc -l")
	if strings.TrimSpace(result) != "3" {
		t.Errorf("head -3 should give 3 lines, got: %s", result)
	}

	result = call(t, cli, "tail -5 log.txt | wc -l")
	if strings.TrimSpace(result) != "5" {
		t.Errorf("tail -5 should give 5 lines, got: %s", result)
	}
}

// --- Test: date command works ---
// Scenario: model checks current time (complementing the injected time).
func TestCLI_Date(t *testing.T) {
	cli, _ := localCLI(t)

	result := call(t, cli, "date '+%Y-%m-%d'")
	if len(result) != 10 { // "2026-04-13"
		t.Errorf("unexpected date format: %s", result)
	}
}

// --- Test: command timeout ---
func TestCLI_Timeout(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DockerConfig{
		Image:   "local",
		Timeout: 1 * time.Second,
	}
	cli := NewCLITool(cfg, dir)

	_, err := cli.Execute(context.Background(), `{"command":"sleep 10"}`)
	if err == nil {
		t.Error("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout message, got: %v", err)
	}
}
