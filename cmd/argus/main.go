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
	"time"

	_ "github.com/lib/pq"

	"argus/internal/agent"
	"argus/internal/config"
	"argus/internal/cron"
	"argus/internal/docindex"
	"argus/internal/embedding"
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
func buildToolRegistry(cfg *config.Config, sb sandbox.Sandbox, loader *skill.FileLoader, db *sql.DB, st store.Store) *tool.Registry {
	registry := tool.NewRegistry()
	registry.Register(tool.NewReadFileTool(cfg.Agent.WorkspaceDir))
	registry.Register(tool.NewWriteFileTool(cfg.Agent.WorkspaceDir))
	registry.Register(tool.NewCLITool(sb))
	registry.Register(tool.NewSearchToolWithConfig(cfg.Search))
	registry.Register(tool.NewFetchTool())
	registry.Register(tool.NewCurrentTimeTool())
	registry.Register(tool.NewFinishTaskTool())

	// save_skill removed — skills are authored by humans, not the model.
	registry.Register(tool.NewActivateSkillTool(loader.Index()))

	if db != nil {
		registry.Register(tool.NewStructuredDBTool(db))
	}

	// Memory tools (available when store supports pinned memories).
	if ps, ok := st.(store.PinnedMemoryStore); ok {
		registry.Register(tool.NewRememberTool(ps))
		registry.Register(tool.NewForgetTool(ps))
	}

	return registry
}

func runCLI(cfg *config.Config) {
	loader := setupSkills(cfg)
	defer loader.Stop()

	// CLI mode: use legacy single-client (all roles use same model).
	modelClient := model.NewOpenAIClient(cfg.Model)
	memStore := store.NewMemoryStore()
	sb := buildSandbox(cfg)
	toolReg := buildToolRegistry(cfg, sb, loader, nil, memStore)
	ag := agent.New(modelClient, modelClient, memStore, toolReg, loader.Index(), nil, cfg.Agent.WorkspaceDir, cfg.Agent.ContextWindow, cfg.Agent.MaxIterations)

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

		msg := model.NewTextMessage(model.RoleUser, text)
		msg.Meta = &model.MessageMeta{SourceIM: "cli", Channel: "local", MsgType: "text"}
		reply, err := ag.Handle(ctx, chatID, msg)
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

			// Run startup repair to fix inconsistencies from previous crashes.
			if rs, ok := st.(store.RepairableStore); ok {
				if n, err := rs.RepairStuckDocuments(ctx); err == nil && n > 0 {
					slog.Info("repaired stuck documents", "count", n)
				}
				if n, err := rs.RepairOrphanChunks(ctx); err == nil && n > 0 {
					slog.Info("cleaned orphan chunks", "count", n)
				}
				if n, err := rs.CountUnembeddedMessages(ctx); err == nil && n > 0 {
					slog.Info("messages pending embedding", "count", n)
				}
				if msgs, err := rs.FailedTranscriptions(ctx); err == nil && len(msgs) > 0 {
					slog.Warn("found messages with failed transcriptions", "count", len(msgs))
				}
			}
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

	// Build model clients for each role (orchestrator, synthesizer, fallback, transcription).
	orchClient, synthClient, fbClient, transClient, err := model.NewClientsForAgent(ctx, cfg.Model)
	if err != nil {
		slog.Error("create model clients", "err", err)
		os.Exit(1)
	}
	// Wrap orchestrator and synthesizer with fallback.
	orchWithFallback := model.NewFallbackClient(orchClient, fbClient)
	synthWithFallback := model.NewFallbackClient(synthClient, fbClient)

	// Embedding client (shared by worker and agent for query embedding).
	// Use the fallback upstream's config for embedding (typically local).
	var embedClient *embedding.Client
	if cfg.Embedding.Enabled {
		fallbackUp := cfg.Model.Upstreams[cfg.Model.Fallback.Upstream]
		embedClient = embedding.NewClient(fallbackUp.BaseURL, fallbackUp.APIKey, cfg.Embedding.ModelName)

		// Start background embedding worker if store supports it.
		if ss, ok := st.(store.SemanticStore); ok {
			var ps store.PinnedMemoryStore
			var ds store.DocumentStore
			if p, ok := st.(store.PinnedMemoryStore); ok {
				ps = p
			}
			if d, ok := st.(store.DocumentStore); ok {
				ds = d
			}
			embedWorker := embedding.NewWorker(embedClient, ss, ps, ds, cfg.Embedding.BatchSize, cfg.Embedding.Interval)
			embedWorker.Start()
			defer embedWorker.Stop()
		} else {
			slog.Warn("embedding enabled but store does not support semantic search (need PostgreSQL)")
		}
	}

	sb := buildSandbox(cfg)
	toolReg := buildToolRegistry(cfg, sb, loader, db, st)
	ag := agent.New(orchWithFallback, synthWithFallback, st, toolReg, loader.Index(), embedClient, cfg.Agent.WorkspaceDir, cfg.Agent.ContextWindow, cfg.Agent.MaxIterations)

	feishuClient := feishu.NewClient(cfg.Feishu)
	processor := render.NewProcessor(feishuClient)
	adapter := feishu.NewAdapter(feishuClient, processor)

	// Document store for RAG indexing (nil if not available).
	var docReg feishu.DocRegisterer
	if ds, ok := st.(store.DocumentStore); ok {
		docReg = ds

		// Start document ingester.
		ingester := docindex.NewIngester(ds, sb, 5*time.Second)
		ingester.Start()
		defer ingester.Stop()
	}

	// QueueStore: use PostgresStore if DB available, else MemoryStore.
	var qs store.QueueStore
	if ps, ok := st.(*store.PostgresStore); ok {
		qs = ps
	} else {
		qs = st.(*store.MemoryStore)
	}

	// Dispatcher: per-chat serial agent processing (channel-per-chat).
	dispatcher := feishu.NewDispatcher(qs, ag, adapter, feishuClient)
	defer dispatcher.Stop()

	// Handler: inbound (store + push + spawn media goroutine).
	handler := feishu.NewHandler(feishuClient, cfg.Feishu, cfg.Agent.WorkspaceDir, qs, dispatcher, transClient, transClient, docReg)

	// Crash recovery: re-queue interrupted messages.
	dispatcher.Recover(ctx, func(msg *store.StoredMessage, readyCh chan struct{}) {
		go handler.ProcessMedia(msg, readyCh)
	})

	// Cron scheduler.
	scheduler := setupCron(cfg, ag, feishuClient, processor, ctx)
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

func setupCron(cfg *config.Config, ag *agent.Agent, feishuClient *feishu.Client, processor *render.Processor, ctx context.Context) *cron.Scheduler {
	scheduler := cron.NewScheduler()

	for _, job := range cfg.Cron.Jobs {
		job := job
		scheduler.AddDaily(job.Name, job.Hour, job.Minute, func() {
			slog.Info("cron job running", "job", job.Name, "chat_id", job.ChatID)

			cronMsg := model.NewTextMessage(model.RoleUser, job.Prompt)
			cronMsg.Meta = &model.MessageMeta{SourceIM: "cron", Channel: job.ChatID, MsgType: "text"}
			reply, err := ag.Handle(ctx, job.ChatID, cronMsg)
			if err != nil {
				slog.Error("cron job agent failed", "job", job.Name, "err", err)
				return
			}

			md := processor.ProcessMarkdown(reply)
			cardJSON := feishu.MarkdownToCard(md)
			receiveIDType, receiveID := parseCronChatID(job.ChatID)
			if err := feishuClient.SendMessageRich(receiveIDType, receiveID, "interactive", cardJSON); err != nil {
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
