// Package migrate imports, once, the database of the Python version.
package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/repo"
	"github.com/jplot/brain/internal/subjects"
)

var rebuild = index.Rebuild

var ErrAlreadyDone = errors.New("paul.db existe déjà : la reprise a déjà eu lieu")

var (
	errInterrupted = errors.New("interrompue avant la fin : rien n'est écrit, la copie de travail est effacée")
	kinds          = map[string]string{"sujet": "topic", "decision": "decision", "suspens": "pending", "fait": "fact"}
)

type counts struct {
	sessions, episodes, points, subjects, archived int
	rootSeen                                       bool
	forgotten                                      map[string]bool
}

// The new base is built in a staging folder and linked in place once complete: an import that
// fails before leaves no paul.db and can run again; after, only the index may be missing.
func Run(old, root string, p paths.Paths, out io.Writer) (err error) {
	// The staging folder holds a plain copy of the old memory: an interrupted run removes it
	// itself, and a killed one leaves it to the next run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	committed := false
	defer func() {
		if err != nil && ctx.Err() != nil && !committed {
			err = errInterrupted
		}
	}()
	leftovers, err := filepath.Glob(filepath.Join(p.Home, "migrate-*"))
	if err != nil {
		return err
	}
	for _, dir := range leftovers {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.DB()); err == nil {
		return ErrAlreadyDone
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if old, err = filepath.Abs(old); err != nil {
		return err
	}
	if root, err = filepath.Abs(root); err != nil {
		return err
	}
	if err := p.EnsureHome(); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(p.Home, "migrate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	staged := paths.Paths{Home: stage, Transcripts: p.Transcripts}
	copied, err := copyOld(old, stage)
	if err != nil {
		return err
	}
	vocalPath := filepath.Join(filepath.Dir(old), "vocal", "conversation.jsonl")
	transcripts, err := filepath.Glob(filepath.Join(p.Transcripts, "*", "*.jsonl"))
	if err != nil {
		return err
	}
	real := resolver{}
	kept := map[string]bool{}
	for _, name := range transcripts {
		kept[real.path(name)] = true
	}
	_, err = os.Stat(vocalPath)
	converted := err == nil
	// The old version keyed vocal lines, and only them, "vocal:": a copied base still names the
	// journal at its old path. Without the journal, the old index is all that is left of them.
	read := func(file, key string) bool {
		return kept[real.path(file)] || converted && strings.HasPrefix(key, "vocal:")
	}
	c, err := importBase(ctx, copied, root, staged, read)
	if err != nil {
		return err
	}
	vocal, dropped, err := convertVocal(vocalPath, staged.Journals(), c.forgotten)
	if err != nil {
		return err
	}
	if err := syncFile(staged.DB()); err != nil {
		return err
	}
	var journals []string
	if vocal > 0 {
		journals = append(journals, "vocal.jsonl")
	}
	if c.archived > 0 {
		journals = append(journals, filepath.Base(p.Archive()))
	}
	if ctx.Err() != nil {
		return errInterrupted
	}
	if err := commit(staged, p, journals); err != nil {
		return err
	}
	// From here the import is done: Ctrl-C during the index rebuild stops paul as usual.
	committed = true
	stop()
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("reprise faite, mais la copie de l'ancienne base reste dans %s : %w", stage, err)
	}
	fmt.Fprintf(out, "Reprise terminée depuis %s.\n", old)
	if !c.rootSeen {
		fmt.Fprintf(out, "Racine des projets absente de l'ancienne base : %s (option --root)\n", root)
	}
	fmt.Fprintf(out, "Sessions résumées : %d\nÉpisodes : %d\nPoints : %d\nSujets créés : %d\nLignes vocales converties : %d\nMessages archivés : %d\n",
		c.sessions, c.episodes, c.points, c.subjects, vocal, c.archived)
	if dropped > 0 {
		fmt.Fprintf(out, "Lignes vocales écartées (sessions oubliées) : %d\n", dropped)
	}
	// The import is committed and paul.db exists, so a retry would be refused: exit 0 and let
	// the next refresh index everything, its files table being empty.
	stats, err := rebuild(p)
	if err != nil {
		log.Printf("reconstruction de l'index après la reprise échouée : %v", err)
		fmt.Fprintln(out, "Index non reconstruit : il le sera au prochain lancement de paul (détail dans paul.log).")
		return nil
	}
	fmt.Fprintf(out, "Index reconstruit : %d fichiers, %d messages\n", stats.Files, stats.Messages)
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// A hard link never replaces a file: if a hook created paul.db meanwhile, the import loses.
func commit(staged, p paths.Paths, journals []string) error {
	var linked []string
	undo := func(err error) error {
		for _, path := range linked {
			err = errors.Join(err, os.Remove(path))
		}
		return err
	}
	if len(journals) > 0 {
		if err := os.MkdirAll(p.Journals(), 0o700); err != nil {
			return err
		}
	}
	for _, name := range journals {
		path := filepath.Join(p.Journals(), name)
		if err := os.Link(filepath.Join(staged.Journals(), name), path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				err = fmt.Errorf("%s existe déjà : la reprise ne le remplace pas, déplacez-le puis relancez", path)
			}
			return undo(err)
		}
		linked = append(linked, path)
	}
	err := os.Link(staged.DB(), p.DB())
	if errors.Is(err, fs.ErrExist) {
		err = ErrAlreadyDone
	}
	if err != nil {
		return undo(err)
	}
	return nil
}

// The old index may name a transcript through a symlinked folder that PAUL_TRANSCRIPTS
// reaches by its real path, or the reverse: both are compared once resolved.
type resolver map[string]string

func (r resolver) path(name string) string {
	dir := filepath.Dir(name)
	real, ok := r[dir]
	if !ok {
		real = dir
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			real = resolved
		}
		r[dir] = real
	}
	return filepath.Join(real, filepath.Base(name))
}

// Even read-only, SQLite leaves -wal and -shm files beside the base it opens: it reads a copy.
func copyOld(old, dir string) (string, error) {
	copied := filepath.Join(dir, "brain.db")
	if err := copyFile(old, copied); errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("ancienne base introuvable : %s", old)
	} else if err != nil {
		return "", err
	}
	if err := copyFile(old+"-wal", copied+"-wal"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return copied, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

func importBase(ctx context.Context, old, root string, staged paths.Paths, read func(file, key string) bool) (counts, error) {
	var c counts
	src, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: old, RawQuery: "mode=ro"}).String())
	if err != nil {
		return c, err
	}
	defer src.Close()
	// One read transaction: every table comes from the same snapshot.
	from, err := src.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer from.Rollback()
	dst, err := db.Open(staged, 30*time.Second)
	if err != nil {
		return c, err
	}
	defer dst.Close()
	to, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer to.Rollback()
	m := &mapper{ctx: ctx, to: to, root: root, uids: map[string]*string{}, c: &c}
	steps := []func(*sql.Tx) error{m.projects, m.episodes, m.points, func(from *sql.Tx) (err error) {
		c.archived, err = archive(from, read, staged.Archive())
		return err
	}}
	for _, t := range carried {
		steps = append(steps, func(from *sql.Tx) error { return copyTable(from, to, t) })
	}
	for _, step := range steps {
		if err := step(from); err != nil {
			return c, err
		}
	}
	if err := to.QueryRow("SELECT count(*), (SELECT count(DISTINCT session_id) FROM episodes) FROM subjects").Scan(&c.subjects, &c.sessions); err != nil {
		return c, err
	}
	if c.forgotten, err = forgottenSessions(to); err != nil {
		return c, err
	}
	if err := to.Commit(); err != nil {
		return c, err
	}
	// Only paul.db is linked in place: nothing may stay in its WAL.
	var busy, pages, moved int
	if err := dst.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &moved); err != nil {
		return c, err
	}
	if busy != 0 {
		return c, errors.New("checkpoint de la nouvelle base bloqué")
	}
	return c, nil
}

func forgottenSessions(q *sql.Tx) (map[string]bool, error) {
	rows, err := q.Query("SELECT session_id FROM forgotten_sessions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		set[id] = true
	}
	return set, rows.Err()
}

type mapper struct {
	ctx  context.Context
	to   *sql.Tx
	root string
	uids map[string]*string
	c    *counts
}

// subject is nil for the old projects root, which held what belonged to no project.
func (m *mapper) subject(path string) (*string, error) {
	if uid, ok := m.uids[path]; ok {
		return uid, nil
	}
	var found *string
	if path == m.root {
		m.c.rootSeen = true
	} else {
		uid, err := m.resolve(path)
		if err != nil {
			return nil, err
		}
		found = &uid
	}
	m.uids[path] = found
	return found, nil
}

func (m *mapper) resolve(path string) (string, error) {
	info, ok, err := repo.Of(m.ctx, path)
	if errors.Is(err, repo.ErrGit) {
		return "", fmt.Errorf("git est nécessaire pour reconnaître les dépôts des anciens projets (%s) : %w", path, err)
	}
	if err != nil {
		return "", err
	}
	if ok {
		return subjects.EnsureRepo(m.to, info.Identity, info.Name)
	}
	return subjects.Create(m.to, filepath.Base(path), nil, nil)
}

func (m *mapper) projects(from *sql.Tx) error {
	rows, err := from.Query("SELECT path FROM projects ORDER BY path")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		if _, err := m.subject(path); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *mapper) episodes(from *sql.Tx) error {
	rows, err := from.Query("SELECT id, session_id, start_ts, end_ts, title, summary, projects, created_at, summary_stale FROM episodes ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, stale int
		var session, start, end, projects, created string
		var title, summary sql.NullString
		if err := rows.Scan(&id, &session, &start, &end, &title, &summary, &projects, &created, &stale); err != nil {
			return err
		}
		var paths []string
		if err := json.Unmarshal([]byte(projects), &paths); err != nil {
			return fmt.Errorf("épisode %d : projets illisibles : %w", id, err)
		}
		uid := db.NewUID()
		if _, err := m.to.Exec(`INSERT INTO episodes(id, session_id, start_ts, end_ts, title, summary, created_at, summary_stale, uid, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, session, start, end, title, summary, created, stale, uid, created); err != nil {
			return err
		}
		for _, path := range paths {
			subject, err := m.subject(path)
			if err != nil {
				return err
			}
			if subject == nil {
				continue
			}
			if _, err := m.to.Exec("INSERT OR IGNORE INTO episode_subjects(episode_uid, subject_uid) VALUES (?, ?)", uid, *subject); err != nil {
				return err
			}
		}
		m.c.episodes++
	}
	return rows.Err()
}

func (m *mapper) points(from *sql.Tx) error {
	rows, err := from.Query("SELECT id, episode_id, kind, project, text, importance, source_ts, created_at, closed_at, corrected FROM points ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, episode, importance, corrected int
		var kind, project, text, source, created string
		var closed sql.NullString
		if err := rows.Scan(&id, &episode, &kind, &project, &text, &importance, &source, &created, &closed, &corrected); err != nil {
			return err
		}
		subject, err := m.subject(project)
		if err != nil {
			return err
		}
		if _, err := m.to.Exec(`INSERT INTO points(id, episode_id, kind, subject_uid, text, importance, source_ts, created_at, closed_at, corrected, uid, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, episode, kinds[kind], subject, text, importance, source, created, closed, corrected, db.NewUID(), created); err != nil {
			return fmt.Errorf("point %d : %w", id, err)
		}
		m.c.points++
	}
	return rows.Err()
}

type table struct {
	name    string
	columns []string
}

// Same columns on both sides; summaries_since in meta keeps the summarizer off the past.
var carried = []table{
	{"forgotten_sessions", []string{"session_id", "at"}},
	{"summary_failures", []string{"session_id", "attempts", "last_at"}},
	{"meta", []string{"key", "value"}},
}

func copyTable(from, to *sql.Tx, t table) error {
	list := strings.Join(t.columns, ", ")
	rows, err := from.Query("SELECT " + list + " FROM " + t.name)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]any, len(t.columns))
	targets := make([]any, len(values))
	for i := range values {
		targets[i] = &values[i]
	}
	insert := "INSERT INTO " + t.name + "(" + list + ") VALUES (?" + strings.Repeat(", ?", len(values)-1) + ")"
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		if _, err := to.Exec(insert, values...); err != nil {
			return err
		}
	}
	return rows.Err()
}
