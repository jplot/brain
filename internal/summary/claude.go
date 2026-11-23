package summary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const Timeout = 300 * time.Second

// Replaced by tests, to see a call cut short without waiting 300 s.
var timeout = Timeout

// A child left by claude keeps stdout open after the kill: the fake claude's
// sleep held Wait for its whole 5 s instead of the 200 ms timeout.
const waitDelay = time.Second

type Result struct {
	Data    map[string]any
	CostUSD *float64
}

type Runner func(prompt, system string, schema any) (Result, error)

// Error never carries the prompt nor claude's output: it ends up in the log.
type Error struct {
	msg  string
	Cost *float64
}

func (e *Error) Error() string { return e.msg }

func RunClaude(prompt, system string, schema any) (Result, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--safe-mode", "--model", "haiku", "--tools", "",
		"--no-session-persistence", "--output-format", "json", "--json-schema", string(encoded),
		"--system-prompt", system)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	if ctx.Err() != nil {
		return Result{}, &Error{msg: "claude n'a pas répondu à temps"}
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return Result{}, &Error{msg: "claude inutilisable"}
	}
	var payload map[string]any
	json.Unmarshal(stdout.Bytes(), &payload)
	var cost *float64
	if v, ok := payload["total_cost_usd"].(float64); ok {
		cost = &v
	}
	if exit != nil {
		return Result{}, &Error{msg: fmt.Sprintf("claude a échoué (code %d)", exit.ExitCode()), Cost: cost}
	}
	if payload == nil {
		return Result{}, &Error{msg: "sortie de claude illisible"}
	}
	if failed, _ := payload["is_error"].(bool); failed {
		return Result{}, &Error{msg: "claude a rendu une erreur", Cost: cost}
	}
	output, ok := payload["structured_output"].(map[string]any)
	if !ok {
		return Result{}, &Error{msg: "sortie structurée absente", Cost: cost}
	}
	return Result{Data: output, CostUSD: cost}, nil
}
