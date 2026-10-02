package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// settings are the operational values an operator may override through the environment.
type settings struct {
	configDir string
	apiBase   string
	oauthBase string
	userAgent string
	// oauthUserAgent goes to the token endpoint; Claude Code issues those calls through axios.
	oauthUserAgent string
	cacheTTL       time.Duration
	debug          bool
}

func loadSettings() (settings, error) {
	cfg := settings{
		configDir: os.Getenv("CLAUDE_USAGE_CONFIG_DIR"),
		apiBase:   envOr("CLAUDE_USAGE_API_BASE", "https://api.anthropic.com"),
		oauthBase: envOr("CLAUDE_USAGE_OAUTH_BASE", "https://platform.claude.com"),
		// The usage endpoint rate-limits by User-Agent and only Claude Code's own UA gets a usable budget.
		userAgent:      envOr("CLAUDE_USAGE_USER_AGENT", "claude-code/2.1.285"),
		oauthUserAgent: envOr("CLAUDE_USAGE_OAUTH_USER_AGENT", "axios/1.15.2"),
		debug:          os.Getenv("CLAUDE_USAGE_DEBUG") != "",
	}
	ttl, err := time.ParseDuration(envOr("CLAUDE_USAGE_CACHE_TTL", "60s"))
	if err != nil {
		return settings{}, fmt.Errorf("parse CLAUDE_USAGE_CACHE_TTL: %w", err)
	}
	cfg.cacheTTL = ttl
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
