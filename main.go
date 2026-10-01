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
	"strings"
	"syscall"
	"time"
)

// version is stamped by the Makefile through -ldflags.
var version = "dev"

const usageText = `claude-usage — rate-limit usage of every Claude subscription account in one place

Usage:
  claude-usage [check] [--json] [--force]
                                         show 5h / weekly usage for all accounts
  claude-usage login [--no-browser] <name>
                                         add (or re-authorize) an account via browser login
  claude-usage list                      list stored accounts
  claude-usage remove <name>             forget an account
  claude-usage serve [--addr 127.0.0.1:8787] [--interval 60s]
                                         local web dashboard
  claude-usage version

Results are cached for CLAUDE_USAGE_CACHE_TTL; --force skips that window but never a rate-limit backoff.

Environment:
  CLAUDE_USAGE_CONFIG_DIR   where accounts.json and cache.json live (default: <user config dir>/claude-usage)
  CLAUDE_USAGE_CACHE_TTL    how long a fetched result is reused (default 60s)
  CLAUDE_USAGE_USER_AGENT   User-Agent sent to Anthropic (default claude-code/<version>)
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
	cli := newClient(cfg)

	cmd := "check"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "check":
		return runCheck(ctx, cli, args)
	case "login":
		return runLogin(ctx, cli, args)
	case "list":
		return runList(cli)
	case "remove":
		return runRemove(cli, args)
	case "serve":
		return runServe(ctx, cli, args)
	case "version":
		fmt.Println("claude-usage", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	}
	if strings.HasPrefix(cmd, "-") {
		// Bare flags (`claude-usage --json`) belong to the default command.
		return runCheck(ctx, cli, append([]string{cmd}, args...))
	}
	fmt.Fprint(os.Stderr, usageText)
	return fmt.Errorf("unknown command %q", cmd)
}

func runCheck(ctx context.Context, cli *client, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print machine-readable JSON instead of a table")
	force := fs.Bool("force", false, "ignore the cache freshness window (rate-limit backoff still applies)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	results, err := cli.snapshot(ctx, *force)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(os.Stdout, results)
	}
	renderTable(os.Stdout, results, colorEnabled())
	return nil
}

func runLogin(ctx context.Context, cli *client, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "print the URL only; do not try to open a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		return errors.New("usage: claude-usage login [--no-browser] <name>")
	}
	name := fs.Arg(0)
	// The browser step can take minutes, so it runs before taking the lock that pollers wait on.
	tok, err := cli.login(ctx, os.Stdin, os.Stdout, !*noBrowser)
	if err != nil {
		return fmt.Errorf("login %s: %w", name, err)
	}
	acc := &account{Name: name, AddedAt: time.Now()}
	acc.applyTokens(tok)
	if err := cli.mutateAccounts(func(st *store, cache *usageCache) error {
		cache.forget(name)
		return st.upsert(acc)
	}); err != nil {
		return err
	}
	fmt.Printf("Logged in %q as %s (%s); token valid for %s\n", name, acc.Email, acc.Organization, formatUntil(acc.ExpiresAt))
	return nil
}

func runList(cli *client) error {
	st, err := openStore(cli.accountsPath())
	if err != nil {
		return err
	}
	accounts := st.list()
	if len(accounts) == 0 {
		fmt.Println(errNoAccounts)
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

func runRemove(cli *client, args []string) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: claude-usage remove <name>")
	}
	name := args[0]
	if err := cli.mutateAccounts(func(st *store, cache *usageCache) error {
		cache.forget(name)
		return st.remove(name)
	}); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	fmt.Printf("Removed %q\n", name)
	return nil
}

func runServe(ctx context.Context, cli *client, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	interval := fs.Duration("interval", time.Minute, "how often to poll the usage endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *interval < 10*time.Second {
		return errors.New("interval must be at least 10s")
	}
	return serve(ctx, cli, *addr, *interval)
}
