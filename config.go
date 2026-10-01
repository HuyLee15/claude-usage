package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// settings are the operational values an operator may override through the environment.
type settings struct {
	configDir string
	apiBase   string
	oauthBase string
}

func loadSettings() (settings, error) {
	cfg := settings{
		configDir: os.Getenv("CLAUDE_USAGE_CONFIG_DIR"),
		apiBase:   envOr("CLAUDE_USAGE_API_BASE", "https://api.anthropic.com"),
		oauthBase: envOr("CLAUDE_USAGE_OAUTH_BASE", "https://platform.claude.com"),
	}
	if cfg.configDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return settings{}, fmt.Errorf("resolve user config dir: %w", err)
		}
		cfg.configDir = filepath.Join(base, "claude-usage")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
