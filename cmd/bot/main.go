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
	_ "time/tzdata" // IANA zones in minimal Docker images

	"github.com/Frozelo/wisprSummary/internal/config"
	"github.com/Frozelo/wisprSummary/internal/formatter"
	"github.com/Frozelo/wisprSummary/internal/meeting"
	"github.com/Frozelo/wisprSummary/internal/storage"
	"github.com/Frozelo/wisprSummary/internal/telegram"
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

const usage = `usage: bot <command>

commands:
  run      poll Wispr and send new meeting summaries to Telegram
  login    authorize Wispr Flow access in the browser (one-off)
  status   show whether a Wispr session is stored
  tools    list Wispr MCP tools with their input schemas
  call     call a Wispr MCP tool: bot call <tool> [json-args]
  telegram-setup  wait for /start in a private chat and print its chat id
  telegram-test   send a test message to TELEGRAM_CHAT_ID
  preview  format a real meeting: bot preview <meeting-id> [--send]
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
	case "run":
		err = run(ctx, cfg)
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
	case "preview":
		err = preview(ctx, cfg, os.Args[2:])
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

// preview prints how a real meeting would look in Telegram; --send delivers it.
func preview(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: bot preview <meeting-id> [--send]")
	}
	m, err := newClient(cfg).GetMeeting(ctx, args[0])
	if err != nil {
		return err
	}
	msgs := formatter.Format(formatter.Meeting{
		Title: m.Title, Start: m.Start, ShareLink: m.ShareLink, Summary: m.Summary,
	}, time.Local)
	for i, msg := range msgs {
		fmt.Printf("--- message %d/%d ---\n%s\n", i+1, len(msgs), msg)
	}
	if len(args) > 1 && args[1] == "--send" {
		tg, err := newTelegram(cfg)
		if err != nil {
			return err
		}
		for _, msg := range msgs {
			if err := tg.SendMessage(ctx, cfg.TelegramChatID, msg); err != nil {
				return err
			}
		}
		fmt.Println("sent")
	}
	return nil
}

func run(ctx context.Context, cfg config.Config) error {
	tg, err := newTelegram(cfg)
	if err != nil {
		return err
	}
	if cfg.TelegramChatID == 0 {
		return errors.New("TELEGRAM_CHAT_ID is not set (run telegram-setup)")
	}
	if _, err := (wispr.TokenStore{Path: cfg.TokenFile}).Load(); err != nil {
		return err
	}
	db, err := storage.Open(cfg.DBFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	svc := meeting.NewService(meeting.Config{
		Interval: cfg.PollInterval,
		Lookback: cfg.Lookback,
		Backfill: cfg.Backfill,
		Loc:      cfg.Location,
	}, newClient(cfg), telegram.Chat{C: tg, ID: cfg.TelegramChatID}, db, slog.Default())

	slog.Info("started", "poll_interval", cfg.PollInterval)
	err = svc.Run(ctx)
	slog.Info("stopped")
	return err
}
