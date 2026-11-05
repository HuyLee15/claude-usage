package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIsPrintMode(t *testing.T) {
	if !isPrintMode([]string{"-p", "hello"}) || !isPrintMode([]string{"--print"}) {
		t.Fatal("expected print mode to be detected")
	}
	if isPrintMode([]string{"--resume", "abc", "prompt"}) {
		t.Fatal("interactive mode must not be treated as print mode")
	}
}

func TestSessionIDFromArgs(t *testing.T) {
	id, resume, err := sessionIDFromArgs([]string{"-p", "hello", "--resume", "session-123"})
	if err != nil || id != "session-123" || !resume {
		t.Fatalf("got (%q, %v, %v)", id, resume, err)
	}
	id, resume, err = sessionIDFromArgs([]string{"-p", "hello", "--session-id=session-456"})
	if err != nil || id != "session-456" || resume {
		t.Fatalf("got (%q, %v, %v)", id, resume, err)
	}
	if _, _, err := sessionIDFromArgs([]string{"-p", "--resume"}); err == nil {
		t.Fatal("expected bare --resume to be rejected")
	}
}

func TestWithResumeSessionReplacesSessionFlags(t *testing.T) {
	got := withResumeSession([]string{"-p", "hello", "--session-id", "old", "--verbose"}, "new")
	want := []string{"-p", "hello", "--verbose", "--resume", "new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestIsUsageLimitOutput(t *testing.T) {
	for _, output := range []string{"You've hit your limit", "RATE_LIMIT_ERROR", "Usage limit has been reached"} {
		if !isUsageLimitOutput(output) {
			t.Errorf("expected usage limit detection for %q", output)
		}
	}
	for _, output := range []string{"network timeout", "overloaded_error", "invalid API key"} {
		if isUsageLimitOutput(output) {
			t.Errorf("unexpected usage limit detection for %q", output)
		}
	}
}

func TestMigrateSessionTranscriptCopiesOnlyMatchingSession(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	sessionID := "session-123"
	sourceFile := filepath.Join(source, "projects", "project-hash", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sourceFile), 0700); err != nil {
		t.Fatal(err)
	}
	const transcript = "{\"type\":\"user\",\"message\":\"hello\"}\n"
	if err := os.WriteFile(sourceFile, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	if err := migrateSessionTranscript(source, target, sessionID); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(target, "projects", "project-hash", sessionID+".jsonl")
	got, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != transcript {
		t.Fatalf("got %q, want %q", got, transcript)
	}
	if strings.Contains(targetFile, "source") {
		t.Fatal("target unexpectedly points into source profile")
	}
}

func TestSelectNextAccountExcludesPreviouslyTriedAccounts(t *testing.T) {
	results := []result{
		usageResult("used", 10, 10, nil),
		usageResult("next", 20, 20, nil),
	}
	got, err := selectNextAccount(results, map[string]bool{"used": true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.Name != "next" {
		t.Fatalf("selected %q, want next", got.Account.Name)
	}
}
