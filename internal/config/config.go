// Package config reads service settings from environment variables.
package config

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultMCPURL = "https://api.wisprflow.ai/connect/mcp"

type Config struct {
	DataDir     string
	WisprMCPURL string
	LogLevel    slog.Level
	TokenFile   string

	TelegramBotToken string
	TelegramChatID   int64 // 0 = not configured

	PollInterval time.Duration
	Lookback     time.Duration
	Backfill     time.Duration // BACKFILL_HOURS: meetings to send on first run
	Location     *time.Location
	DBFile       string
}

// Load reads configuration from the environment, after filling unset
// variables from an optional .env file in the working directory.
func Load() Config {
	loadDotEnv(".env")
	dir := getenv("DATA_DIR", "./data")
	return Config{
		DataDir:     dir,
		WisprMCPURL: getenv("WISPR_MCP_URL", defaultMCPURL),
		LogLevel:    parseLevel(getenv("LOG_LEVEL", "info")),
		TokenFile:   filepath.Join(dir, "wispr_token.json"),

		TelegramBotToken: getenv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:   parseInt64(getenv("TELEGRAM_CHAT_ID", "")),

		PollInterval: parseDuration(getenv("POLL_INTERVAL", ""), 2*time.Minute),
		Lookback:     parseDuration(getenv("LOOKBACK", ""), 48*time.Hour),
		Backfill:     time.Duration(parseInt64(getenv("BACKFILL_HOURS", ""))) * time.Hour,
		Location:     parseLocation(getenv("TIMEZONE", "")),
		DBFile:       filepath.Join(dir, "state.db"),
	}
}

func parseDuration(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// parseLocation resolves an IANA name; empty or invalid falls back to local time.
func parseLocation(name string) *time.Location {
	if name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.Local
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// loadDotEnv sets variables from KEY=VALUE lines without overriding the
// real environment. A missing file is fine.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set && k != "" {
			os.Setenv(k, v)
		}
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
