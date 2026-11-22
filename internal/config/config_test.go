package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAbsentGivesDefaults(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "l'utilisateur" || got.Command != filepath.Join(home, ".local", "bin", "paul") ||
		len(got.Roots) != 0 || len(got.Journals) != 0 || len(got.Labels) != 0 || got.Sync != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadExpandsTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := Load(write(t, "roots = [\"~/code\"]\njournals = [\"~/j\"]\ncommand = \"~/bin/paul\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Roots[0] != filepath.Join(home, "code") || got.Journals[0] != filepath.Join(home, "j") ||
		got.Command != filepath.Join(home, "bin", "paul") {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	_, err := Load(write(t, "colour = 1\n"))
	if err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsBadLabel(t *testing.T) {
	_, err := Load(write(t, "[labels]\nx = \"perso\"\n"))
	if err == nil || !strings.Contains(err.Error(), "x") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadReadsSync(t *testing.T) {
	got, err := Load(write(t, "[sync]\nrepository = \"git@example.com:mon-compte/paul.git\"\nencryption = \"age\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Sync == nil || got.Sync.Repository != "git@example.com:mon-compte/paul.git" || got.Sync.Encryption != "age" {
		t.Fatalf("got %+v", got.Sync)
	}
}

func TestEmptyCommandDisablesLink(t *testing.T) {
	got, err := Load(write(t, "command = \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "" {
		t.Fatalf("Command = %q", got.Command)
	}
}
