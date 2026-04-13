package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	_ "github.com/lib/pq"

	"argus/internal/agent"
	"argus/internal/config"
	"argus/internal/cron"
	"argus/internal/feishu"
	"argus/internal/model"
	"argus/internal/skill"
	"argus/internal/store"
	"argus/internal/tool"
)

func main() {
	mode := flag.String("mode", "server", "run mode: server or cli")
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	switch *mode {
	case "cli":
		runCLI(cfg)
	case "server":
		runServer(cfg)
	default:
		slog.Error("unknown mode", "mode", *mode)
		os.Exit(1)
	}
}

func ensureWorkspace(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		slog.Error("resolve workspace", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		slog.Error("create workspace", "err", err)
		os.Exit(1)
	}
	return abs
}

func buildToolRegistry(cfg *config.Config, workspaceDir string, db *sql.DB) *tool.Registry {
	registry := tool.NewRegistry()
	registry.Register(tool.NewFileTool(workspaceDir))
	registry.Register(tool.NewCLITool(cfg.Docker, workspaceDir))
	registry.Register(tool.NewSearchTool())
	if db != nil {
		registry.Register(tool.NewDBTool(db))
	}
	return registry
}

func buildSkillRegistry() *skill.Registry {
	registry := skill.NewRegistry()
	registry.Register(skill.NewCodingSkill())
	registry.Register(skill.NewCalorieSkill())
	return registry
}

func runCLI(cfg *config.Config) {
	workspaceDir := ensureWorkspace(cfg.Agent.WorkspaceDir)
	modelClient := model.NewOpenAIClient(cfg.Model)
	memStore := store.NewMemoryStore()
	toolReg := buildToolRegistry(cfg, workspaceDir, nil)
	skillReg := buildSkillRegistry()
	ag := agent.New(modelClient, memStore, toolReg, skillReg, cfg.Agent.SystemPrompt, cfg.Agent.ContextWindow, cfg.Agent.MaxIterations)

	chatID := "cli:local"
	ctx := context.Background()

	fmt.Println("Argus CLI mode. Type messages, Ctrl+C to quit.")
	fmt.Printf("Workspace: %s\n", workspaceDir)
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			fmt.Print("> ")
			continue
		}

		reply, err := ag.Handle(ctx, chatID, text)
		if err != nil {
			fmt.Printf("Error: %v\n> ", err)
			continue
		}
		fmt.Printf("Argus: %s\n> ", reply)
	}
}

func runServer(cfg *config.Config) {
	ctx := context.Background()
	workspaceDir := ensureWorkspace(cfg.Agent.WorkspaceDir)

	db, err := sql.Open("postgres", cfg.Database.DSN)
	if err != nil {
		slog.Error("connect db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	pgStore := store.NewPostgresStore(db)
	if err := pgStore.Migrate(ctx); err != nil {
		slog.Error("migrate db", "err", err)
		os.Exit(1)
	}

	modelClient := model.NewOpenAIClient(cfg.Model)
	toolReg := buildToolRegistry(cfg, workspaceDir, db)
	skillReg := buildSkillRegistry()
	ag := agent.New(modelClient, pgStore, toolReg, skillReg, cfg.Agent.SystemPrompt, cfg.Agent.ContextWindow, cfg.Agent.MaxIterations)

	feishuClient := feishu.NewClient(cfg.Feishu)

	onMsg := func(chatID, text, messageID string) {
		slog.Info("handling message", "chat_id", chatID, "text", text)
		reply, err := ag.Handle(ctx, chatID, text)
		if err != nil {
			slog.Error("agent handle failed", "err", err, "chat_id", chatID)
			reply = fmt.Sprintf("抱歉，处理消息时出错：%v", err)
		}
		if err := feishuClient.Reply(messageID, reply); err != nil {
			slog.Error("reply failed", "err", err, "message_id", messageID)
		}
	}

	handler := feishu.NewHandler(feishuClient, cfg.Feishu, onMsg)

	// Start cron scheduler for scheduled tasks.
	scheduler := setupCron(cfg, ag, feishuClient, ctx)
	scheduler.Start()
	defer scheduler.Stop()

	mux := http.NewServeMux()
	mux.Handle("/webhook/feishu", handler)

	addr := ":" + cfg.Server.Port
	slog.Info("starting server", "addr", addr, "workspace", workspaceDir)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func setupCron(cfg *config.Config, ag *agent.Agent, feishuClient *feishu.Client, ctx context.Context) *cron.Scheduler {
	scheduler := cron.NewScheduler()

	for _, job := range cfg.Cron.Jobs {
		job := job // capture loop variable
		scheduler.AddDaily(job.Name, job.Hour, job.Minute, func() {
			slog.Info("cron job running", "job", job.Name, "chat_id", job.ChatID)

			reply, err := ag.Handle(ctx, job.ChatID, job.Prompt)
			if err != nil {
				slog.Error("cron job agent failed", "job", job.Name, "err", err)
				return
			}

			// Determine receive_id_type from chat_id format.
			receiveIDType, receiveID := parseCronChatID(job.ChatID)
			if err := feishuClient.SendMessage(receiveIDType, receiveID, reply); err != nil {
				slog.Error("cron job send failed", "job", job.Name, "err", err)
			}
		})
	}

	return scheduler
}

// parseCronChatID extracts the receive_id_type and actual ID from a chat_id.
// Format: "p2p:open_id_xxx" or "group:chat_id_xxx"
func parseCronChatID(chatID string) (receiveIDType, receiveID string) {
	if len(chatID) > 4 && chatID[:4] == "p2p:" {
		return "open_id", chatID[4:]
	}
	if len(chatID) > 6 && chatID[:6] == "group:" {
		return "chat_id", chatID[6:]
	}
	// Fallback: treat as chat_id directly.
	return "chat_id", chatID
}
