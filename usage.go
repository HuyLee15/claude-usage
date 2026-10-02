package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	usagePath = "/api/oauth/usage"
	// oauthBeta is the beta flag api.anthropic.com requires before it accepts a subscription OAuth bearer.
	oauthBeta = "oauth-2025-04-20"
	// refreshSkew renews a token slightly before its recorded expiry so a request never straddles it.
	refreshSkew       = 2 * time.Minute
	maxBodyBytes      = 1 << 20
	defaultRetryAfter = time.Minute
)

var (
	errLoginRequired = errors.New("login required")
	errNoAccounts    = errors.New("no accounts yet: run `claude-usage login <name>` first")
)

// window is one rate-limit bucket; resets_at is null for buckets the plan does not track.
type window struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

type usage struct {
	FiveHour       *window `json:"five_hour"`
	SevenDay       *window `json:"seven_day"`
	SevenDaySonnet *window `json:"seven_day_sonnet"`
	SevenDayOpus   *window `json:"seven_day_opus"`
}

// result carries the newest usage known for an account; Usage and Err are both set when a fetch failed
// and the last good value is shown as stale.
type result struct {
	Account   *account
	Usage     *usage
	Err       error
	FetchedAt time.Time
}

// backoffError tells the caller when the next real request for the account will be attempted.
type backoffError struct {
	reason  string
	retryAt time.Time
}

func (e *backoffError) Error() string {
	return fmt.Sprintf("%s; next try in %s", e.reason, formatUntil(e.retryAt))
}

type client struct {
	httpClient *http.Client
	apiBase    string
	oauthBase  string
	userAgent  string
	oauthUA    string
	configDir  string
	cacheTTL   time.Duration
	debug      bool
	// refreshMu serializes token refreshes: the token endpoint is far stricter than the usage endpoint,
	// and every account tends to expire in the same minute.
	refreshMu sync.Mutex
}

func newClient(cfg settings) *client {
	return &client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		apiBase:    cfg.apiBase,
		oauthBase:  cfg.oauthBase,
		userAgent:  cfg.userAgent,
		oauthUA:    cfg.oauthUserAgent,
		configDir:  cfg.configDir,
		cacheTTL:   cfg.cacheTTL,
		debug:      cfg.debug,
	}
}

func (c *client) accountsPath() string {
	return filepath.Join(c.configDir, accountsFileName)
}

func (c *client) cachePath() string {
	return filepath.Join(c.configDir, cacheFileName)
}

// snapshotOptions tunes one fetch cycle: force skips the cache window (never the backoff) and only
// restricts the cycle to a single account.
type snapshotOptions struct {
	force bool
	only  string
}

// snapshot runs one fetch cycle under the config-dir lock and re-reads both files inside it,
// so it always starts from tokens another process may have just rotated.
func (c *client) snapshot(ctx context.Context, opts snapshotOptions) ([]result, error) {
	unlock, err := lockConfigDir(c.configDir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	st, err := openStore(c.accountsPath())
	if err != nil {
		return nil, err
	}
	accounts := st.list()
	if len(accounts) == 0 {
		return nil, errNoAccounts
	}
	cache, err := openCache(c.cachePath())
	if err != nil {
		return nil, err
	}
	cache.retain(accounts)
	if opts.only != "" {
		accounts = slices.DeleteFunc(accounts, func(acc *account) bool { return acc.Name != opts.only })
		if len(accounts) == 0 {
			return nil, fmt.Errorf("%w: %s", errAccountNotFound, opts.only)
		}
	}
	results := c.fetchAll(ctx, st, cache, accounts, opts.force)
	if err := cache.save(); err != nil {
		return nil, err
	}
	return results, nil
}

// fetchAll queries every account in parallel and returns results in the same order.
func (c *client) fetchAll(ctx context.Context, st *store, cache *usageCache, accounts []*account, force bool) []result {
	var wg sync.WaitGroup

	results := make([]result, len(accounts))
	for i, acc := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.fetchCached(ctx, st, cache, acc, force)
		}()
	}
	wg.Wait()
	return results
}

// fetchCached serves a fresh cache entry, honours the account's backoff, and only then calls the API.
// force skips the freshness window but never the backoff.
func (c *client) fetchCached(ctx context.Context, st *store, cache *usageCache, acc *account, force bool) result {
	now := time.Now()
	entry := cache.get(acc.Name)
	cached := result{Account: acc, Usage: entry.Usage, FetchedAt: entry.FetchedAt}
	if now.Before(entry.RetryAt) {
		cached.Err = &backoffError{reason: entry.LastError, retryAt: entry.RetryAt}
		return cached
	}
	if !force && entry.Usage != nil && now.Sub(entry.FetchedAt) < c.cacheTTL {
		return cached
	}
	u, err := c.fetchOne(ctx, st, acc)
	if err != nil {
		if ctx.Err() != nil {
			// An interrupted run says nothing about the endpoint, so it must not start a backoff.
			cached.Err = err
			return cached
		}
		cached.Err = &backoffError{reason: err.Error(), retryAt: cache.recordFailure(acc.Name, err, now)}
		return cached
	}
	cache.recordSuccess(acc.Name, u, now)
	return result{Account: acc, Usage: u, FetchedAt: now}
}

func (c *client) fetchOne(ctx context.Context, st *store, acc *account) (*usage, error) {
	refreshed := false
	if time.Until(acc.ExpiresAt) < refreshSkew {
		if err := c.refreshTokens(ctx, st, acc); err != nil {
			return nil, err
		}
		refreshed = true
	}
	u, err := c.getUsage(ctx, acc.AccessToken)
	if isStatus(err, http.StatusUnauthorized) && !refreshed {
		// The server rejected a token we still consider valid; one refresh settles whether the grant is alive.
		if err := c.refreshTokens(ctx, st, acc); err != nil {
			return nil, err
		}
		u, err = c.getUsage(ctx, acc.AccessToken)
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (c *client) refreshTokens(ctx context.Context, st *store, acc *account) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	tok, err := c.postToken(ctx, tokenRequest{
		GrantType:    "refresh_token",
		ClientID:     oauthClientID,
		RefreshToken: acc.RefreshToken,
		Scope:        oauthScopes,
	})
	switch {
	case isStatus(err, http.StatusBadRequest), isStatus(err, http.StatusUnauthorized), isStatus(err, http.StatusForbidden):
		return fmt.Errorf("%w: run `claude-usage login %s`", errLoginRequired, acc.Name)
	case err != nil:
		return fmt.Errorf("refresh token: %w", err)
	}
	if err := st.setTokens(acc, tok); err != nil {
		return fmt.Errorf("persist refreshed token: %w", err)
	}
	return nil
}

func (c *client) getUsage(ctx context.Context, accessToken string) (*usage, error) {
	var u usage
	if err := c.doJSON(ctx, http.MethodGet, c.apiBase+usagePath, accessToken, nil, &u); err != nil {
		return nil, fmt.Errorf("fetch usage: %w", err)
	}
	return &u, nil
}

// doJSON performs one JSON round trip; any non-2xx status comes back as *httpError so callers can branch on it.
func (c *client) doJSON(ctx context.Context, method, endpoint, bearer string, body, out any) error {
	var payload io.Reader

	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("anthropic-beta", oauthBeta)
	} else {
		// Unauthenticated calls are the token endpoint; mirror the axios request shape Claude Code sends there.
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("User-Agent", c.oauthUA)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if c.debug {
		logExchange(req, resp, data)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{
			Status:     resp.StatusCode,
			Body:       preview(data),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

type httpError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *httpError) Error() string {
	switch {
	case e.Status == http.StatusTooManyRequests && e.Body != "":
		return "rate limited (HTTP 429): " + e.Body
	case e.Status == http.StatusTooManyRequests:
		return "rate limited (HTTP 429)"
	case e.Body == "":
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// logExchange prints one request/response pair to stderr. Request bodies and 2xx bodies carry tokens,
// so only the status line, a few diagnostic headers and error bodies are shown.
func logExchange(req *http.Request, resp *http.Response, body []byte) {
	fmt.Fprintf(os.Stderr, "> %s %s  ua=%q\n", req.Method, req.URL, req.Header.Get("User-Agent"))
	fmt.Fprintf(os.Stderr, "< %s  retry-after=%q  cf-ray=%q  content-type=%q  request-id=%q\n",
		resp.Status, resp.Header.Get("Retry-After"), resp.Header.Get("Cf-Ray"),
		resp.Header.Get("Content-Type"), resp.Header.Get("Request-Id"))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "< body: %s\n", preview(body))
	}
}

func isStatus(err error, status int) bool {
	var httpErr *httpError

	return errors.As(err, &httpErr) && httpErr.Status == status
}

// parseRetryAfter handles both the seconds and the HTTP-date form of the header.
func parseRetryAfter(header string) time.Duration {
	if secs, err := strconv.Atoi(header); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil && time.Until(at) > 0 {
		return time.Until(at)
	}
	return defaultRetryAfter
}

// preview keeps error bodies to one short line so an HTML error page cannot flood the table.
func preview(data []byte) string {
	text := []rune(strings.Join(strings.Fields(string(data)), " "))
	if len(text) > 160 {
		return string(text[:160]) + "…"
	}
	return string(text)
}

// mutateAccounts applies a change to the stored accounts under the config-dir lock.
func (c *client) mutateAccounts(change func(st *store, cache *usageCache) error) error {
	unlock, err := lockConfigDir(c.configDir)
	if err != nil {
		return err
	}
	defer unlock()

	st, err := openStore(c.accountsPath())
	if err != nil {
		return err
	}
	cache, err := openCache(c.cachePath())
	if err != nil {
		return err
	}
	if err := change(st, cache); err != nil {
		return err
	}
	return cache.save()
}
