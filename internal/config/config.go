package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig                 `yaml:"server"`
	Feishu    FeishuConfig                 `yaml:"feishu"`
	Upstreams map[string]UpstreamConfig    `yaml:"upstreams"` // top-level upstream definitions
	Model     ModelConfig                  `yaml:"model"`
	Database  DatabaseConfig               `yaml:"database"`
	Agent     AgentConfig                  `yaml:"agent"`
	Sandbox   SandboxConfig                `yaml:"sandbox"`
	Embedding EmbeddingConfig              `yaml:"embedding"`
	Search    SearchConfig                 `yaml:"search"`
	Cron      CronConfig                   `yaml:"cron"`
}

type SearchConfig struct {
	Provider      string `yaml:"provider"`        // "tavily" or "duckduckgo" (default: auto-detect)
	TavilyAPIKey  string `yaml:"tavily_api_key"`
	MaxResults    int    `yaml:"max_results"`
	IncludeAnswer bool   `yaml:"include_answer"`  // Tavily: include pre-generated answer
}

type EmbeddingConfig struct {
	Enabled   bool          `yaml:"enabled"`
	ModelName string        `yaml:"model_name"`
	BatchSize int           `yaml:"batch_size"`
	Interval  time.Duration `yaml:"interval"`
}

type CronConfig struct {
	Jobs []CronJobConfig `yaml:"jobs"`
}

type CronJobConfig struct {
	Name    string `yaml:"name"`
	Hour    int    `yaml:"hour"`
	Minute  int    `yaml:"minute"`
	ChatID  string `yaml:"chat_id"` // target chat to send results
	Prompt  string `yaml:"prompt"`  // prompt to send to the agent
}

type ServerConfig struct {
	Port string `yaml:"port"`
}

type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
}

// UpstreamConfig defines a model provider backend.
type UpstreamConfig struct {
	Type     string        `yaml:"type"`      // "openai" or "vertex_ai"
	BaseURL  string        `yaml:"base_url"`  // openai only
	APIKey   string        `yaml:"api_key"`   // openai / anthropic direct
	Project  string        `yaml:"project"`   // vertex_ai only
	Location string        `yaml:"location"`  // vertex_ai only
	Timeout  time.Duration `yaml:"timeout"`
}

// RoleConfig selects an upstream + model for a specific agent role.
type RoleConfig struct {
	Upstream  string `yaml:"upstream"`   // key into Upstreams map
	ModelName string `yaml:"model_name"`
	MaxTokens int    `yaml:"max_tokens"`
}

// ModelConfig holds per-role model selection.
// Upstreams are defined at Config level, not here.
type ModelConfig struct {
	Orchestrator RoleConfig                `yaml:"orchestrator"`
	Synthesizer  RoleConfig                `yaml:"synthesizer"`
	Fallback     RoleConfig                `yaml:"fallback"`
	Transcription RoleConfig               `yaml:"transcription"`

	// Legacy fields (still supported for backward compatibility)
	BaseURL            string        `yaml:"base_url"`
	ModelName          string        `yaml:"model_name"`
	APIKey             string        `yaml:"api_key"`
	MaxTokens          int           `yaml:"max_tokens"`
	MaxReplyTokens     int           `yaml:"max_reply_tokens"`
	Timeout            time.Duration `yaml:"timeout"`
	TranscriptionModel string        `yaml:"transcription_model"`
}

type DatabaseConfig struct {
	DSN string `yaml:"dsn"`
}

type AgentConfig struct {
	MaxIterations int           `yaml:"max_iterations"`
	ContextWindow int           `yaml:"context_window"`
	SystemPrompt  string        `yaml:"system_prompt"`
	WorkspaceDir  string        `yaml:"workspace_dir"`
	SkillsDir     string        `yaml:"skills_dir"`
	SkillRescan   time.Duration `yaml:"skill_rescan"`
}

type SandboxConfig struct {
	Type        string        `yaml:"type"`         // "local" or "docker"
	Image       string        `yaml:"image"`        // docker image name
	Timeout     time.Duration `yaml:"timeout"`
	MemoryLimit string        `yaml:"memory_limit"` // docker only
	Network     string        `yaml:"network"`      // docker only
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	cfg.applyEnvOverrides()

	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Port == "" {
		c.Server.Port = "8080"
	}
	// Legacy config migration: if no upstreams defined but legacy fields exist,
	// create a "default" upstream from the legacy fields.
	if len(c.Upstreams) == 0 {
		baseURL := c.Model.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		timeout := c.Model.Timeout
		if timeout == 0 {
			timeout = 120 * time.Second
		}
		c.Upstreams = map[string]UpstreamConfig{
			"default": {
				Type:    "openai",
				BaseURL: baseURL,
				APIKey:  c.Model.APIKey,
				Timeout: timeout,
			},
		}
		modelName := c.Model.ModelName
		if modelName == "" {
			modelName = "gemma3:27b"
		}
		maxTokens := c.Model.MaxTokens
		if maxTokens == 0 {
			maxTokens = 4096
		}
		maxReplyTokens := c.Model.MaxReplyTokens
		if maxReplyTokens == 0 {
			maxReplyTokens = 16384
		}
		if c.Model.Orchestrator.Upstream == "" {
			c.Model.Orchestrator = RoleConfig{Upstream: "default", ModelName: modelName, MaxTokens: maxTokens}
		}
		if c.Model.Synthesizer.Upstream == "" {
			c.Model.Synthesizer = RoleConfig{Upstream: "default", ModelName: modelName, MaxTokens: maxReplyTokens}
		}
		if c.Model.Fallback.Upstream == "" {
			c.Model.Fallback = RoleConfig{Upstream: "default", ModelName: modelName, MaxTokens: maxTokens}
		}
		if c.Model.Transcription.Upstream == "" {
			transModel := c.Model.TranscriptionModel
			if transModel == "" {
				transModel = modelName
			}
			c.Model.Transcription = RoleConfig{Upstream: "default", ModelName: transModel}
		}
	}
	// Apply defaults for new-style config missing individual role fields.
	for name, up := range c.Upstreams {
		if up.Timeout == 0 {
			up.Timeout = 120 * time.Second
			c.Upstreams[name] = up
		}
	}
	if c.Model.Orchestrator.MaxTokens == 0 {
		c.Model.Orchestrator.MaxTokens = 4096
	}
	if c.Model.Synthesizer.MaxTokens == 0 {
		c.Model.Synthesizer.MaxTokens = 16384
	}
	if c.Model.Fallback.MaxTokens == 0 {
		c.Model.Fallback.MaxTokens = 4096
	}
	if c.Agent.MaxIterations == 0 {
		c.Agent.MaxIterations = 10
	}
	if c.Agent.ContextWindow == 0 {
		c.Agent.ContextWindow = 20
	}
	if c.Agent.WorkspaceDir == "" {
		c.Agent.WorkspaceDir = "./workspace"
	}
	if c.Agent.SystemPrompt == "" {
		c.Agent.SystemPrompt = "You are Argus, a personal assistant with tools. Be concise, accurate, and helpful. Respond in the user's language.\n\nIMPORTANT: When a task requires tools, call them immediately. NEVER describe what you would do — just do it. Do not say \"I will try to...\" or \"Let me check...\" without making the actual tool call in the same response.\n\nINFORMATION POLICY: Your training data may be outdated. For ANY question about facts, news, events, people, technology, prices, weather, or knowledge — ALWAYS use the search tool first to get current information from the internet. Do NOT answer from memory alone. Search first, then synthesize. Use the fetch tool to read full articles when search snippets are insufficient. Multiple searches are encouraged to get comprehensive, accurate answers."
	}
	if c.Agent.SkillsDir == "" {
		c.Agent.SkillsDir = ".skills"
	}
	if c.Agent.SkillRescan == 0 {
		c.Agent.SkillRescan = 30 * time.Second
	}
	if c.Search.MaxResults == 0 {
		c.Search.MaxResults = 5
	}
	// Auto-detect provider: use Tavily if API key is set.
	if c.Search.Provider == "" {
		if c.Search.TavilyAPIKey != "" {
			c.Search.Provider = "tavily"
		} else {
			c.Search.Provider = "duckduckgo"
		}
	}
	if c.Embedding.ModelName == "" {
		c.Embedding.ModelName = "nomic-embed-text"
	}
	if c.Embedding.BatchSize == 0 {
		c.Embedding.BatchSize = 32
	}
	if c.Embedding.Interval == 0 {
		c.Embedding.Interval = 2 * time.Second
	}
	if c.Sandbox.Type == "" {
		c.Sandbox.Type = "local"
	}
	if c.Sandbox.Image == "" {
		c.Sandbox.Image = "argus-sandbox"
	}
	if c.Sandbox.Timeout == 0 {
		c.Sandbox.Timeout = 30 * time.Second
	}
	if c.Sandbox.MemoryLimit == "" {
		c.Sandbox.MemoryLimit = "512m"
	}
	if c.Sandbox.Network == "" {
		c.Sandbox.Network = "none"
	}
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("ARGUS_FEISHU_APP_ID"); v != "" {
		c.Feishu.AppID = v
	}
	if v := os.Getenv("ARGUS_FEISHU_APP_SECRET"); v != "" {
		c.Feishu.AppSecret = v
	}
	if v := os.Getenv("ARGUS_FEISHU_VERIFICATION_TOKEN"); v != "" {
		c.Feishu.VerificationToken = v
	}
	if v := os.Getenv("ARGUS_FEISHU_ENCRYPT_KEY"); v != "" {
		c.Feishu.EncryptKey = v
	}
	if v := os.Getenv("ARGUS_MODEL_BASE_URL"); v != "" {
		c.Model.BaseURL = v
	}
	if v := os.Getenv("ARGUS_MODEL_API_KEY"); v != "" {
		c.Model.APIKey = v
	}
	if v := os.Getenv("ARGUS_DATABASE_DSN"); v != "" {
		c.Database.DSN = v
	}
}
