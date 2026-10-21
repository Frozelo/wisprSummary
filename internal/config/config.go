// Package config reads service settings from environment variables.
package config

import (
	"bufio"
	"errors"
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
func Load() (Config, error) {
	loadDotEnv(".env")
	chatID, err := parseChatID(getenv("TELEGRAM_CHAT_ID", ""))
	if err != nil {
		return Config{}, err
	}
	dir := getenv("DATA_DIR", "./data")
	return Config{
		DataDir:     dir,
		WisprMCPURL: getenv("WISPR_MCP_URL", defaultMCPURL),
		LogLevel:    parseLevel(getenv("LOG_LEVEL", "info")),
		TokenFile:   filepath.Join(dir, "wispr_token.json"),

		TelegramBotToken: getenv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:   chatID,

		PollInterval: parseDuration(getenv("POLL_INTERVAL", ""), 2*time.Minute),
		Lookback:     parseDuration(getenv("LOOKBACK", ""), 48*time.Hour),
		Backfill:     time.Duration(parseHours(getenv("BACKFILL_HOURS", ""))) * time.Hour,
		Location:     parseLocation(getenv("TIMEZONE", "")),
		DBFile:       filepath.Join(dir, "state.db"),
	}, nil
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

// parseHours reads BACKFILL_HOURS; empty, invalid or negative means 0.
func parseHours(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseChatID returns 0 for an unset value and an error for a malformed one.
func parseChatID(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return 0, errors.New("TELEGRAM_CHAT_ID must be a non-zero integer (run telegram-setup)")
	}
	return n, nil
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
		k, v, ok := parseEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

// parseEnvLine understands `KEY=value`, an optional `export ` prefix, quoted
// values, and trailing ` # comments` on unquoted values.
func parseEnvLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	k, v, found := strings.Cut(line, "=")
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if !found || k == "" {
		return "", "", false
	}
	if n := len(v); n >= 2 && (v[0] == '"' || v[0] == '\'') && v[n-1] == v[0] {
		return k, v[1 : n-1], true
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return k, v, true
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
