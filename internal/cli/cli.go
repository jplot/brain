// Package cli is the paul command line: parsing, dispatch and the French messages around the other packages.
package cli

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/memory"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/search"
	"github.com/jplot/brain/internal/subjects"
)

// Version is set by cmd/paul from the -X main.version linker flag.
var Version = "dev"

const (
	busyTimeout = 30 * time.Second
	allPoints   = 1 << 30
	usage       = `paul : mémoire persistante pour Claude Code

Usage : paul <commande> [options]

  search <mots>                 --subject --since --until --limit
  souvenirs [mots]              --kind --subject --open --history --limit
  session <id>
  day <AAAA-MM-JJ>
  subjects [mots]               l'arbre des sujets et leurs liens
  memo                          --subject
  summarize                     --session --limit --all --force
  forget point|session <id>     --yes
  index
  rebuild
  web                           --no-open
  hook start|prompt|end
  migrate                       --from --root
  sync                          pas encore disponible
`
)

type usageError string

func (e usageError) Error() string { return string(e) }

func usagef(format string, args ...any) error { return usageError(fmt.Sprintf(format, args...)) }

type env struct {
	p        paths.Paths
	out, err io.Writer
}

type command func(e *env, args []string) (int, error)

func commands() map[string]command {
	notYet := func(name string) command {
		return func(*env, []string) (int, error) { return 0, usagef("%s : pas encore disponible", name) }
	}
	return map[string]command{
		"search": searchCmd, "souvenirs": souvenirs, "session": session, "day": day,
		"subjects": subjectsTree, "memo": memoCmd, "summarize": summarize, "forget": forget,
		"index": indexCmd, "rebuild": rebuild,
		"web": webCmd, "hook": hookCmd, "migrate": migrateCmd, "sync": notYet("sync"),
	}
}

// Main runs one command and returns the exit code: 2 for a usage error, 1 for a failure.
// The standard logger goes to paul.log for the whole run, never to a terminal.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "--version":
		fmt.Fprintf(stdout, "paul %s\n", Version)
		return 0
	case "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	run, known := commands()[args[0]]
	if !known {
		fmt.Fprintf(stderr, "paul : commande inconnue : %s\n\n%s", args[0], usage)
		return 2
	}
	p := paths.Default()
	defer redirectLog(p)()
	code, err := run(&env{p: p, out: stdout, err: stderr}, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err == nil {
		return code
	}
	var ambiguousSession *search.ErrAmbiguousSession
	var ambiguousSubject *subjects.ErrAmbiguousSubject
	var unknownSubject *subjects.ErrUnknownSubject
	var missingPoint *memory.ErrPointNotFound
	if errors.As(err, new(usageError)) || errors.Is(err, search.ErrEmptyQuery) || errors.As(err, &ambiguousSession) ||
		errors.As(err, &ambiguousSubject) || errors.As(err, &unknownSubject) || errors.As(err, &missingPoint) {
		fmt.Fprintf(stderr, "paul : %v\n", err)
		return 2
	}
	log.Printf("%s : %v", args[0], err)
	fmt.Fprintf(stderr, "paul : échec de %s : voir paul.log\n", args[0])
	return 1
}

func redirectLog(p paths.Paths) (restore func()) {
	saved := log.Writer()
	var sink io.Writer = io.Discard
	closer := func() {}
	if err := p.EnsureHome(); err == nil {
		if f, err := os.OpenFile(p.Log(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			sink, closer = f, func() { f.Close() }
		}
	}
	log.SetOutput(sink)
	return func() {
		log.SetOutput(saved)
		closer()
	}
}

func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var after []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, after = args[:i], args[i+1:]
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usagef("%s : %s", fs.Name(), french(err))
		}
		if fs.NArg() == 0 {
			return append(positional, after...), nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func french(err error) string {
	text := err.Error()
	switch {
	case strings.HasPrefix(text, "flag provided but not defined: "):
		return "option inconnue : " + strings.TrimPrefix(text, "flag provided but not defined: ")
	case strings.HasPrefix(text, "flag needs an argument: "):
		return "valeur manquante pour " + strings.TrimPrefix(text, "flag needs an argument: ")
	}
	return "option invalide : " + text
}

func newFlags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }

func count(text string, fallback int) (int, error) {
	if text == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || strconv.Itoa(n) != text {
		return 0, usagef("nombre invalide : %s", text)
	}
	return n, nil
}

func parseDay(text string) (*time.Time, error) {
	if text == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02", text, time.Local)
	if err != nil {
		return nil, usagef("date invalide : %s (attendu AAAA-MM-JJ)", text)
	}
	return &t, nil
}

func open(e *env) (*sql.DB, error) { return db.Open(e.p, busyTimeout) }

func openFresh(e *env) (*sql.DB, error) {
	conn, err := open(e)
	if err != nil {
		return nil, err
	}
	if err := index.Refresh(conn, e.p); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func flat(text string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)), " ")
}

func local(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
