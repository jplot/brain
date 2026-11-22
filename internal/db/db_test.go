package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
	_, err := q.ExecContext(ctx, `INSERT INTO messages(key, session_id, file, ts, role, project, text)
		VALUES (?, 's1', 'f', '2026-01-01T00:00:00Z', 'user', 'p', ?)`, key, text)
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

func TestRebuildIndexKeepsState(t *testing.T) {
	d := open(t)
	d.Exec(`INSERT INTO projects VALUES ('id1','p','work','user','2026-01-01T00:00:00Z')`)
	d.Exec(`INSERT INTO accesses VALUES ('id1','id2',1,'2026-01-01T00:00:00Z')`)
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
	if n := count(t, d, "SELECT count(*) FROM projects WHERE label='work'"); n != 1 {
		t.Fatalf("projects %d", n)
	}
	if n := count(t, d, "SELECT count(*) FROM accesses"); n != 1 {
		t.Fatalf("accesses %d", n)
	}
}

func TestRebuildIndexKeepsProjectPaths(t *testing.T) {
	d := open(t)
	d.Exec(`INSERT INTO project_paths VALUES ('/some/dir','id1')`)
	if err := RebuildIndex(d); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM project_paths WHERE path='/some/dir'"); n != 1 {
		t.Fatalf("got %d", n)
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

func TestSchemaConstraints(t *testing.T) {
	d := open(t)
	bad := []string{
		`INSERT INTO projects VALUES ('a','n','perso','user','t')`,
		`INSERT INTO projects VALUES ('a','n','work','other','t')`,
		`INSERT INTO messages(key,session_id,file,ts,role,project,text) VALUES ('k','s','f','t','other','p','x')`,
		`INSERT INTO episodes(session_id,start_ts,end_ts,projects,created_at,updated_at) VALUES ('s','a','b','p','c','u')`,
	}
	for _, q := range bad {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	d.Exec(`INSERT INTO episodes(id,session_id,start_ts,end_ts,projects,created_at,uid,updated_at) VALUES (1,'s','a','b','p','c','u1','t')`)
	if _, err := d.Exec(`INSERT INTO points(episode_id,kind,project,text,importance,source_ts,created_at,uid,updated_at) VALUES (1,'sujet','p','x',3,'a','b','u2','t')`); err == nil {
		t.Error("old kind accepted")
	}
	if _, err := d.Exec(`INSERT INTO points(episode_id,kind,project,text,importance,source_ts,created_at,uid,updated_at) VALUES (1,'fact','p','x',3,'a','b','u1','t')`); err != nil {
		t.Error(err)
	}
	if _, err := d.Exec(`INSERT INTO forgotten_points VALUES ('u1','t')`); err != nil {
		t.Error(err)
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
