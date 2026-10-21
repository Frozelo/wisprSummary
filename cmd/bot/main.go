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
	"syscall"
	"time"

	"github.com/Frozelo/wisprSummary/internal/config"
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

const usage = `usage: bot <command>

commands:
  login    authorize Wispr Flow access in the browser (one-off)
  status   show whether a Wispr session is stored
  tools    list Wispr MCP tools with their input schemas
  call     call a Wispr MCP tool: bot call <tool> [json-args]
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
