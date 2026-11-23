package paths

import (
	"os"
	"path/filepath"
)

type Paths struct {
	Home, Transcripts string
}

func (p Paths) DB() string          { return filepath.Join(p.Home, "paul.db") }
func (p Paths) Log() string         { return filepath.Join(p.Home, "paul.log") }
func (p Paths) IndexLock() string   { return filepath.Join(p.Home, "index.lock") }
func (p Paths) SummaryLock() string { return filepath.Join(p.Home, "summary.lock") }
func (p Paths) InstallLock() string { return filepath.Join(p.Home, "install.lock") }
func (p Paths) Bin() string         { return filepath.Join(p.Home, "bin") }
func (p Paths) Config() string      { return filepath.Join(p.Home, "config.toml") }
func (p Paths) Journals() string    { return filepath.Join(p.Home, "journals") }
func (p Paths) Archive() string     { return filepath.Join(p.Journals(), "archive.jsonl") }
func (p Paths) SyncDir() string     { return filepath.Join(p.Home, "sync") }

func (p Paths) BinDir() string {
	user, _ := os.UserHomeDir()
	return envOr("PAUL_BIN_DIR", filepath.Join(user, ".local", "bin"))
}

// EnsureHome chmods even when the directory exists: MkdirAll leaves an
// existing directory's mode untouched.
func (p Paths) EnsureHome() error {
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.Home, 0o700)
}

func Default() Paths {
	user, _ := os.UserHomeDir()
	return Paths{
		Home:        envOr("PAUL_HOME", filepath.Join(user, ".claude", "paul")),
		Transcripts: envOr("PAUL_TRANSCRIPTS", filepath.Join(user, ".claude", "projects")),
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
