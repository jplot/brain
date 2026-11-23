package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type SyncConfig struct {
	Repository string `toml:"repository"`
	Encryption string `toml:"encryption"`
}

type Config struct {
	Sync *SyncConfig `toml:"sync"`
}

func Load(path string) (Config, error) {
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
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
	return cfg, nil
}

func UserName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "config", "--global", "user.name").Output()
	if err != nil {
		return "l'utilisateur"
	}
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		return fields[0]
	}
	return "l'utilisateur"
}
