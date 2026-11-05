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
	force := fs.Bool("force", false, "refresh usage before selecting an account")
	autoResume := fs.Bool("auto-resume", true, "automatically switch accounts and resume after a detected usage limit (print mode only)")
	maxSwitches := fs.Int("max-switches", 3, "maximum account switches during one run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *min5h < 0 || *min5h > 100 || *minWeek < 0 || *minWeek > 100 {
		return errors.New("minimum quota percentages must be between 0 and 100")
	}
	if *command == "" {
		return errors.New("Claude Code executable cannot be empty")
	}
	if *maxSwitches < 0 {
		return errors.New("max-switches cannot be negative")
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

	results, err := cli.snapshot(ctx, snapshotOptions{force: *force})
	if err != nil {
		return err
	}
	selected, err := selectAccount(results, *min5h, *minWeek)
	if err != nil {
		return err
	}
	profile, err := accountProfile(root, selected.Account.Name)
	if err != nil {
		return err
	}

	claudeArgs := fs.Args()
	if !*autoResume || !isPrintMode(claudeArgs) || *maxSwitches == 0 {
		if *autoResume && !isPrintMode(claudeArgs) {
			fmt.Fprintln(os.Stderr, "Auto Resume is available in Claude Code print mode (-p/--print); launching interactive mode without automatic migration to preserve its TTY.")
		}
		return launchClaude(ctx, *command, claudeArgs, profile)
	}

	sessionID, resumeExisting, err := sessionIDFromArgs(claudeArgs)
	if err != nil {
		return err
	}
	if sessionID == "" {
		sessionID, err = newSessionID()
		if err != nil {
			return fmt.Errorf("create Claude session id: %w", err)
		}
		claudeArgs = append(claudeArgs, "--session-id", sessionID)
	} else if resumeExisting {
		owner, err := accountWithSession(root, results, sessionID)
		if err != nil {
			return err
		}
		selected = owner
		profile, err = accountProfile(root, selected.Account.Name)
		if err != nil {
			return err
		}
	}

	tried := map[string]bool{selected.Account.Name: true}
	for switches := 0; ; {
		fmt.Fprintf(os.Stderr, "Using Claude account %q (%s); profile: %s\n",
			selected.Account.Name, selected.Account.Email, profile)
		runArgs := claudeArgs
		if resumeExisting || switches > 0 {
			runArgs = withResumeSession(claudeArgs, sessionID)
		}
		output, runErr := launchClaudeCapture(ctx, *command, runArgs, profile)
		if runErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return runErr
		}
		if !isUsageLimitOutput(output) {
			return fmt.Errorf("Claude Code: %w", runErr)
		}
		if switches >= *maxSwitches {
			return fmt.Errorf("Claude Code usage limit detected; reached --max-switches=%d", *maxSwitches)
		}

		fmt.Fprintf(os.Stderr, "Usage limit detected for %q; checking other accounts...\n", selected.Account.Name)
		results, err = cli.snapshot(ctx, snapshotOptions{force: true})
		if err != nil {
			return fmt.Errorf("refresh account usage after limit: %w", err)
		}
		next, err := selectNextAccount(results, tried, *min5h, *minWeek)
		if err != nil {
			return fmt.Errorf("usage limit reached and no other eligible account is available: %w", err)
		}
		nextProfile, err := accountProfile(root, next.Account.Name)
		if err != nil {
			return err
		}
		if err := migrateSessionTranscript(profile, nextProfile, sessionID); err != nil {
			return fmt.Errorf("cannot safely resume session on account %q: %w", next.Account.Name, err)
		}
		tried[next.Account.Name] = true
		selected, profile = next, nextProfile
		switches++
		resumeExisting = true
	}
}

func accountProfile(root, accountName string) (string, error) {
	if strings.ContainsAny(accountName, `/\\`) || accountName == "." || accountName == ".." {
		return "", fmt.Errorf("account name %q cannot be used as a profile directory", accountName)
	}
	profile := filepath.Join(root, accountName)
	if err := os.MkdirAll(profile, 0700); err != nil {
		return "", fmt.Errorf("create Claude profile %q: %w", profile, err)
	}
	return profile, nil
}

func launchClaude(ctx context.Context, executable string, args []string, profile string) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = claudeProfileEnv(os.Environ(), profile)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Claude Code: %w", err)
	}
	return nil
}

func selectNextAccount(results []result, excluded map[string]bool, min5h, minWeek float64) (result, error) {
	eligible := make([]result, 0, len(results))
	for _, candidate := range results {
		if candidate.Account != nil && excluded[candidate.Account.Name] {
			continue
		}
		eligible = append(eligible, candidate)
	}
	return selectAccount(eligible, min5h, minWeek)
}

func selectAccount(results []result, min5h, minWeek float64) (result, error) {
	bestScore := 101.0
	var best result
	found := false
	for _, candidate := range results {
		if candidate.Err != nil || candidate.Usage == nil || candidate.Account == nil {
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
