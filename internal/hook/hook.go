// Package hook implements the Claude Code hooks. A hook never delays Claude Code: its only output is
// the memo or the injection on stdout, and every failure goes to paul.log.
package hook

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/memo"
	"github.com/jplot/brain/internal/paths"
)

const (
	stdinBudget  = 200 * time.Millisecond
	startBudget  = 500 * time.Millisecond
	promptBudget = 150 * time.Millisecond
	// A hook that waits on a locked database would blow its budget.
	busyTimeout = 100 * time.Millisecond
)

var executable = os.Executable

type payload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"`
	Prompt         string `json:"prompt"`
}

// Run executes one hook and always returns 0.
func Run(event string, p paths.Paths, stdin io.Reader, stdout io.Writer) int {
	in := readPayload(stdin)
	switch event {
	case "start":
		detach(summarizeArgs(""))
		if err := LinkCommand(p, Version); err != nil {
			log.Printf("hook start : commande du terminal : %v", err)
		}
		if in.Source == "startup" || in.Source == "clear" {
			emit(stdout, "SessionStart", startBudget, func() (string, func() error, error) {
				inj, err := withDB(p, func(d *sql.DB) (memo.Injection, error) {
					return memo.PlanStart(d, cwdOrCurrent(in.Cwd), in.SessionID, time.Now())
				})
				return inj.Text, recorder(p, inj), err
			})
		}
	case "prompt":
		if in.SessionID != "" && in.Prompt != "" {
			emit(stdout, "UserPromptSubmit", promptBudget, func() (string, func() error, error) {
				inj, err := withDB(p, func(d *sql.DB) (memo.Injection, error) { return memo.Plan(d, in.SessionID, in.Prompt) })
				return inj.Text, recorder(p, inj), err
			})
		}
	case "end":
		if info, err := os.Stat(in.TranscriptPath); in.SessionID != "" && err == nil && info.Mode().IsRegular() {
			detach(summarizeArgs(in.SessionID))
		} else {
			detach([]string{"index"})
		}
	default:
		log.Printf("hook : événement inconnu : %q", event)
	}
	return 0
}

// Version is the build version the start hook links the terminal command to; "dev" links nothing.
var Version = "dev"

func summarizeArgs(session string) []string {
	if session == "" {
		return []string{"summarize"}
	}
	return []string{"summarize", "--session", session}
}

func cwdOrCurrent(cwd string) string {
	if cwd != "" {
		return cwd
	}
	wd, _ := os.Getwd()
	return wd
}

func recorder(p paths.Paths, inj memo.Injection) func() error {
	return func() error {
		_, err := withDB(p, func(d *sql.DB) (struct{}, error) { return struct{}{}, inj.Record(d, time.Now()) })
		return err
	}
}

func readPayload(r io.Reader) payload {
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buf := make([]byte, 65536)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	var data []byte
	deadline := time.After(stdinBudget)
	for done := false; !done; {
		select {
		case c, ok := <-chunks:
			if !ok {
				done = true
			}
			data = append(data, c...)
		case <-deadline:
			done = true
		}
	}
	var in payload
	if err := json.Unmarshal(data, &in); err != nil {
		return payload{}
	}
	return in
}

func withDB[T any](p paths.Paths, f func(*sql.DB) (T, error)) (T, error) {
	d, err := db.Open(p, busyTimeout)
	if err != nil {
		var zero T
		return zero, err
	}
	defer d.Close()
	return f(d)
}

type output struct {
	HookSpecificOutput specific `json:"hookSpecificOutput"`
}

type specific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// emit runs work against a deadline; on timeout the work is abandoned to the process exit.
// record, when not nil, runs only once the text was written.
func emit(stdout io.Writer, name string, budget time.Duration, work func() (string, func() error, error)) {
	type result struct {
		text   string
		record func() error
		err    error
	}
	done := make(chan result, 1)
	go func() {
		text, record, err := work()
		done <- result{text, record, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			log.Printf("hook %s : %v", name, r.err)
			return
		}
		if r.text == "" {
			return
		}
		// Only now: children are spawned by then and must not inherit an ignored SIGPIPE.
		// A closed stdout then gives EPIPE instead of killing the hook.
		signal.Ignore(syscall.SIGPIPE)
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		enc.Encode(output{specific{name, r.text}})
		if _, err := stdout.Write(bytes.TrimRight(out.Bytes(), "\n")); err != nil {
			log.Printf("hook %s : écriture : %v", name, err)
			return
		}
		if r.record != nil {
			if err := r.record(); err != nil {
				log.Printf("hook %s : enregistrement : %v", name, err)
			}
		}
	case <-time.After(budget):
		log.Printf("hook %s : délai de %v dépassé", name, budget)
	}
}

// detach starts paul in its own session and does not wait for it.
func detach(args []string) {
	exe, err := executable()
	if err != nil {
		log.Printf("hook : lancement impossible : %v", err)
		return
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("hook : lancement impossible : %v", err)
		return
	}
	go cmd.Wait()
}
