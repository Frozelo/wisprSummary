// Package config reads service settings from environment variables.
package config

import (
	"bufio"
	"errors"
	"fmt"
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
	SelfName     string // SELF_NAME: replaces the Wispr account name in summaries
}

// Load reads configuration from the environment, after filling unset
// variables from an optional .env file in the working directory.
func Load() (Config, error) {
	loadDotEnv(".env")
	chatID, err := parseChatID(getenv("TELEGRAM_CHAT_ID", ""))
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := durationEnv("POLL_INTERVAL", 2*time.Minute, 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	lookback, err := durationEnv("LOOKBACK", 48*time.Hour, time.Hour)
	if err != nil {
		return Config{}, err
	}
	backfill, err := hoursEnv("BACKFILL_HOURS")
	if err != nil {
		return Config{}, err
	}
	loc, err := locationEnv("TIMEZONE")
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

		PollInterval: pollInterval,
		Lookback:     lookback,
		Backfill:     time.Duration(backfill) * time.Hour,
		Location:     loc,
		DBFile:       filepath.Join(dir, "state.db"),
		SelfName:     getenv("SELF_NAME", ""),
	}, nil
}

// durationEnv reads a Go duration such as "2m"; unset means def, malformed or
// below min is an error (a typo must not silently change behaviour).
func durationEnv(key string, def, min time.Duration) (time.Duration, error) {
	v := getenv(key, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < min {
		return 0, fmt.Errorf("%s must be a duration of at least %s (e.g. %s), got %q", key, min, def, v)
	}
	return d, nil
}

// hoursEnv reads a non-negative whole number of hours; unset means 0.
func hoursEnv(key string) (int64, error) {
	v := getenv(key, "")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > 24*365 {
		return 0, fmt.Errorf("%s must be a whole number of hours between 0 and %d, got %q", key, 24*365, v)
	}
	return n, nil
}

// locationEnv resolves an IANA zone; unset means the system local zone.
func locationEnv(key string) (*time.Location, error) {
	v := getenv(key, "")
	if v == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(v)
	if err != nil {
		return nil, fmt.Errorf("%s: unknown time zone %q (use an IANA name like Europe/Moscow)", key, v)
	}
	return loc, nil
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
