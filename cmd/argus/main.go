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

	_ "github.com/lib/pq"

	"argus/internal/agent"
	"argus/internal/config"
	"argus/internal/feishu"
	"argus/internal/model"
	"argus/internal/store"
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

func runCLI(cfg *config.Config) {
	modelClient := model.NewOpenAIClient(cfg.Model)
	memStore := store.NewMemoryStore()
	ag := agent.New(modelClient, memStore, cfg.Agent.SystemPrompt, cfg.Agent.ContextWindow)

	chatID := "cli:local"
	ctx := context.Background()

	fmt.Println("Argus CLI mode. Type messages, Ctrl+C to quit.")
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

	// Connect to PostgreSQL.
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
	ag := agent.New(modelClient, pgStore, cfg.Agent.SystemPrompt, cfg.Agent.ContextWindow)

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

	mux := http.NewServeMux()
	mux.Handle("/webhook/feishu", handler)

	addr := ":" + cfg.Server.Port
	slog.Info("starting server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
