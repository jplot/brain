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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/repo"
	"github.com/jplot/brain/internal/subjects"
)

var ErrAlreadyDone = errors.New("paul.db existe déjà : la reprise a déjà eu lieu")

var (
	ctx   = context.Background()
	kinds = map[string]string{"sujet": "topic", "decision": "decision", "suspens": "pending", "fait": "fact"}
)

type counts struct {
	sessions, episodes, points, subjects int
	rootSeen                             bool
}

// Run builds the new base in a staging folder and links it in place at the end,
// so a failed import leaves no paul.db behind and can be run again.
func Run(old, root string, p paths.Paths, out io.Writer) error {
	if _, err := os.Stat(p.DB()); err == nil {
		return ErrAlreadyDone
	} else if !errors.Is(err, fs.ErrNotExist) {
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
	c, err := importBase(old, filepath.Clean(root), staged)
	if err != nil {
		return err
	}
	vocal, err := convertVocal(filepath.Join(filepath.Dir(old), "vocal", "conversation.jsonl"), staged.Journals())
	if err != nil {
		return err
	}
	if err := commit(staged, p, vocal > 0); err != nil {
		return err
	}
	stats, err := index.Rebuild(p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Reprise terminée depuis %s.\n", old)
	if !c.rootSeen {
		fmt.Fprintf(out, "Racine des projets absente de l'ancienne base : %s (option --root)\n", root)
	}
	fmt.Fprintf(out, "Sessions résumées : %d\nÉpisodes : %d\nPoints : %d\nSujets créés : %d\nLignes vocales converties : %d\n",
		c.sessions, c.episodes, c.points, c.subjects, vocal)
	fmt.Fprintf(out, "Index reconstruit : %d fichiers, %d messages\n", stats.Files, stats.Messages)
	return nil
}

// A hard link never replaces a file: if a hook created paul.db meanwhile, the import loses.
func commit(staged, p paths.Paths, vocal bool) error {
	journal := filepath.Join(p.Journals(), "vocal.jsonl")
	if vocal {
		if err := os.MkdirAll(p.Journals(), 0o700); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(staged.Journals(), "vocal.jsonl"), journal); err != nil {
			return fmt.Errorf("journal vocal : %w", err)
		}
	}
	err := os.Link(staged.DB(), p.DB())
	if errors.Is(err, fs.ErrExist) {
		err = ErrAlreadyDone
	}
	if err != nil && vocal {
		return errors.Join(err, os.Remove(journal))
	}
	return err
}

func importBase(old, root string, staged paths.Paths) (counts, error) {
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
	m := &mapper{to: to, root: root, uids: map[string]*string{}, c: &c}
	steps := []func(*sql.Tx) error{m.projects, m.episodes, m.points}
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

type mapper struct {
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
	info, ok, err := repo.Of(ctx, path)
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
