package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type SyncConfig struct {
	Repository string `toml:"repository"`
	Encryption string `toml:"encryption"`
}

type Config struct {
	User     string            `toml:"user"`
	Roots    []string          `toml:"roots"`
	Journals []string          `toml:"journals"`
	Command  string            `toml:"command"`
	Labels   map[string]string `toml:"labels"`
	Sync     *SyncConfig       `toml:"sync"`
}

func defaults() Config {
	return Config{User: "l'utilisateur", Command: "~/.local/bin/paul"}
}

func Load(path string) (Config, error) {
	cfg := defaults()
	meta, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return expand(cfg), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("configuration %s : %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("configuration %s : clé inconnue %s", path, strings.Join(keys, ", "))
	}
	for _, key := range sortedKeys(cfg.Labels) {
		if label := cfg.Labels[key]; label != "personal" && label != "work" {
			return Config{}, fmt.Errorf("configuration %s : labels.%s vaut %q, attendu personal ou work", path, key, label)
		}
	}
	return expand(cfg), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func expand(cfg Config) Config {
	for i, r := range cfg.Roots {
		cfg.Roots[i] = expandTilde(r)
	}
	for i, j := range cfg.Journals {
		cfg.Journals[i] = expandTilde(j)
	}
	cfg.Command = expandTilde(cfg.Command)
	return cfg
}

func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
