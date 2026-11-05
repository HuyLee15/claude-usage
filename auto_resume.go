package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func isPrintMode(args []string) bool {
	for _, arg := range args {
		if arg == "-p" || arg == "--print" || strings.HasPrefix(arg, "--print=") {
			return true
		}
	}
	return false
}

// sessionIDFromArgs returns an explicit resume/session id when one is supplied.
// A bare --resume is intentionally rejected because the most-recent session is profile-local.
func sessionIDFromArgs(args []string) (string, bool, error) {
	for i, arg := range args {
		switch {
		case arg == "--resume":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", true, errors.New("Auto Resume requires an explicit session id after --resume")
			}
			return args[i+1], true, nil
		case strings.HasPrefix(arg, "--resume="):
			id := strings.TrimPrefix(arg, "--resume=")
			if id == "" {
				return "", true, errors.New("--resume requires a session id")
			}
			return id, true, nil
		case arg == "--session-id":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", false, errors.New("--session-id requires a value")
			}
			return args[i+1], false, nil
		case strings.HasPrefix(arg, "--session-id="):
			id := strings.TrimPrefix(arg, "--session-id=")
			if id == "" {
				return "", false, errors.New("--session-id requires a value")
			}
			return id, false, nil
		}
	}
	return "", false, nil
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func withResumeSession(args []string, sessionID string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--session-id" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--session-id=") {
			continue
		}
		if arg == "--resume" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--resume=") {
			continue
		}
		out = append(out, arg)
	}
	return append(out, "--resume", sessionID)
}

func launchClaudeCapture(ctx context.Context, executable string, args []string, profile string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	var stdout, stderr tailBuffer
	stdout.limit, stderr.limit = 1<<20, 1<<20
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdout)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Env = claudeProfileEnv(os.Environ(), profile)
	err := cmd.Run()
	return stdout.String() + "\n" + stderr.String(), err
}

func isUsageLimitOutput(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{
		"you've hit your limit",
		"you have hit your limit",
		"usage limit reached",
		"usage limit has been reached",
		"rate_limit_error",
		"you've reached your usage limit",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// tailBuffer retains only the last limit bytes, keeping error detection bounded for long print runs.
type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit <= 0 {
		return n, nil
	}
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	if overflow := len(b.data) + len(p) - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.data) }

// accountWithSession locates an existing session in exactly one configured profile.
func accountWithSession(root string, results []result, sessionID string) (result, error) {
	var owner result
	found := false
	for _, candidate := range results {
		if candidate.Account == nil {
			continue
		}
		profile, err := accountProfile(root, candidate.Account.Name)
		if err != nil {
			return result{}, err
		}
		if hasSessionTranscript(profile, sessionID) {
			if found {
				return result{}, fmt.Errorf("session %s exists in more than one account profile; refusing ambiguous resume", sessionID)
			}
			owner, found = candidate, true
		}
	}
	if !found {
		return result{}, fmt.Errorf("session %s was not found in any configured Claude account profile", sessionID)
	}
	return owner, nil
}

func hasSessionTranscript(profile, sessionID string) bool {
	found := false
	_ = filepath.WalkDir(filepath.Join(profile, "projects"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && entry.Name() == sessionID+".jsonl" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// migrateSessionTranscript copies only the selected session's JSONL transcript. It never copies
// credentials, settings, or other account-profile data.
func migrateSessionTranscript(sourceProfile, targetProfile, sessionID string) error {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\`) {
		return errors.New("invalid session id")
	}
	sourceProjects := filepath.Join(sourceProfile, "projects")
	var source string
	err := filepath.WalkDir(sourceProjects, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && entry.Name() == sessionID+".jsonl" {
			source = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if source == "" {
		return fmt.Errorf("session transcript %s.jsonl was not found under %s/projects", sessionID, sourceProfile)
	}
	rel, err := filepath.Rel(sourceProjects, source)
	if err != nil {
		return err
	}
	target := filepath.Join(targetProfile, "projects", rel)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(target)
		return err
	}
	return out.Close()
}
