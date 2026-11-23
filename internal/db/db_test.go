package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/paths"
)

var ctx = context.Background()

func newPaths(t *testing.T) paths.Paths {
	t.Helper()
	dir := t.TempDir()
	return paths.Paths{Home: dir + "/home", Transcripts: dir + "/t"}
}

func open(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(newPaths(t), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func insertMessage(t *testing.T, q Querier, key, text string) {
	t.Helper()
	_, err := q.ExecContext(ctx, `INSERT INTO messages(key, session_id, file, ts, role, text)
		VALUES (?, 's1', 'f', '2026-01-01T00:00:00Z', 'user', ?)`, key, text)
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, q Querier, query string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ftsCount(t *testing.T, q Querier, term string) int {
	return count(t, q, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?", term)
}

func TestPermissions(t *testing.T) {
	p := newPaths(t)
	d, err := Open(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for path, want := range map[string]os.FileMode{p.Home: 0o700, p.DB(): 0o600} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", path, fi.Mode().Perm(), err)
		}
	}
}

func TestOpenFixesLooseHomeMode(t *testing.T) {
	p := newPaths(t)
	os.MkdirAll(p.Home, 0o755)
	os.Chmod(p.Home, 0o755)
	d, err := Open(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if fi, _ := os.Stat(p.Home); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestAccentsIgnored(t *testing.T) {
	d := open(t)
	insertMessage(t, d, "k1", "la mémoire persistante")
	if n := ftsCount(t, d, "memoire"); n != 1 {
		t.Fatalf("got %d", n)
	}
}

func TestRebuildIndexKeepsSubjectsAndLinks(t *testing.T) {
	d := open(t)
	mustExec(t, d, `INSERT INTO subjects(uid,kind,name,aliases,created_at,updated_at) VALUES ('s1','subject','a','[]','t','t'),('s2','subject','b','[]','t','t')`)
	mustExec(t, d, `INSERT INTO links VALUES ('s1','depends_on','s2',1,'model',NULL,'t')`)
	mustExec(t, d, `INSERT INTO merged VALUES ('s3','s1','t')`)
	insertMessage(t, d, "k1", "bonjour")
	if err := RebuildIndex(d); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM messages"); n != 0 {
		t.Fatalf("messages %d", n)
	}
	if n := ftsCount(t, d, "bonjour"); n != 0 {
		t.Fatalf("fts %d", n)
	}
	for _, table := range []string{"subjects", "links", "merged"} {
		if n := count(t, d, "SELECT count(*) FROM "+table); n == 0 {
			t.Fatalf("%s emptied", table)
		}
	}
}

func TestRebuildIndexClearsRepoCache(t *testing.T) {
	d := open(t)
	mustExec(t, d, `INSERT INTO repo_cache VALUES ('/some/dir',NULL,'t')`)
	mustExec(t, d, `INSERT INTO session_repos VALUES ('s1','u1')`)
	mustExec(t, d, `INSERT INTO injected VALUES ('s1','u1')`)
	mustExec(t, d, `INSERT INTO pending_paths VALUES ('s1','/d',1,'t')`)
	if err := RebuildIndex(d); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"repo_cache", "session_repos", "injected", "pending_paths"} {
		if n := count(t, d, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("%s kept %d rows", table, n)
		}
	}
}

func TestFailedRebuildKeepsMessagesAndSearch(t *testing.T) {
	d := open(t)
	insertMessage(t, d, "k1", "bonjour")
	if err := rebuild(d, indexSchema+"SELECT * FROM table_that_does_not_exist;"); err == nil {
		t.Fatal("expected an error")
	}
	if n := count(t, d, "SELECT count(*) FROM messages"); n != 1 {
		t.Fatalf("messages %d", n)
	}
	if n := ftsCount(t, d, "bonjour"); n != 1 {
		t.Fatalf("fts %d", n)
	}
}

func TestPragmasOnEveryPooledConnection(t *testing.T) {
	d := open(t)
	var conns []*sql.Conn
	for range 3 {
		c, err := d.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	for _, c := range conns {
		for pragma, want := range map[string]int{"secure_delete": 1, "foreign_keys": 1, "busy_timeout": 5000} {
			var got int
			if err := c.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
				t.Fatalf("%s=%d err=%v", pragma, got, err)
			}
		}
	}
	var mode string
	d.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode %s", mode)
	}
}

func mustExec(t *testing.T, q Querier, query string) {
	t.Helper()
	if _, err := q.ExecContext(ctx, query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestSchemaConstraints(t *testing.T) {
	d := open(t)
	mustExec(t, d, `INSERT INTO subjects(uid,kind,name,aliases,repo,created_at,updated_at) VALUES ('s1','repo','a','[]','github.com/o/r','t','t')`)
	mustExec(t, d, `INSERT INTO subjects(uid,kind,name,aliases,created_at,updated_at) VALUES ('s2','subject','b','[]','t','t')`)
	mustExec(t, d, `INSERT INTO links VALUES ('s2','about','s1',1,'user',NULL,'t')`)
	mustExec(t, d, `INSERT INTO episodes(id,session_id,start_ts,end_ts,created_at,uid,updated_at) VALUES (1,'s','a','b','c','u1','t')`)
	mustExec(t, d, `INSERT INTO episode_subjects VALUES ('u1','s1')`)
	mustExec(t, d, `INSERT INTO points(episode_id,kind,text,importance,source_ts,created_at,uid,updated_at) VALUES (1,'fact','x',3,'a','b','p1','t')`)
	mustExec(t, d, `INSERT INTO forgotten_points VALUES ('p1','t')`)
	mustExec(t, d, `INSERT INTO repo_cache VALUES ('/d',NULL,'t')`)

	bad := []string{
		`INSERT INTO subjects(uid,kind,name,aliases,created_at,updated_at) VALUES ('x','project','n','[]','t','t')`,
		`INSERT INTO subjects(uid,kind,name,aliases,repo,created_at,updated_at) VALUES ('x','repo','n','[]','github.com/o/r','t','t')`,
		`INSERT INTO subjects(uid,kind,name,aliases,parent_origin,created_at,updated_at) VALUES ('x','subject','n','[]','other','t','t')`,
		`INSERT INTO links VALUES ('s1','part_of','s2',1,'model',NULL,'t')`,
		`INSERT INTO links VALUES ('s1','depends_on','s2',1,'other',NULL,'t')`,
		`INSERT INTO links VALUES ('s2','about','s1',0,'user',NULL,'t')`,
		`INSERT INTO episode_subjects VALUES ('u1','s1')`,
		`INSERT INTO points(episode_id,kind,text,importance,source_ts,created_at,uid,updated_at) VALUES (1,'sujet','x',3,'a','b','p2','t')`,
		`INSERT INTO points(episode_id,kind,text,importance,source_ts,created_at,uid,updated_at) VALUES (1,'fact','x',3,'a','b','p1','t')`,
		`INSERT INTO messages(key,session_id,file,ts,role,text) VALUES ('k','s','f','t','other','x')`,
		`INSERT INTO injected VALUES ('s1','u1'),('s1','u1')`,
	}
	for _, q := range bad {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func TestPointsSupersededColumnsDefaultNull(t *testing.T) {
	d := open(t)
	mustExec(t, d, `INSERT INTO episodes(id,session_id,start_ts,end_ts,created_at,uid,updated_at) VALUES (1,'s','a','b','c','u1','t')`)
	mustExec(t, d, `INSERT INTO points(episode_id,kind,text,importance,source_ts,created_at,uid,updated_at) VALUES (1,'fact','x',3,'a','b','p1','t')`)
	if n := count(t, d, "SELECT count(*) FROM points WHERE superseded_by_uid IS NULL AND superseded_at IS NULL"); n != 1 {
		t.Fatalf("got %d", n)
	}
}

const (
	writerEnv = "PAUL_DB_WRITER"
	homeEnv   = "PAUL_DB_HOME"
	writers   = 4
	perWriter = 200
)

// TestWriterProcess is the child body: it only runs when re-executed.
func TestWriterProcess(t *testing.T) {
	id := os.Getenv(writerEnv)
	if id == "" {
		t.Skip("child process only")
	}
	p := paths.Paths{Home: os.Getenv(homeEnv)}
	d, err := Open(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	release, acquired, err := lock.Writer(p.IndexLock(), true)
	if err != nil || !acquired {
		t.Fatalf("lock: %v %v", acquired, err)
	}
	defer release()
	for i := range perWriter {
		// Read then write inside a deferred transaction: without the
		// lock, concurrent processes hit SQLITE_BUSY on lock upgrade.
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := count(t, tx, "SELECT count(*) FROM messages")
		insertMessage(t, tx, fmt.Sprintf("w%s-%d-%d", id, i, n), "pomme")
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentWritersRealProcesses(t *testing.T) {
	p := newPaths(t)
	d, err := Open(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var cmds []*exec.Cmd
	var outs []*bytes.Buffer
	for i := range writers {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWriterProcess$")
		cmd.Env = append(os.Environ(), writerEnv+"="+strconv.Itoa(i), homeEnv+"="+p.Home)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		outs = append(outs, out)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("writer %d: %v\n%s", i, err, outs[i].String())
		}
	}
	if n := count(t, d, "SELECT count(*) FROM messages"); n != writers*perWriter {
		t.Fatalf("got %d rows, want %d", n, writers*perWriter)
	}
}

func TestPointsBySubjectIndexed(t *testing.T) {
	d := open(t)
	var id, parent, unused int
	var plan string
	if err := d.QueryRow("EXPLAIN QUERY PLAN SELECT id FROM points WHERE subject_uid = 'x'").Scan(&id, &parent, &unused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "INDEX points_subject") {
		t.Fatal(plan)
	}
}
