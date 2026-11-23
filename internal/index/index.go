// Package index fills the rebuildable part of the database from the transcripts and the journals.
package index

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/repo"
	"github.com/jplot/brain/internal/subjects"
	"github.com/jplot/brain/internal/transcript"
)

var gitBudget = 5 * time.Second

const (
	busyTimeout = 30 * time.Second
	maxAttempts = 3
)

var ctx = context.Background()

// Replaced by tests to count or slow down the parse calls of a pass.
var parseClaude = transcript.ParseClaudeLine

type Stats struct{ Files, Messages, Malformed, Failed int }

// Run takes the index lock, so concurrent callers queue instead of reading the same offsets.
func Run(p paths.Paths) (Stats, error) {
	if err := p.EnsureHome(); err != nil {
		return Stats{}, err
	}
	release, _, err := lock.Writer(p.IndexLock(), true)
	if err != nil {
		return Stats{}, err
	}
	defer release()
	conn, err := db.Open(p, busyTimeout)
	if err != nil {
		return Stats{}, err
	}
	defer conn.Close()
	stats, upgraded, err := upgrade(conn, p)
	if err != nil || upgraded {
		return stats, err
	}
	return All(conn, p), nil
}

func Rebuild(p paths.Paths) (Stats, error) {
	if err := p.EnsureHome(); err != nil {
		return Stats{}, err
	}
	release, _, err := lock.Writer(p.IndexLock(), true)
	if err != nil {
		return Stats{}, err
	}
	defer release()
	conn, err := db.Open(p, busyTimeout)
	if err != nil {
		return Stats{}, err
	}
	defer conn.Close()
	if err := db.RebuildIndex(conn); err != nil {
		return Stats{}, err
	}
	if err := subjects.RebuildTerms(conn); err != nil {
		return Stats{}, err
	}
	stats := All(conn, p)
	return stats, finishUpgrade(conn)
}

// Refresh does nothing while another process indexes: that one is already doing the work.
func Refresh(conn *sql.DB, p paths.Paths) error {
	if err := p.EnsureHome(); err != nil {
		return err
	}
	release, acquired, err := lock.Writer(p.IndexLock(), false)
	if err != nil || !acquired {
		return err
	}
	defer release()
	if _, upgraded, err := upgrade(conn, p); err != nil || upgraded {
		return err
	}
	All(conn, p)
	return nil
}

func All(conn *sql.DB, p paths.Paths) Stats {
	var stats Stats
	ps := &pass{}
	if err := ps.retryPending(conn); err != nil {
		stats.Failed++
		log.Printf("retry of pending paths failed (%T)", err)
	}
	files, _ := filepath.Glob(filepath.Join(p.Transcripts, "*", "*.jsonl"))
	for _, name := range files {
		indexOne(conn, ps, name, false, &stats)
	}
	journals, _ := filepath.Glob(filepath.Join(p.Journals(), "*.jsonl"))
	for _, name := range journals {
		indexOne(conn, ps, name, true, &stats)
	}
	return stats
}

func indexOne(conn *sql.DB, ps *pass, name string, journal bool, stats *Stats) {
	done, err := indexFile(conn, ps, name, journal)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		stats.Failed++
		log.Printf("indexing failed: %s (%T)", name, err)
	default:
		stats.Files++
		stats.Messages += done.Messages
		stats.Malformed += done.Malformed
	}
}

func upgrade(conn *sql.DB, p paths.Paths) (stats Stats, upgraded bool, err error) {
	var version int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return stats, false, err
	}
	if version == db.IndexVersion {
		return stats, false, nil
	}
	var empty bool
	if err := conn.QueryRow("SELECT NOT EXISTS(SELECT 1 FROM messages) AND NOT EXISTS(SELECT 1 FROM files)").Scan(&empty); err != nil {
		return stats, false, err
	}
	// An upgrade killed right after the rebuild leaves an empty version 0 base whose free pages
	// still hold the old messages: the cleanup must run here too.
	if version == 0 && empty {
		return stats, false, errors.Join(subjects.RebuildTerms(conn), finishUpgrade(conn))
	}
	if err := db.RebuildIndex(conn); err != nil {
		return stats, false, err
	}
	if err := subjects.RebuildTerms(conn); err != nil {
		return stats, false, err
	}
	stats = All(conn, p)
	return stats, true, finishUpgrade(conn)
}

// The checkpoint reports "busy" without failing while a reader holds the WAL, and the WAL keeps
// the old bytes: the version is only set after a complete cleanup.
func finishUpgrade(conn *sql.DB) error {
	if err := vacuum(conn); err != nil {
		log.Printf("database cleanup failed (%T)", err)
		return nil
	}
	_, err := conn.Exec(fmt.Sprintf("PRAGMA user_version=%d", db.IndexVersion))
	return err
}

var vacuum = func(conn *sql.DB) error {
	if _, err := conn.Exec("VACUUM"); err != nil {
		return err
	}
	var busy, pages, moved int
	if err := conn.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &moved); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("checkpoint blocked by a reader")
	}
	return nil
}

func indexFile(conn *sql.DB, ps *pass, name string, journal bool) (Stats, error) {
	var stats Stats
	f, err := os.Open(name)
	if err != nil {
		return stats, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return stats, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()

	// A write comes first so the transaction takes the write lock at once: a deferred
	// read-then-write transaction fails with SQLITE_BUSY_SNAPSHOT when another process commits between.
	var offset int64
	err = tx.QueryRow("UPDATE files SET size = size WHERE path = ? RETURNING offset", name).Scan(&offset)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, err
	}
	if info.Size() < offset {
		if err := forget(tx, name); err != nil {
			return stats, err
		}
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return stats, err
	}
	r := &reader{tx: tx, name: name, journal: journal, forgotten: map[string]bool{}, ps: ps, touched: map[string]*touched{}}
	if err := r.loadForgotten(); err != nil {
		return stats, err
	}
	lines := bufio.NewReader(f)
	for {
		raw, err := lines.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return stats, err
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			break
		}
		offset += int64(len(raw))
		if err := r.line(raw, &stats); err != nil {
			return Stats{}, err
		}
	}
	if err := r.discover(); err != nil {
		return Stats{}, err
	}
	_, err = tx.Exec(`INSERT INTO files(path, offset, size) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET offset = excluded.offset, size = excluded.size`, name, offset, info.Size())
	if err != nil {
		return Stats{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// A session shared with another file keeps wrong dates or title until a rebuild;
// none of the 125 transcripts probed on 2026-11-02 has one.
func forget(tx *sql.Tx, name string) error {
	if _, err := tx.Exec("DELETE FROM sessions WHERE id IN (SELECT session_id FROM messages WHERE file = ?)", name); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM messages WHERE file = ?", name)
	return err
}

type reader struct {
	tx        *sql.Tx
	name      string
	journal   bool
	forgotten map[string]bool
	ps        *pass
	touched   map[string]*touched
}

type touched struct {
	cwds, files []string
	seen        map[string]bool
}

func (r *reader) touch(e transcript.Entry) {
	t := r.touched[e.SessionID]
	if t == nil {
		t = &touched{seen: map[string]bool{}}
		r.touched[e.SessionID] = t
	}
	add := func(list *[]string, p string) {
		if filepath.IsAbs(p) && !t.seen[p] {
			t.seen[p] = true
			*list = append(*list, p)
		}
	}
	if e.Cwd != nil {
		add(&t.cwds, *e.Cwd)
	}
	for _, p := range e.Paths {
		add(&t.files, p)
	}
}

// The working directory goes first and every directory is looked up once per file:
// a session that edits 200 files of one repository costs one git lookup.
func (r *reader) discover() error {
	gitCtx, cancel := context.WithTimeout(ctx, gitBudget)
	defer cancel()
	memo := map[string]string{}
	for session, t := range r.touched {
		for _, p := range slices.Concat(t.cwds, t.files) {
			dir, ok := existingDir(p)
			if !ok {
				continue
			}
			uid, resolved, timedOut, err := r.ps.lookup(gitCtx, r.tx, dir, memo)
			if err != nil {
				return err
			}
			if !resolved {
				if err := deferPath(r.tx, session, dir, timedOut); err != nil {
					return err
				}
				continue
			}
			if err := link(r.tx, session, uid); err != nil {
				return err
			}
		}
	}
	return nil
}

// pass is shared by all the files of one indexing pass: after the first git execution
// failure no other git call is made, so a missing git costs one failed exec per pass.
type pass struct{ gitDown bool }

// resolved is false when git could not answer; timedOut tells that this very call hit the
// deadline, the only failure that counts as an attempt: with git absent a healthy hint would
// be dropped every 3 passes. An error is never git's.
func (ps *pass) lookup(c context.Context, q db.Querier, dir string, memo map[string]string) (uid string, resolved, timedOut bool, err error) {
	if uid, ok := memo[dir]; ok {
		return uid, true, false, nil
	}
	if ps.gitDown {
		return "", false, false, nil
	}
	uid, _, err = repo.Cached(c, q, dir)
	if errors.Is(err, repo.ErrGit) {
		ps.gitDown = true
		log.Printf("repository discovery postponed: %s", gitReason(err))
		return "", false, errors.Is(err, context.DeadlineExceeded), nil
	}
	if err != nil {
		return "", false, false, err
	}
	memo[dir] = uid
	return uid, true, false, nil
}

func gitReason(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "git missing"
	case errors.Is(err, context.DeadlineExceeded):
		return "git timed out"
	}
	return "git failed"
}

func link(q db.Querier, session, uid string) error {
	if uid == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO session_repos(session_id, subject_uid) SELECT ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM forgotten_sessions WHERE session_id = ?)`, session, uid, session)
	return err
}

func deferPath(q db.Querier, session, dir string, counted bool) error {
	attempts := 0
	if counted {
		attempts = 1
	}
	now := db.Stamp(time.Time{})
	if _, err := q.ExecContext(ctx, `INSERT INTO pending_paths(session_id, path, attempts, last_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id, path) DO UPDATE SET attempts = attempts + ?, last_at = excluded.last_at`,
		session, dir, attempts, now, attempts); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, "DELETE FROM pending_paths WHERE session_id = ? AND path = ? AND attempts >= ?", session, dir, maxAttempts)
	return err
}

type pendingPath struct{ session, path string }

func (ps *pass) retryPending(conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, "DELETE FROM pending_paths WHERE session_id IN (SELECT session_id FROM forgotten_sessions)"); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, "SELECT session_id, path FROM pending_paths")
	if err != nil {
		return err
	}
	var pending []pendingPath
	for rows.Next() {
		var p pendingPath
		if err := rows.Scan(&p.session, &p.path); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(pending) == 0 {
		return err
	}
	gitCtx, cancel := context.WithTimeout(ctx, gitBudget)
	defer cancel()
	memo := map[string]string{}
	for _, p := range pending {
		dir, ok := existingDir(p.path)
		if !ok {
			if err := forgetPath(conn, p); err != nil {
				return err
			}
			continue
		}
		uid, resolved, timedOut, err := ps.lookup(gitCtx, conn, dir, memo)
		if err != nil {
			return err
		}
		if !resolved {
			if timedOut {
				if err := deferPath(conn, p.session, p.path, true); err != nil {
					return err
				}
			}
			continue
		}
		if err := link(conn, p.session, uid); err != nil {
			return err
		}
		if err := forgetPath(conn, p); err != nil {
			return err
		}
	}
	return nil
}

func forgetPath(q db.Querier, p pendingPath) error {
	_, err := q.ExecContext(ctx, "DELETE FROM pending_paths WHERE session_id = ? AND path = ?", p.session, p.path)
	return err
}

func existingDir(p string) (string, bool) {
	info, err := os.Stat(p)
	switch {
	case err != nil:
		return "", false
	case info.IsDir():
		return p, true
	}
	return filepath.Dir(p), true
}

func (r *reader) loadForgotten() error {
	rows, err := r.tx.Query("SELECT session_id FROM forgotten_sessions")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		r.forgotten[id] = true
	}
	return rows.Err()
}

func (r *reader) line(raw []byte, stats *Stats) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	items, err := r.parse(raw)
	if err != nil || hasNulCwd(items) {
		stats.Malformed++
		return nil
	}
	for _, item := range items {
		if err := r.store(item, stats); err != nil {
			return err
		}
	}
	return nil
}

func (r *reader) parse(raw []byte) ([]transcript.Item, error) {
	if r.journal {
		return transcript.ParseJournalLine(raw, r.name)
	}
	return parseClaude(raw, r.isAgentUse)
}

// A NUL cannot be part of a path, and exec refuses it as an argument.
func hasNulCwd(items []transcript.Item) bool {
	for _, item := range items {
		if e, ok := item.(transcript.Entry); ok && e.Cwd != nil && strings.ContainsRune(*e.Cwd, 0) {
			return true
		}
	}
	return false
}

func (r *reader) isAgentUse(id string) bool {
	var one int
	return r.tx.QueryRow("SELECT 1 FROM agent_uses WHERE id = ?", id).Scan(&one) == nil
}

func (r *reader) store(item transcript.Item, stats *Stats) error {
	switch it := item.(type) {
	case transcript.AgentUse:
		_, err := r.tx.Exec("INSERT OR IGNORE INTO agent_uses(id) VALUES (?)", it.ToolUseID)
		return err
	case transcript.Title:
		if r.forgotten[it.SessionID] {
			return nil
		}
		_, err := r.tx.Exec(`INSERT INTO sessions(id, title) VALUES (?, ?)
			ON CONFLICT(id) DO UPDATE SET title = excluded.title`, it.SessionID, it.Title)
		return err
	case transcript.Entry:
		if r.forgotten[it.SessionID] {
			return nil
		}
		return r.entry(it, stats)
	}
	return nil
}

func (r *reader) entry(e transcript.Entry, stats *Stats) error {
	r.touch(e)
	res, err := r.tx.Exec(`INSERT OR IGNORE INTO messages(key, session_id, file, ts, role, git_branch, text)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, e.Key, e.SessionID, r.name, e.TS, e.Role, e.GitBranch, e.Text)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	stats.Messages++
	var launchCwd any
	if e.Cwd != nil && *e.Cwd != "" {
		launchCwd = *e.Cwd
	}
	_, err = r.tx.Exec(`INSERT INTO sessions(id, launch_cwd, started_at, ended_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		launch_cwd = COALESCE(launch_cwd, excluded.launch_cwd),
		started_at = COALESCE(MIN(started_at, excluded.started_at), excluded.started_at),
		ended_at = COALESCE(MAX(ended_at, excluded.ended_at), excluded.ended_at)`,
		e.SessionID, launchCwd, e.TS, e.TS)
	return err
}
