package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Feishu   FeishuConfig   `yaml:"feishu"`
	Model    ModelConfig    `yaml:"model"`
	Database DatabaseConfig `yaml:"database"`
	Agent   AgentConfig   `yaml:"agent"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	Cron    CronConfig    `yaml:"cron"`
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

type ModelConfig struct {
	BaseURL   string        `yaml:"base_url"`
	ModelName string        `yaml:"model_name"`
	APIKey    string        `yaml:"api_key"`
	MaxTokens int           `yaml:"max_tokens"`
	Timeout   time.Duration `yaml:"timeout"`
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
	if c.Model.BaseURL == "" {
		c.Model.BaseURL = "http://localhost:11434/v1"
	}
	if c.Model.ModelName == "" {
		c.Model.ModelName = "gemma3:27b"
	}
	if c.Model.MaxTokens == 0 {
		c.Model.MaxTokens = 4096
	}
	if c.Model.Timeout == 0 {
		c.Model.Timeout = 120 * time.Second
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
