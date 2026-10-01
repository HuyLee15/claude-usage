# claude-usage

One command that shows the rate-limit usage (5-hour window, weekly window, per-model weekly windows)
of **every** Claude subscription account you own. Log in to each account once; from then on the tool
refreshes its own tokens, so you never have to switch accounts inside Claude Code just to run `/usage`.

```
$ claude-usage
ACCOUNT  EMAIL                5H                          RESETS    WEEK                        RESETS    SONNET  OPUS
work     felix@example.com     42% ████████░░░░░░░░░░░░  1h12m      8% ██░░░░░░░░░░░░░░░░░░  3d4h      5%      —
side     me@example.com        96% ███████████████████░  41m       14% ███░░░░░░░░░░░░░░░░░  5d9h      —       —
team     dev@example.com      login required: run `claude-usage login team`
updated 14:32:07
```

It reads the same endpoint the `/usage` command inside Claude Code reads (`GET /api/oauth/usage`).

## Install

Go 1.25+ and no other dependency (standard library only).

```sh
make install          # -> ~/.local/bin/claude-usage
# or
go build -o claude-usage .
```

## Add your accounts (once)

```sh
claude-usage login work
claude-usage login side
claude-usage login team
```

Each `login` opens claude.ai in your browser (or prints the URL; add `--no-browser` to only print it).
Sign in with that account, approve, and the browser is redirected back to the tool on
`http://localhost:54545/callback`. If the browser cannot reach this machine (SSH session, remote
desktop), copy the final redirect URL from the address bar and paste it into the terminal instead.

Every account gets its **own** OAuth grant, independent from the one Claude Code holds. Logging in
here does not log Claude Code out, and Claude Code refreshing its token does not break this tool.

## Check usage

```sh
claude-usage             # table for all accounts (same as `claude-usage check`)
claude-usage --json      # machine-readable, e.g. for a status bar or a script
claude-usage list        # stored accounts and token state
claude-usage remove side # forget an account
```

Columns: `5H` and `WEEK` show utilization with a bar (green < 70 %, yellow < 90 %, red ≥ 90 %) and
the time until the window resets. `SONNET` / `OPUS` are the per-model weekly windows; `—` means the
plan does not track that window.

## Web dashboard

```sh
claude-usage serve                       # http://127.0.0.1:8787, polls every 60s
claude-usage serve --addr :9000 --interval 2m
```

The page auto-refreshes, has a "refresh now" button (throttled to once per 10 s), and exposes the
same data as JSON at `/api/usage`.

## How it works

1. `login` runs the standard OAuth authorization-code + PKCE flow against `claude.ai/oauth/authorize`
   with the Claude Code client id, and exchanges the code at `platform.claude.com/v1/oauth/token`.
2. Tokens are stored in `accounts.json` (mode 0600) under `CLAUDE_USAGE_CONFIG_DIR`
   (default `~/.config/claude-usage` on Linux).
3. On every check the tool refreshes any token that is about to expire (or that the API rejects with
   401) using the refresh token, persists the rotated pair immediately, then calls
   `GET https://api.anthropic.com/api/oauth/usage` with `Authorization: Bearer …`,
   `anthropic-beta: oauth-2025-04-20` and a Claude Code `User-Agent`. Accounts are queried in parallel.
4. Results are cached in `cache.json` for `CLAUDE_USAGE_CACHE_TTL` (60 s). A failed fetch keeps showing the last
   good numbers marked "as of … ago" and backs off 1, 2, 4, 8, then 15 minutes before that account is tried
   again. `--force` skips the cache window but never the backoff.
5. Every process takes a lock on the config dir for the whole cycle. Refresh tokens rotate on use, so this keeps
   two callers from refreshing the same account at once and leaving one with a revoked token.
6. If a refresh token is revoked (logged out of all devices, grant expired), the row shows
   `login required` and `claude-usage login <name>` fixes it.

## Environment

| Variable                  | Default                          | Purpose                                  |
|---------------------------|----------------------------------|------------------------------------------|
| `CLAUDE_USAGE_CONFIG_DIR` | `<user config dir>/claude-usage` | where `accounts.json` and `cache.json` live |
| `CLAUDE_USAGE_CACHE_TTL`  | `60s`                            | how long a fetched result is reused      |
| `CLAUDE_USAGE_USER_AGENT` | `claude-code/2.1.285`            | User-Agent sent to Anthropic             |
| `CLAUDE_USAGE_API_BASE`   | `https://api.anthropic.com`      | host of the usage endpoint               |
| `CLAUDE_USAGE_OAUTH_BASE` | `https://platform.claude.com`    | host of the token endpoint               |
| `NO_COLOR`                | unset                            | disable ANSI colors in the table         |

## Limitations

- `/api/oauth/usage` is not a documented API; Anthropic may change its shape without notice.
- Organizations that disable OAuth for Claude Code return HTTP 403 for that account.
- The endpoint rate-limits by `User-Agent`: any value other than Claude Code's gets HTTP 429 almost every time
  ([anthropics/claude-code#30930](https://github.com/anthropics/claude-code/issues/30930)). Even with the right
  value it throttles if polled too often, which is what the cache and backoff are for.
