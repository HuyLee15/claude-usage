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

const accountsFileName = "accounts.json"

var errAccountNotFound = errors.New("account not found")

// account is one authorized Claude subscription plus the tokens needed to query its usage.
type account struct {
	Name         string    `json:"name"`
	Email        string    `json:"email,omitempty"`
	Organization string    `json:"organization,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AddedAt      time.Time `json:"added_at"`
}

// applyTokens records a grant; a refresh response may omit the refresh token, in which case the old one stays valid.
func (a *account) applyTokens(tok *tokenResponse) {
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.Account.EmailAddress != "" {
		a.Email = tok.Account.EmailAddress
	}
	if tok.Organization.Name != "" {
		a.Organization = tok.Organization.Name
	}
}

type accountsFile struct {
	Accounts []*account `json:"accounts"`
}

// store owns accounts.json; every mutation takes the mutex because parallel fetches refresh tokens concurrently.
type store struct {
	path     string
	mu       sync.Mutex
	accounts []*account
}

func openStore(path string) (*store, error) {
	st := &store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var file accountsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	st.accounts = file.Accounts
	return st, nil
}

func (s *store) list() []*account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*account(nil), s.accounts...)
}

func (s *store) upsert(acc *account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.accounts {
		if existing.Name == acc.Name {
			acc.AddedAt = existing.AddedAt
			s.accounts[i] = acc
			return s.save()
		}
	}
	s.accounts = append(s.accounts, acc)
	return s.save()
}

func (s *store) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, acc := range s.accounts {
		if acc.Name == name {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			return s.save()
		}
	}
	return errAccountNotFound
}

// setTokens persists a rotated grant immediately: a refresh token lost before it hits disk means a forced re-login.
func (s *store) setTokens(acc *account, tok *tokenResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc.applyTokens(tok)
	return s.save()
}

// save writes through a temp file so a crash mid-write never truncates the token store. Caller holds mu.
func (s *store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(accountsFile{Accounts: s.accounts}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode accounts: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace %s: %w", s.path, err)
	}
	return nil
}
