package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	// The first exec of a fresh script took over 2 s under parallel load on macOS.
	if err := exec.Command(filepath.Join(dir, "git")).Run(); err != nil {
		t.Fatal(err)
	}
}

func TestUserNameFirstWord(t *testing.T) {
	fakeGit(t, "echo 'Camille Dupont'")
	if got := UserName(); got != "Camille" {
		t.Fatalf("got %q", got)
	}
}

func TestUserNameFallback(t *testing.T) {
	fakeGit(t, "echo")
	if got := UserName(); got != "l'utilisateur" {
		t.Fatalf("got %q", got)
	}
}
