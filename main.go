// claude-usage shows the rate-limit usage of several Claude subscription accounts from one place,
// logging in to each account once and refreshing its token on its own from then on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// version is stamped by the Makefile through -ldflags.
var version = "dev"

const usageText = `claude-usage — rate-limit usage of every Claude subscription account in one place

Usage:
  claude-usage [check] [--json]          show 5h / weekly usage for all accounts
  claude-usage login [--no-browser] <name>
                                         add (or re-authorize) an account via browser login
  claude-usage list                      list stored accounts
  claude-usage remove <name>             forget an account
  claude-usage serve [--addr 127.0.0.1:8787] [--interval 60s]
                                         local web dashboard
  claude-usage version

Environment:
  CLAUDE_USAGE_CONFIG_DIR   where accounts.json lives (default: <user config dir>/claude-usage)
  CLAUDE_USAGE_API_BASE     usage endpoint host (default https://api.anthropic.com)
  CLAUDE_USAGE_OAUTH_BASE   token endpoint host (default https://platform.claude.com)
  NO_COLOR                  disable ANSI colors
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadSettings()
	if err != nil {
		return err
	}
	st, err := openStore(filepath.Join(cfg.configDir, accountsFileName))
	if err != nil {
		return err
	}
	cli := newClient(cfg)

	cmd := "check"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "check":
		return runCheck(ctx, cli, st, args)
	case "login":
		return runLogin(ctx, cli, st, args)
	case "list":
		return runList(st)
	case "remove":
		return runRemove(st, args)
	case "serve":
		return runServe(ctx, cli, st, args)
	case "version":
		fmt.Println("claude-usage", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	}
	if strings.HasPrefix(cmd, "-") {
		// Bare flags (`claude-usage --json`) belong to the default command.
		return runCheck(ctx, cli, st, append([]string{cmd}, args...))
	}
	fmt.Fprint(os.Stderr, usageText)
	return fmt.Errorf("unknown command %q", cmd)
}

func runCheck(ctx context.Context, cli *client, st *store, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine-readable JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	accounts := st.list()
	if len(accounts) == 0 {
		return errors.New("no accounts yet: run `claude-usage login <name>` first")
	}
	results := cli.fetchAll(ctx, st, accounts)
	if *asJSON {
		return writeJSON(os.Stdout, results)
	}
	renderTable(os.Stdout, results, colorEnabled())
	return nil
}

func runLogin(ctx context.Context, cli *client, st *store, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "print the URL only; do not try to open a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return errors.New("usage: claude-usage login [--no-browser] <name>")
	}
	name := fs.Arg(0)
	tok, err := cli.login(ctx, os.Stdin, os.Stdout, !*noBrowser)
	if err != nil {
		return fmt.Errorf("login %s: %w", name, err)
	}
	acc := &account{Name: name, AddedAt: time.Now()}
	acc.applyTokens(tok)
	if err := st.upsert(acc); err != nil {
		return err
	}
	fmt.Printf("Logged in %q as %s (%s); token valid for %s\n", name, acc.Email, acc.Organization, formatUntil(acc.ExpiresAt))
	return nil
}

func runList(st *store) error {
	accounts := st.list()
	if len(accounts) == 0 {
		fmt.Println("no accounts yet: run `claude-usage login <name>` first")
		return nil
	}
	for _, acc := range accounts {
		state := "token valid for " + formatUntil(acc.ExpiresAt)
		if time.Until(acc.ExpiresAt) < refreshSkew {
			state = "token expired (refreshes on next check)"
		}
		fmt.Printf("%-16s %-32s %-24s %s\n", acc.Name, acc.Email, acc.Organization, state)
	}
	return nil
}

func runRemove(st *store, args []string) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: claude-usage remove <name>")
	}
	if err := st.remove(args[0]); err != nil {
		return fmt.Errorf("remove %s: %w", args[0], err)
	}
	fmt.Printf("Removed %q\n", args[0])
	return nil
}

func runServe(ctx context.Context, cli *client, st *store, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	interval := fs.Duration("interval", time.Minute, "how often to poll the usage endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *interval < 10*time.Second {
		return errors.New("interval must be at least 10s")
	}
	if len(st.list()) == 0 {
		return errors.New("no accounts yet: run `claude-usage login <name>` first")
	}
	return serve(ctx, cli, st, *addr, *interval)
}
