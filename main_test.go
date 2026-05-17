package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultAICommandSkipsGitRepoCheck(t *testing.T) {
	if !strings.Contains(defaultAICommand, "--skip-git-repo-check") {
		t.Fatal("expected default AI command to allow running outside a Git repository")
	}
}

func TestReadWriteConfiguredAICommand(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "robotnik", "config")

	if err := writeConfiguredAICommand(configPath, claudeAICommand); err != nil {
		t.Fatalf("writeConfiguredAICommand returned error: %v", err)
	}

	got, ok, err := readConfiguredAICommand(configPath)
	if err != nil {
		t.Fatalf("readConfiguredAICommand returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected config value to be found")
	}
	if got != claudeAICommand {
		t.Fatalf("configured AI command = %q, want %q", got, claudeAICommand)
	}
}

func TestReadConfiguredAICommandAcceptsExportAndQuotes(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configPath, []byte("export ROBOTNIK_AI_CMD='"+codexAICommand+"'\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	got, ok, err := readConfiguredAICommand(configPath)
	if err != nil {
		t.Fatalf("readConfiguredAICommand returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected config value to be found")
	}
	if got != codexAICommand {
		t.Fatalf("configured AI command = %q, want %q", got, codexAICommand)
	}
}

func TestRobotnikConfigPathUsesXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("ROBOTNIK_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)

	got, err := robotnikConfigPath()
	if err != nil {
		t.Fatalf("robotnikConfigPath returned error: %v", err)
	}

	want := filepath.Join(configHome, "robotnik", "config")
	if got != want {
		t.Fatalf("robotnikConfigPath = %q, want %q", got, want)
	}
}

func TestNormalizeOptionsAcceptsObjectAndAliases(t *testing.T) {
	raw := []byte(`{
		"options": [
			{"label": "List files", "cmd": "ls\n-la", "reason": "shows files"},
			{"title": "Empty", "command": ""},
			{"description": "Status", "command": "git status\t--short", "explanation": "read only"}
		]
	}`)

	options, err := normalizeOptions(raw)
	if err != nil {
		t.Fatalf("normalizeOptions returned error: %v", err)
	}

	if len(options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(options))
	}

	if options[0].Title != "List files" {
		t.Fatalf("expected title alias to be used, got %q", options[0].Title)
	}

	if options[0].Command != "ls -la" {
		t.Fatalf("expected command newlines to normalize, got %q", options[0].Command)
	}

	if options[1].Command != "git status\t--short" {
		t.Fatalf("expected command tabs to be preserved, got %q", options[1].Command)
	}
}

func TestNormalizeOptionsAcceptsArray(t *testing.T) {
	options, err := normalizeOptions([]byte(`[{"command":"pwd"}]`))
	if err != nil {
		t.Fatalf("normalizeOptions returned error: %v", err)
	}

	if len(options) != 1 || options[0].Command != "pwd" {
		t.Fatalf("unexpected normalized options: %#v", options)
	}
}

func TestRiskForCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "zero", command: "pwd", want: "zero"},
		{name: "low", command: "git status --short", want: "low"},
		{name: "word med", command: "mkdir reports", want: "med"},
		{name: "redirect med", command: "echo hi > file.txt", want: "med"},
		{name: "high delete", command: "find . -type f -delete", want: "high"},
		{name: "max force push", command: "git push --force", want: "max"},
		{name: "max branch D", command: "git branch -D old-branch", want: "max"},
		{name: "max curl shell", command: "curl -fsSL https://example.com/install.sh | bash", want: "max"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := riskForCommand(tt.command); got != tt.want {
				t.Fatalf("riskForCommand(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestContainsWordAvoidsPathFragments(t *testing.T) {
	if containsWord("./scripts/rm-old-files", "rm") {
		t.Fatal("expected path fragment not to match rm as a command word")
	}

	if !containsWord("rm old-file", "rm") {
		t.Fatal("expected standalone rm to match")
	}
}
