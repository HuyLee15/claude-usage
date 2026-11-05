package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runClaude(ctx context.Context, cli *client, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	command := fs.String("claude", "claude", "Claude Code executable")
	min5h := fs.Float64("min-5h", 0, "minimum remaining 5-hour quota percentage")
	minWeek := fs.Float64("min-week", 0, "minimum remaining weekly quota percentage")
	profileRoot := fs.String("profiles", "", "root directory for per-account Claude profiles")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *min5h < 0 || *min5h > 100 || *minWeek < 0 || *minWeek > 100 {
		return errors.New("minimum quota percentages must be between 0 and 100")
	}
	if *command == "" {
		return errors.New("Claude Code executable cannot be empty")
	}

	results, err := cli.snapshot(ctx, snapshotOptions{})
	if err != nil {
		return err
	}
	selected, err := selectAccount(results, *min5h, *minWeek)
	if err != nil {
		return err
	}

	root := *profileRoot
	if root == "" {
		if configured := os.Getenv("CLAUDE_USAGE_PROFILE_DIR"); configured != "" {
			root = configured
		} else {
			base, err := os.UserConfigDir()
			if err != nil {
				return fmt.Errorf("resolve user config dir: %w", err)
			}
			root = filepath.Join(base, "claude-usage", "profiles")
		}
	}
	if strings.ContainsAny(selected.Account.Name, `/\\`) || selected.Account.Name == "." || selected.Account.Name == ".." {
		return fmt.Errorf("account name %q cannot be used as a profile directory", selected.Account.Name)
	}
	profile := filepath.Join(root, selected.Account.Name)
	if err := os.MkdirAll(profile, 0700); err != nil {
		return fmt.Errorf("create Claude profile %q: %w", profile, err)
	}

	fmt.Fprintf(os.Stderr, "Using Claude account %q (%s); profile: %s\n",
		selected.Account.Name, selected.Account.Email, profile)
	cmd := exec.CommandContext(ctx, *command, fs.Args()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = claudeProfileEnv(os.Environ(), profile)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Claude Code: %w", err)
	}
	return nil
}

func selectAccount(results []result, min5h, minWeek float64) (result, error) {
	bestScore := 101.0
	var best result
	found := false
	for _, candidate := range results {
		if candidate.Err != nil || candidate.Usage == nil {
			continue
		}
		fiveHour, weekly := candidate.Usage.FiveHour, candidate.Usage.SevenDay
		if fiveHour == nil || weekly == nil {
			continue
		}
		remaining5h, remainingWeek := 100-fiveHour.Utilization, 100-weekly.Utilization
		if remaining5h < min5h || remainingWeek < minWeek {
			continue
		}
		score := fiveHour.Utilization
		if weekly.Utilization > score {
			score = weekly.Utilization
		}
		if !found || score < bestScore {
			best, bestScore, found = candidate, score, true
		}
	}
	if !found {
		return result{}, errors.New("no account has fresh usage data and enough quota; check account status first")
	}
	return best, nil
}

func claudeProfileEnv(env []string, profile string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "CLAUDE_CONFIG_DIR="+profile)
}
