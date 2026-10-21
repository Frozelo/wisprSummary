// Package config reads service settings from environment variables.
package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const defaultMCPURL = "https://api.wisprflow.ai/connect/mcp"

type Config struct {
	DataDir     string
	WisprMCPURL string
	LogLevel    slog.Level
	TokenFile   string
}

func Load() Config {
	dir := getenv("DATA_DIR", "./data")
	return Config{
		DataDir:     dir,
		WisprMCPURL: getenv("WISPR_MCP_URL", defaultMCPURL),
		LogLevel:    parseLevel(getenv("LOG_LEVEL", "info")),
		TokenFile:   filepath.Join(dir, "wispr_token.json"),
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
