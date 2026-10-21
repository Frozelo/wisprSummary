package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Frozelo/wisprSummary/internal/config"
	"github.com/Frozelo/wisprSummary/internal/telegram"
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

const usage = `usage: bot <command>

commands:
  login    authorize Wispr Flow access in the browser (one-off)
  status   show whether a Wispr session is stored
  tools    list Wispr MCP tools with their input schemas
  call     call a Wispr MCP tool: bot call <tool> [json-args]
  telegram-setup  wait for /start in a private chat and print its chat id
  telegram-test   send a test message to TELEGRAM_CHAT_ID
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	var err error
	switch os.Args[1] {
	case "login":
		err = wispr.Login(ctx, wispr.LoginOptions{
			MCPURL: cfg.WisprMCPURL,
			HTTP:   &http.Client{Timeout: 30 * time.Second},
			Store:  wispr.TokenStore{Path: cfg.TokenFile},
			Out:    os.Stdout,
		})
		if err == nil {
			fmt.Println("Wispr login saved to", cfg.TokenFile)
		}
	case "status":
		err = status(cfg)
	case "tools":
		err = tools(ctx, cfg)
	case "call":
		err = call(ctx, cfg, os.Args[2:])
	case "telegram-setup":
		err = telegramSetup(ctx, cfg)
	case "telegram-test":
		err = telegramTest(ctx, cfg)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func status(cfg config.Config) error {
	t, err := wispr.TokenStore{Path: cfg.TokenFile}.Load()
	if errors.Is(err, wispr.ErrLoginRequired) {
		fmt.Println("not logged in")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("logged in; access token expires %s; refresh token present: %t\n",
		t.Expiry.Local().Format(time.RFC3339), t.RefreshToken != "")
	return nil
}

func newClient(cfg config.Config) *wispr.Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	ts := wispr.NewTokenSource(wispr.TokenStore{Path: cfg.TokenFile}, hc)
	return wispr.NewClient(cfg.WisprMCPURL, hc, ts)
}

func tools(ctx context.Context, cfg config.Config) error {
	list, err := newClient(cfg).ListTools(ctx)
	if err != nil {
		return err
	}
	for _, t := range list {
		fmt.Printf("== %s\n%s\nschema: %s\n\n", t.Name, t.Description, t.InputSchema)
	}
	return nil
}

func call(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: bot call <tool> [json-args]")
	}
	var params map[string]any
	if len(args) > 1 {
		if err := json.Unmarshal([]byte(args[1]), &params); err != nil {
			return fmt.Errorf("bad json args: %w", err)
		}
	}
	res, err := newClient(cfg).CallTool(ctx, args[0], params)
	if err != nil {
		return err
	}
	if len(res.StructuredContent) > 0 {
		fmt.Printf("structuredContent: %s\n", res.StructuredContent)
	}
	fmt.Println(res.Text())
	return nil
}

func newTelegram(cfg config.Config) (*telegram.Client, error) {
	if cfg.TelegramBotToken == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN is not set (see .env.example)")
	}
	return telegram.New(cfg.TelegramBotToken, &http.Client{Timeout: 45 * time.Second}), nil
}

// telegramSetup waits for /start in a private chat and prints the chat id.
func telegramSetup(ctx context.Context, cfg config.Config) error {
	tg, err := newTelegram(cfg)
	if err != nil {
		return err
	}
	fmt.Println("Send /start to your bot from your personal Telegram account (Ctrl+C to cancel)...")
	var offset int64
	for {
		ups, err := tg.GetUpdates(ctx, offset, 30)
		if err != nil {
			return err
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			if m == nil || m.Chat.Type != "private" || !strings.HasPrefix(m.Text, "/start") {
				continue
			}
			fmt.Printf("\nChat id: %d\nAdd to .env:  TELEGRAM_CHAT_ID=%d\n", m.Chat.ID, m.Chat.ID)
			_ = tg.SendMessage(ctx, m.Chat.ID, "Chat registered. Set TELEGRAM_CHAT_ID in the service config to receive meeting summaries here.")
			return nil
		}
	}
}

func telegramTest(ctx context.Context, cfg config.Config) error {
	tg, err := newTelegram(cfg)
	if err != nil {
		return err
	}
	if cfg.TelegramChatID == 0 {
		return errors.New("TELEGRAM_CHAT_ID is not set (run telegram-setup)")
	}
	if err := tg.SendMessage(ctx, cfg.TelegramChatID, "<b>Test message</b> from wispr-telegram assistant"); err != nil {
		return err
	}
	fmt.Println("sent")
	return nil
}
