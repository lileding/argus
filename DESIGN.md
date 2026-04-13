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
   消息分类路由
        ↓
  ┌─────┴─────┐
  │           │
Gemma 4    Claude Code
(本地)      (cli skill)
  │
  └── 工具调用 ──→ 执行工具 ──→ 结果注入 ──→ 继续 loop
        ↓
    飞书发送回复
        ↓
    写入历史 DB
```

---

## 模型路由策略

| 任务类型 | 模型 |
|----------|------|
| 日常对话、闲聊、热量记录、查询 | 本地 Gemma 4（OpenAI API 格式） |
| 写代码、执行代码任务 | Claude Code（通过 cli skill 调用） |

**路由判断由 Gemma 4 本地完成**，不消耗 Claude API。  
Claude Code 使用订阅额度，不消耗 API token。

---

## Agent Loop

```
1. 收到消息
2. 构建 context（见记忆架构）
3. 组装：system prompt + 注入的记忆 + 当前消息 + 可用工具描述
4. 调用模型
5. 解析响应：
   - stop_reason == tool_use → 执行工具 → 注入结果 → 回到步骤 4
   - stop_reason == end_turn → 发送回复
6. 将本轮消息 + 回复存入 DB
```

---

## 记忆架构

**原则：不传递原始上下文，全量存储，按需召回注入。**

原始上下文线性增长，很快撞 context window，且充满噪音。正确方式：

```
所有消息全量写入 DB（永久保存）
            ↓
每次请求时构建注入内容：
  1. 近期原文   — 最近 N 条消息，保留细节
  2. 短期摘要   — 最近 1-2 小时的对话摘要
  3. 长期记忆   — 周期性生成的用户画像和重要事项摘要
  4. 语义召回   — pgvector 对当前消息做余弦相似度检索，拉取相关历史片段
            ↓
注入 context 大小稳定可控，同时"记得住一切"
```

**摘要生成策略：**
- 短期摘要：每次对话结束后异步生成，存 DB
- 长期记忆：每日定时任务生成，覆盖写入

**MVP 阶段：** 先用 fixed window（最近 20 条）跑通，记忆架构作为后续第一个优化项。

---

## 工具集（四个核心工具）

### 1. `file` — 文件读写
```
read_file(path) → content
write_file(path, content)
```
用途：持久化非结构化内容、报告、笔记

### 2. `cli` — Docker 内执行命令
```
run_cli(command, working_dir?) → stdout, stderr, exit_code
```
- 在 Docker 容器内执行，天然隔离
- 覆盖：运行 Python 脚本、抓取数据、任何计算任务
- Claude Code 也通过此工具启动：`claude --no-interactive -p "任务描述"`

### 3. `search` — 网页搜索
```
search_web(query) → results[]
```
用途：实时信息、新闻、知识查询

### 4. `db` — 结构化数据库
```
db_query(sql) → rows
db_exec(sql, params)
```
用途：热量记录、会话历史、任何结构化状态

---

## Skills 机制

Skills 是**可插拔的能力描述**，通过注入工具描述和 system prompt 片段实现。每个 skill 明确自己用哪些工具、怎么用。

当前规划的 skills：
- **热量记录 skill**：自然语言 → 解析食物热量 → 写 db
- **股票分析 skill**：Claude Code 写抓取脚本 → cli 执行 → 返回结果
- **coding skill**：调用 `cli` 启动 Claude Code，全自动无人值守

Skills 后期按需扩展，核心 loop 不变。

---

## 数据模型（PostgreSQL + pgvector）

```sql
CREATE EXTENSION IF NOT EXISTS vector;
```

### messages 表
```sql
CREATE TABLE messages (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    role        TEXT NOT NULL,        -- user / assistant / tool
    content     TEXT NOT NULL,
    tool_name   TEXT,
    tool_call_id TEXT,
    embedding   vector(768),          -- 消息向量，用于语义召回
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ON messages USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX ON messages (chat_id, created_at);
```

### summaries 表
```sql
CREATE TABLE summaries (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    type        TEXT NOT NULL,        -- short_term / long_term
    content     TEXT NOT NULL,
    covers_from TIMESTAMPTZ NOT NULL,
    covers_to   TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### food_log 表
```sql
CREATE TABLE food_log (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    description TEXT NOT NULL,
    calories    INTEGER,
    meal_type   TEXT,                 -- breakfast / lunch / dinner / snack
    logged_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

---

## 定时任务

独立 goroutine 跑 cron，不依赖用户触发：
- 每日定时：触发股票分析 skill，推送总结到飞书
- 后续按需扩展

---

## 技术选型

| 组件 | 选型 |
|------|------|
| 语言 | Go |
| IM | 飞书 Bot API |
| 主模型 | 本地 Gemma 4（OpenAI API 格式，LM Studio / Ollama） |
| 代码任务 | Claude Code（cli 调用） |
| 数据库 | PostgreSQL + pgvector |
| 沙箱 | Docker |
| 部署 | 单二进制 daemon |

---

## 开发顺序

1. **飞书 webhook 收发消息跑通**（echo bot 验证）
2. **基本 agent loop**：Gemma 4 + fixed window 历史
3. **四个工具最简实现**
4. **Claude Code skill**
5. **热量记录 skill**
6. **定时推送**
7. **记忆架构升级**：fixed window → 摘要 + 语义召回
8. **用起来，发现真需求，迭代**

---

## 约束与原则

- 没有"会话"概念，只有时间线
- 工具设计正交，不重叠
- MVP 先跑通，过度设计延后
- 核心 loop 稳定后 skills 才扩展
- 本地模型处理日常，Claude Code 处理编程，API 消耗趋近于零
