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

func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestLoadAbsentGivesZeroConfig(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Sync != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadRejectsOldKeys(t *testing.T) {
	for key, content := range map[string]string{
		"roots":    "roots = [\"~/code\"]\n",
		"labels":   "[labels]\nx = \"work\"\n",
		"user":     "user = \"x\"\n",
		"journals": "journals = []\n",
		"command":  "command = \"\"\n",
		"colour":   "colour = 1\n",
	} {
		_, err := Load(write(t, content))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v", key, err)
		}
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

func TestUserNameFirstWord(t *testing.T) {
	fakeGit(t, "echo 'Camille Dupont'")
	if got := UserName(); got != "Camille" {
		t.Fatalf("got %q", got)
	}
}

func TestUserNameFallback(t *testing.T) {
	fakeGit(t, "exit 0")
	if got := UserName(); got != "l'utilisateur" {
		t.Fatalf("got %q", got)
	}
}
