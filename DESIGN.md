# Argus — 私人助理 Agent 设计文档

## 核心理念

这不是一个 chatbot，是一个**私人助理**。

助理只有一个人，一段记忆，一个上下文。用户不需要"新建会话"——就像你不会对着助理半张脸说记录吃饭，对着另半张脸说查账单。所有交互共享同一条时间线。

---

## 交互模式

### 私聊
- 完整助理模式
- 全量历史记忆
- 自由对话，无需任何命令

### 群/频道 @ 触发
- 专项任务模式
- 只响应 @ 提及
- 其他人的对话不介入

### 群/频道 静默监听
- 被动记录模式
- 默默记录群内提到的事项
- 适时主动反馈（如"你上周说要买的东西还没买"）

---

## 系统架构

```
飞书消息事件 (webhook)
        ↓
   Harness 上下文策展
   (skill 选择 + 历史策展 + prompt 组装)
        ↓
   Gemma 4 (本地 LLM)
        ↓
   工具调用 ──→ 执行工具 ──→ 结果注入 ──→ 继续 loop
        ↓
   飞书发送回复
        ↓
   写入历史 DB
```

---

## Harness Engineering（上下文策展）

**核心原则：上下文是稀缺有限资源，只放高信号内容。**

LLM 看到的不是原始对话，而是被精心策展的、具有约束性的内容：

```
用户消息到达
    ↓
[1] Skill 选择
    - 关键词预过滤：description 匹配用户消息
    - LLM 自选：system prompt 包含全部 skill 目录（name + description）
    - activate_skill 工具：LLM 按需加载未被预过滤命中的 skill
    ↓
[2] System Prompt 组装
    - 基础 prompt
    - Skill 目录（全部 skill 的 name + description 摘要）
    - 已选 skill 的完整指令
    - 技能积累指引
    ↓
[3] 历史策展
    - 加载近 N 条消息
    - 只保留 user 消息 + assistant 最终回复
    - 移除中间 tool_call / tool_result 噪音
    ↓
[4] 工具过滤
    - base 工具 + 选中 skill 所需工具
```

---

## 模型路由策略

所有消息由 Gemma 4 本地处理。路由通过 tool calling 隐式完成：当需要编程时，Gemma 4 通过 coding skill 调用 cli 工具启动 Claude Code。

| 任务类型 | 模型 |
|----------|------|
| 日常对话、闲聊、查询 | 本地 Gemma 4（OpenAI API 格式） |
| 写代码、执行代码任务 | Claude Code（通过 coding skill + cli 工具） |

---

## Agent Loop

```
1. 收到消息
2. Harness 策展上下文（skill 选择 + prompt 组装 + 历史过滤 + 工具过滤）
3. 调用模型
4. 解析响应：
   - tool_calls → 执行工具 → 注入结果 → 回到步骤 3
   - stop → 发送回复
5. 将用户消息 + 回复存入 DB
```

---

## Skills 机制

Skills 遵循 [Agent Skills 开放标准](https://agentskills.io)（Claude Code 同款 SKILL.md 格式），是**文件驱动的可插拔能力**。

### Skill 文件格式

每个 skill 是一个目录，包含 `SKILL.md` 入口文件：

```
workspace/.skills/
  coding/
    SKILL.md
  calorie/
    SKILL.md
    setup.sql
  stock-analysis/
    SKILL.md
    scripts/
      fetch.py
```

SKILL.md 使用 YAML frontmatter + Markdown body：

```yaml
---
name: calorie
description: "记录日常饮食热量和卡路里消耗。当用户提到吃了什么、喝了什么、问今日热量时使用。"
tools:
  - db
  - db_exec
---

## 热量记录

当用户提到吃了什么...（完整指令）
```

### Skill 生命周期

1. **种子 Skill**：首次启动时自动复制内建 seed（如 coding）
2. **动态创建**：agent 通过 `save_skill` 工具在对话中创建新 skill
3. **自动加载**：启动时扫描 `.skills/` 目录，后台定期重新扫描
4. **按需激活**：keyword 预过滤 + LLM 自选（`activate_skill` 工具）

### 技能积累

成功完成新类型任务后，agent 将经验沉淀为 SKILL.md 文件。这是 Harness Engineering 的核心——通过不断积累 skills，agent 的能力持续增长。

---

## 工具集

### 核心工具（始终可用）
| 工具 | 用途 |
|------|------|
| `file` | 工作区文件读写（路径严格限定） |
| `save_skill` | 创建/更新 SKILL.md 文件 |
| `activate_skill` | 按名称加载 skill 完整指令 |

### 按需工具（由 skill 声明）
| 工具 | 用途 |
|------|------|
| `cli` | Docker 沙箱内执行命令 |
| `search` | 网页搜索 |
| `db` | 只读 SQL 查询 |
| `db_exec` | 可写 SQL（INSERT/UPDATE/CREATE TABLE） |

---

## 记忆架构

**MVP 阶段：** fixed window + 历史策展（只保留 user/assistant 最终回复，过滤 tool 噪音）。

**后续优化：**
- 短期摘要：对话结束后异步生成
- 长期记忆：每日定时生成用户画像
- 语义召回：pgvector 余弦相似度检索

---

## 数据模型（PostgreSQL）

### messages 表
```sql
CREATE TABLE messages (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    role        TEXT NOT NULL,
    content     TEXT NOT NULL,
    tool_name   TEXT,
    tool_call_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX ON messages (chat_id, created_at);
```

其他业务表（如 food_log）由 agent 通过 `db_exec` 工具动态创建，不作为核心 schema。

---

## 定时任务

独立 goroutine 跑 cron，不依赖用户触发：
- 通过配置文件定义定时任务（prompt + 目标 chat_id）
- 定时执行 agent.Handle()，结果通过飞书主动推送

---

## 技术选型

| 组件 | 选型 |
|------|------|
| 语言 | Go |
| IM | 飞书 Bot API |
| 主模型 | 本地 Gemma 4（OpenAI API 格式，LM Studio / Ollama） |
| 代码任务 | Claude Code（通过 coding skill + cli 工具） |
| 数据库 | PostgreSQL |
| 沙箱 | Docker |
| Skills | SKILL.md 文件（Agent Skills 开放标准） |
| 部署 | 单二进制 daemon |

---

## 约束与原则

- 没有"会话"概念，只有时间线
- 上下文是稀缺资源，只放高信号内容（Harness Engineering）
- 工具设计正交，不重叠
- Skills 由 agent 动态创建和积累，不硬编码
- 核心 loop 稳定后 skills 自然增长
- 本地模型处理日常，Claude Code 处理编程，API 消耗趋近于零
