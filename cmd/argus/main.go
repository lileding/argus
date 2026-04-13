package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"argus/internal/config"
	"argus/internal/feishu"
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
	fmt.Println("Argus CLI mode (echo). Type messages, Ctrl+C to quit.")
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			fmt.Print("> ")
			continue
		}
		// Phase 1: echo mode.
		fmt.Printf("Argus: %s\n> ", text)
	}
}

func runServer(cfg *config.Config) {
	client := feishu.NewClient(cfg.Feishu)

	// Phase 1: echo handler — replies with the same text.
	onMsg := func(chatID, text, messageID string) {
		slog.Info("handling message", "chat_id", chatID, "text", text)
		if err := client.Reply(messageID, text); err != nil {
			slog.Error("reply failed", "err", err, "message_id", messageID)
		}
	}

	handler := feishu.NewHandler(client, cfg.Feishu, onMsg)

	mux := http.NewServeMux()
	mux.Handle("/webhook/feishu", handler)

	addr := ":" + cfg.Server.Port
	slog.Info("starting server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
