package main

import (
	"context"
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
