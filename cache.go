package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	cacheFileName = "cache.json"
	backoffBase   = time.Minute
	backoffMax    = 15 * time.Minute
	// backoffMaxShift caps the doubling so the shift can never overflow.
	backoffMaxShift = 5
)

// cacheEntry keeps the last good usage of one account plus its failure backoff state.
type cacheEntry struct {
	Usage     *usage    `json:"usage,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	Failures  int       `json:"failures,omitempty"`
	RetryAt   time.Time `json:"retry_at"`
	LastError string    `json:"last_error,omitempty"`
}

// usageCache is shared by every caller (CLI, dashboard, panel indicator) so they never spend the rate limit twice.
type usageCache struct {
	path    string
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

func openCache(path string) (*usageCache, error) {
	cache := &usageCache{path: path, entries: map[string]*cacheEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cache, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cache.entries); err != nil {
		// A corrupt cache only costs one extra fetch, so start fresh instead of failing every call.
		cache.entries = map[string]*cacheEntry{}
	}
	return cache, nil
}

func (c *usageCache) get(name string) cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[name]; ok {
		return *entry
	}
	return cacheEntry{}
}

func (c *usageCache) recordSuccess(name string, u *usage, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[name] = &cacheEntry{Usage: u, FetchedAt: at}
}

// recordFailure doubles the wait on each consecutive failure; a longer Retry-After from the server wins.
func (c *usageCache) recordFailure(name string, err error, at time.Time) time.Time {
	var httpErr *httpError

	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[name]
	if !ok {
		entry = &cacheEntry{}
		c.entries[name] = entry
	}
	entry.Failures++
	delay := min(backoffBase<<(min(entry.Failures, backoffMaxShift)-1), backoffMax)
	if errors.As(err, &httpErr) && httpErr.RetryAfter > delay {
		delay = min(httpErr.RetryAfter, backoffMax)
	}
	entry.RetryAt = at.Add(delay)
	entry.LastError = err.Error()
	return entry.RetryAt
}

func (c *usageCache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, name)
}

// retain drops entries of accounts that no longer exist.
func (c *usageCache) retain(accounts []*account) {
	keep := make(map[string]struct{}, len(accounts))
	for _, acc := range accounts {
		keep[acc.Name] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.entries {
		if _, ok := keep[name]; !ok {
			delete(c.entries, name)
		}
	}
}

func (c *usageCache) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	return writeFileAtomic(c.path, data)
}

// writeFileAtomic writes through a temp file so a crash mid-write never truncates the target.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
