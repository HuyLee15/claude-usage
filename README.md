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
claude-usage --force     # skip the 60s cache (a rate-limit backoff is still honoured)
claude-usage --account be1
claude-usage list        # stored accounts and token state
claude-usage remove side # forget an account
```

Columns: `5H` and `WEEK` show utilization with a bar (green < 70 %, yellow < 90 %, red ≥ 90 %) and
the time until the window resets. `SONNET` / `OPUS` are the per-model weekly windows; `—` means the
plan does not track that window.

## Launch Claude Code with the account that has the most quota

Authenticate each account separately in Claude Code once. The launcher keeps Claude Code's
configuration directories isolated by account and never copies or edits OAuth tokens:

```sh
claude-usage run
claude-usage run --min-5h 20 --min-week 10
claude-usage run --auto-resume --max-switches 3 -- -p "Continue implementing the feature"
claude-usage run --claude /path/to/claude -- --resume <session-id>
```

The launcher queries fresh usage, skips accounts with failed or incomplete usage data, and selects
the eligible account with the lowest maximum utilization across the 5-hour and weekly windows.
It starts Claude Code with `CLAUDE_CONFIG_DIR` set to
`$CLAUDE_USAGE_PROFILE_DIR/<account-name>` (default:
`<user config dir>/claude-usage/profiles/<account-name>`). Each profile must be signed in once
using Claude Code itself. The optional minimums are remaining quota percentages.

For Claude Code **print mode** (`-p` / `--print`), Auto Resume is enabled by default. When
Claude Code exits with a recognized usage-limit message, the launcher refreshes account usage,
selects the best account not already tried, copies only that session's JSONL transcript into the
next account profile, and retries with `--resume <session-id>`. It never copies OAuth credentials
or other profile files. Use `--max-switches 0` to disable switching, or set a smaller limit to
control how many account changes are attempted.

Interactive TUI sessions are launched normally without automatic migration. Capturing an
interactive process would detach its terminal (TTY) and break the UI; the launcher therefore
does not pretend to resume an interactive session. Auto Resume is best-effort and only triggers
for explicit usage-limit markers, not network failures or arbitrary non-zero exits. A session
started with `--resume <session-id>` is routed to the profile that already owns its transcript.

## Web dashboard

```sh
claude-usage serve                       # http://127.0.0.1:8787, polls every 60s
claude-usage serve --addr :9000 --interval 2m
```

The page auto-refreshes, has a "refresh now" button (throttled to once per 10 s), and exposes the
same data as JSON at `/api/usage`.

## GNOME panel indicator (Ubuntu)

A GNOME Shell extension (GNOME 45 to 48) puts every account on the top bar, e.g. `be1 76%·w100%  be2 22%  be3 100%`.
The number is the 5-hour usage; `·wNN%` is appended when the weekly window is at 90 % or more, and the colour
follows whichever of the two is fuller. Click it for the full breakdown, reset times, and a "Refresh now" item.

```sh
make install              # the extension calls this binary
make install-extension    # copies the extension and enables it
```

Then reload GNOME Shell once so it picks the extension up: on X11 press `Alt+F2`, type `r`, Enter; on Wayland log
out and back in. `make uninstall-extension` removes it again.

The extension only runs `claude-usage --json` every 2 minutes and when the menu opens. Tokens, the cache and the
rate-limit backoff all stay in the CLI, so the panel, the dashboard and a terminal can run at the same time.

## How it works

1. `login` runs the standard OAuth authorization-code + PKCE flow against `claude.ai/oauth/authorize`
   with the Claude Code client id, and exchanges the code at `platform.claude.com/v1/oauth/token`.
2. Tokens are stored in `accounts.json` (mode 0600) under `CLAUDE_USAGE_CONFIG_DIR`
   (default `~/.config/claude-usage` on Linux).
3. On every check the tool refreshes any token that is about to expire (or that the API rejects with
   401) using the refresh token, persists the rotated pair immediately, then calls
   `GET https://api.anthropic.com/api/oauth/usage` with `Authorization: Bearer …`,
   `anthropic-beta: oauth-2025-04-20` and a Claude Code `User-Agent`. Usage calls run in parallel;
   token refreshes run one at a time and carry the `axios/<version>` User-Agent, which is what Claude
   Code's own OAuth calls send (see Limitations for why each endpoint gets a different one).
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
| `CLAUDE_USAGE_USER_AGENT` | `claude-code/2.1.285`            | User-Agent for the usage endpoint        |
| `CLAUDE_USAGE_OAUTH_USER_AGENT` | `axios/1.15.2`             | User-Agent for the token endpoint        |
| `CLAUDE_USAGE_DEBUG`      | unset                            | log every HTTP exchange to stderr (no tokens) |
| `CLAUDE_USAGE_API_BASE`   | `https://api.anthropic.com`      | host of the usage endpoint               |
| `CLAUDE_USAGE_OAUTH_BASE` | `https://platform.claude.com`    | host of the token endpoint               |
| `NO_COLOR`                | unset                            | disable ANSI colors in the table         |

## Limitations

- `/api/oauth/usage` is not a documented API; Anthropic may change its shape without notice.
- Organizations that disable OAuth for Claude Code return HTTP 403 for that account.
- Both endpoints rate-limit by `User-Agent`, in opposite directions. `/api/oauth/usage` returns HTTP 429 for any
  value other than Claude Code's own (`claude-code/<version>`,
  [anthropics/claude-code#30930](https://github.com/anthropics/claude-code/issues/30930)). The token endpoint
  returns the same `rate_limit_error` 429 **for** `claude-code/<version>`, because the real client reaches it
  through axios, and accepts `axios/<version>`. Even with the right values the usage endpoint throttles if polled
  too often, which is what the cache and backoff are for. `CLAUDE_USAGE_DEBUG=1` shows each exchange when
  something changes again.
