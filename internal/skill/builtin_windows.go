//go:build windows

package skill

func builtinSkills() []*SkillEntry {
	return []*SkillEntry{
		{
			Name:        "powershell-cli",
			Description: "Use PowerShell commands to process files, text, and data. Covers Get-ChildItem, Select-String, Sort-Object, ConvertFrom-Json, and pipelines.",
			Tools:       []string{"cli"},
			Prompt: `## PowerShell CLI Toolkit

You have access to PowerShell commands via the cli tool.

### File Discovery
- Get-ChildItem -Recurse -Filter *.py
- Get-ChildItem | Sort-Object LastWriteTime -Descending

### Text Search
- Select-String -Pattern 'keyword' -Path *.txt -Recurse
- Get-Content file.txt | Select-String 'pattern'

### Text Processing
- Get-Content file.txt | Sort-Object | Get-Unique
- Import-Csv data.csv | Where-Object { $_.Status -eq 'active' }
- Import-Csv data.csv | Measure-Object -Property Amount -Sum

### JSON Processing
- Get-Content file.json | ConvertFrom-Json
- Invoke-RestMethod -Uri URL

### Best Practices
- Use Select-Object -First 20 to limit output
- Use Measure-Object for counting and aggregation`,
		},
	}
}
