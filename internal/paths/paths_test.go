package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultUsesEnvironment(t *testing.T) {
	t.Setenv("PAUL_HOME", "/x/home")
	t.Setenv("PAUL_TRANSCRIPTS", "/x/transcripts")
	got := Default()
	if got.Home != "/x/home" || got.Transcripts != "/x/transcripts" {
		t.Fatalf("got %+v", got)
	}
}

func TestDefaultFallsBackUnderUserHome(t *testing.T) {
	t.Setenv("PAUL_HOME", "")
	t.Setenv("PAUL_TRANSCRIPTS", "")
	t.Setenv("HOME", "/u")
	got := Default()
	if got.Home != "/u/.claude/paul" || got.Transcripts != "/u/.claude/projects" {
		t.Fatalf("got %+v", got)
	}
}

func TestFileNames(t *testing.T) {
	p := Paths{Home: "/h"}
	for got, want := range map[string]string{
		p.DB(): "/h/paul.db", p.Log(): "/h/paul.log", p.IndexLock(): "/h/index.lock",
		p.SummaryLock(): "/h/summary.lock", p.InstallLock(): "/h/install.lock",
		p.Bin(): "/h/bin", p.Config(): "/h/config.toml",
		p.Journals(): "/h/journals", p.SyncDir(): "/h/sync",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestEnsureHomeIs700(t *testing.T) {
	home := filepath.Join(t.TempDir(), "a", "paul")
	p := Paths{Home: home}
	if err := p.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(home)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestBinDirDefault(t *testing.T) {
	t.Setenv("PAUL_BIN_DIR", "")
	t.Setenv("HOME", "/u")
	if got := (Paths{}).BinDir(); got != "/u/.local/bin" {
		t.Fatalf("got %s", got)
	}
}

func TestBinDirFromEnv(t *testing.T) {
	t.Setenv("PAUL_BIN_DIR", "/x/bin")
	if got := (Paths{}).BinDir(); got != "/x/bin" {
		t.Fatalf("got %s", got)
	}
}
