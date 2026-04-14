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
	"argus/internal/render"
	"argus/internal/sandbox"
	"argus/internal/skill"
	"argus/internal/store"
	"argus/internal/tool"
)

func main() {
	mode := flag.String("mode", "server", "run mode: server or cli")
	workspace := flag.String("workspace", ".", "workspace directory (contains config.yaml and .skills/)")
	flag.Parse()

	// Resolve workspace to absolute path.
	absWorkspace, err := filepath.Abs(*workspace)
	if err != nil {
		slog.Error("resolve workspace", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(absWorkspace, 0755); err != nil {
		slog.Error("create workspace", "err", err)
		os.Exit(1)
	}

	// Config is always workspace/config.yaml.
	configPath := filepath.Join(absWorkspace, "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("load config", "path", configPath, "err", err)
		os.Exit(1)
	}

	// Override workspace dir to absolute path.
	cfg.Agent.WorkspaceDir = absWorkspace

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

// setupSkills initializes the skill loader: loads builtins + user skills, starts background rescan.
func setupSkills(cfg *config.Config) *skill.FileLoader {
	skillsDir := filepath.Join(cfg.Agent.WorkspaceDir, cfg.Agent.SkillsDir)
	loader := skill.NewFileLoader(skillsDir, cfg.Agent.SkillRescan)

	if err := loader.LoadAll(); err != nil {
		slog.Warn("load skills failed", "err", err)
	}

	loader.Start()
	return loader
}

// buildSandbox creates the appropriate sandbox based on config.
func buildSandbox(cfg *config.Config) sandbox.Sandbox {
	switch cfg.Sandbox.Type {
	case "docker":
		return &sandbox.Docker{
			Image:        cfg.Sandbox.Image,
			WorkspaceDir: cfg.Agent.WorkspaceDir,
			Network:      cfg.Sandbox.Network,
			MemoryLimit:  cfg.Sandbox.MemoryLimit,
			Timeout:      cfg.Sandbox.Timeout,
		}
	default: // "local"
		return &sandbox.Local{
			WorkspaceDir: cfg.Agent.WorkspaceDir,
			Timeout:      cfg.Sandbox.Timeout,
		}
	}
}

// buildToolRegistry creates the tool registry with all available tools.
func buildToolRegistry(cfg *config.Config, sb sandbox.Sandbox, loader *skill.FileLoader, db *sql.DB) *tool.Registry {
	registry := tool.NewRegistry()
	registry.Register(tool.NewReadFileTool(cfg.Agent.WorkspaceDir))
	registry.Register(tool.NewWriteFileTool(cfg.Agent.WorkspaceDir))
	registry.Register(tool.NewCLITool(sb))
	registry.Register(tool.NewSearchTool())
	registry.Register(tool.NewFetchTool())
	registry.Register(tool.NewCurrentTimeTool())

	skillsDir := filepath.Join(cfg.Agent.WorkspaceDir, cfg.Agent.SkillsDir)
	registry.Register(tool.NewSaveSkillTool(skillsDir, loader.Rebuild))
	registry.Register(tool.NewActivateSkillTool(loader.Index()))

	if db != nil {
		registry.Register(tool.NewDBTool(db))
		registry.Register(tool.NewDBExecTool(db))
	}

	return registry
}

func runCLI(cfg *config.Config) {
	loader := setupSkills(cfg)
	defer loader.Stop()

	modelClient := model.NewOpenAIClient(cfg.Model)
	memStore := store.NewMemoryStore()
	sb := buildSandbox(cfg)
	toolReg := buildToolRegistry(cfg, sb, loader, nil)
	ag := agent.New(modelClient, memStore, toolReg, loader.Index(), cfg.Agent.SystemPrompt, cfg.Agent.WorkspaceDir, cfg.Agent.ContextWindow, cfg.Agent.MaxIterations)

	chatID := "cli:local"
	ctx := context.Background()

	fmt.Println("Argus CLI mode. Type messages, Ctrl+C to quit.")
	fmt.Printf("Workspace: %s\n", cfg.Agent.WorkspaceDir)
	fmt.Printf("Skills: %d loaded\n", len(loader.Index().All()))
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			fmt.Print("> ")
			continue
		}

		reply, err := ag.Handle(ctx, chatID, model.NewTextMessage(model.RoleUser, text))
		if err != nil {
			fmt.Printf("Error: %v\n> ", err)
			continue
		}
		fmt.Printf("Argus: %s\n> ", reply)
	}
}

func runServer(cfg *config.Config) {
	ctx := context.Background()

	loader := setupSkills(cfg)
	defer loader.Stop()

	// Database is optional — use memory store if DSN is empty or connection fails.
	var st store.Store
	var db *sql.DB
	if cfg.Database.DSN != "" {
		var err error
		db, err = sql.Open("postgres", cfg.Database.DSN)
		if err == nil {
			err = db.PingContext(ctx)
		}
		if err == nil {
			pgStore := store.NewPostgresStore(db)
			if err := pgStore.Migrate(ctx); err != nil {
				slog.Error("migrate db", "err", err)
				os.Exit(1)
			}
			st = pgStore
			slog.Info("using PostgreSQL store")
		} else {
			slog.Warn("database unavailable, using memory store", "err", err)
			db = nil
		}
	}
	if st == nil {
		st = store.NewMemoryStore()
		slog.Info("using memory store (messages will not persist across restarts)")
	}
	if db != nil {
		defer db.Close()
	}

	modelClient := model.NewOpenAIClient(cfg.Model)
	sb := buildSandbox(cfg)
	toolReg := buildToolRegistry(cfg, sb, loader, db)
	ag := agent.New(modelClient, st, toolReg, loader.Index(), cfg.Agent.SystemPrompt, cfg.Agent.WorkspaceDir, cfg.Agent.ContextWindow, cfg.Agent.MaxIterations)

	feishuClient := feishu.NewClient(cfg.Feishu)

	onMsg := func(chatID string, msg model.Message, messageID string) {
		slog.Info("handling message", "chat_id", chatID, "msg_text", msg.TextContent())
		reply, err := ag.Handle(ctx, chatID, msg)
		if err != nil {
			slog.Error("agent handle failed", "err", err, "chat_id", chatID)
			reply = fmt.Sprintf("Error: %v", err)
		}
		msgType, content := render.ForFeishu(reply)
		if err := feishuClient.ReplyRich(messageID, msgType, content); err != nil {
			slog.Error("reply failed", "err", err, "message_id", messageID)
		}
	}

	handler := feishu.NewHandler(feishuClient, cfg.Feishu, cfg.Agent.WorkspaceDir, onMsg)

	// Cron scheduler.
	scheduler := setupCron(cfg, ag, feishuClient, ctx)
	scheduler.Start()
	defer scheduler.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("/webhook/feishu", handler)

	addr := ":" + cfg.Server.Port
	slog.Info("starting server", "addr", addr, "workspace", cfg.Agent.WorkspaceDir)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func setupCron(cfg *config.Config, ag *agent.Agent, feishuClient *feishu.Client, ctx context.Context) *cron.Scheduler {
	scheduler := cron.NewScheduler()

	for _, job := range cfg.Cron.Jobs {
		job := job
		scheduler.AddDaily(job.Name, job.Hour, job.Minute, func() {
			slog.Info("cron job running", "job", job.Name, "chat_id", job.ChatID)

			reply, err := ag.Handle(ctx, job.ChatID, model.NewTextMessage(model.RoleUser, job.Prompt))
			if err != nil {
				slog.Error("cron job agent failed", "job", job.Name, "err", err)
				return
			}

			receiveIDType, receiveID := parseCronChatID(job.ChatID)
			if err := feishuClient.SendMessage(receiveIDType, receiveID, reply); err != nil {
				slog.Error("cron job send failed", "job", job.Name, "err", err)
			}
		})
	}

	return scheduler
}

func parseCronChatID(chatID string) (receiveIDType, receiveID string) {
	if len(chatID) > 4 && chatID[:4] == "p2p:" {
		return "open_id", chatID[4:]
	}
	if len(chatID) > 6 && chatID[:6] == "group:" {
		return "chat_id", chatID[6:]
	}
	return "chat_id", chatID
}
