package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

var errLoginRequired = errors.New("login required")

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

type result struct {
	Account   *account
	Usage     *usage
	Err       error
	FetchedAt time.Time
}

type client struct {
	httpClient *http.Client
	apiBase    string
	oauthBase  string
}

func newClient(cfg settings) *client {
	return &client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		apiBase:    cfg.apiBase,
		oauthBase:  cfg.oauthBase,
	}
}

// fetchAll queries every account in parallel and returns results in the same order.
func (c *client) fetchAll(ctx context.Context, st *store, accounts []*account) []result {
	var wg sync.WaitGroup

	results := make([]result, len(accounts))
	for i, acc := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := c.fetchOne(ctx, st, acc)
			results[i] = result{Account: acc, Usage: u, Err: err, FetchedAt: time.Now()}
		}()
	}
	wg.Wait()
	return results
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
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-usage/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("anthropic-beta", oauthBeta)
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
	case e.Status == http.StatusTooManyRequests:
		return fmt.Sprintf("rate limited (retry in %s)", e.RetryAfter.Round(time.Second))
	case e.Body == "":
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
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
