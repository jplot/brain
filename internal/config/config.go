package config

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

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
