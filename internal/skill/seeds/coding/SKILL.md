---
name: coding
description: "编程任务。当用户要求写代码、调试、代码审查、技术方案、运行脚本时使用。"
tools:
  - cli
  - file
---

## Coding Skill

当用户请求编程相关任务（写代码、调试、代码审查、技术方案等），你应该使用 cli 工具调用 Claude Code 来完成。

调用方式：
- 使用 cli 工具执行: claude -p "任务描述" --no-interactive
- 工作目录设置为 /workspace
- Claude Code 会在 /workspace 目录下工作

使用场景：
- 用户要求写代码、脚本
- 用户要求分析或调试代码
- 用户要求执行复杂的技术任务
- 用户要求代码审查或重构

注意事项：
- 将用户的需求完整、准确地传递给 Claude Code
- 如果任务涉及特定文件，先用 file 工具确认文件存在
- Claude Code 的输出可能很长，需要提取关键信息回复用户
