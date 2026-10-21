// Package config reads service settings from environment variables.
package config

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultMCPURL = "https://api.wisprflow.ai/connect/mcp"

type Config struct {
	DataDir     string
	WisprMCPURL string
	LogLevel    slog.Level
	TokenFile   string

	TelegramBotToken string
	TelegramChatID   int64 // 0 = not configured
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
	}
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
