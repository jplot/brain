package migrate

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/repo"
	"github.com/jplot/brain/internal/search"
	"github.com/jplot/brain/internal/summary"
)

// The schema of the Python version, at INDEX_VERSION 4.
const oldSchema = `
CREATE TABLE IF NOT EXISTS projects(
    path TEXT PRIMARY KEY, name TEXT NOT NULL,
    label TEXT NOT NULL CHECK(label IN ('perso','pro','a_classer')),
    label_by TEXT NOT NULL CHECK(label_by IN ('regle','jonathan')));
CREATE TABLE IF NOT EXISTS accesses(
    reader TEXT NOT NULL, target TEXT NOT NULL, PRIMARY KEY(reader, target));
CREATE TABLE IF NOT EXISTS episodes(
    id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, start_ts TEXT NOT NULL, end_ts TEXT NOT NULL,
    title TEXT, summary TEXT, projects TEXT NOT NULL, created_at TEXT NOT NULL,
    summary_stale INTEGER NOT NULL DEFAULT 0, UNIQUE(session_id, start_ts));
CREATE TABLE IF NOT EXISTS points(
    id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id INTEGER NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('sujet','decision','suspens','fait')),
    project TEXT NOT NULL, text TEXT NOT NULL,
    importance INTEGER NOT NULL CHECK(importance BETWEEN 1 AND 5),
    source_ts TEXT NOT NULL, created_at TEXT NOT NULL, closed_at TEXT,
    corrected INTEGER NOT NULL DEFAULT 0);
CREATE VIRTUAL TABLE IF NOT EXISTS points_fts USING fts5(
    text, content='points', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER IF NOT EXISTS points_ai AFTER INSERT ON points BEGIN
    INSERT INTO points_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TABLE IF NOT EXISTS forgotten_sessions(session_id TEXT PRIMARY KEY, at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS summary_failures(
    session_id TEXT PRIMARY KEY, attempts INTEGER NOT NULL, last_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS files(
    path TEXT PRIMARY KEY, offset INTEGER NOT NULL, size INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS cwd_projects(cwd TEXT PRIMARY KEY, project TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agent_uses(id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS sessions(
    id TEXT PRIMARY KEY, launch_cwd TEXT, launch_project TEXT, title TEXT,
    started_at TEXT, ended_at TEXT);
CREATE TABLE IF NOT EXISTS messages(
    id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, session_id TEXT NOT NULL, file TEXT NOT NULL,
    ts TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('jonathan','claude','sous_agent','outil')),
    project TEXT NOT NULL, git_branch TEXT, text TEXT NOT NULL);
PRAGMA user_version=4;
`

type fixture struct {
	t                     *testing.T
	dir, old, root        string
	atelier, jardin, away string
	p                     paths.Paths
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir, old: filepath.Join(dir, "brain", "brain.db"), root: filepath.Join(dir, "Projects")}
	f.atelier = filepath.Join(f.root, "atelier")
	f.jardin = filepath.Join(f.root, "jardin")
	f.away = filepath.Join(dir, "ailleurs", "jardin")
	f.p = paths.Paths{Home: filepath.Join(dir, "paul"), Transcripts: filepath.Join(dir, "transcripts")}
	if err := os.MkdirAll(f.atelier, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:exemple/atelier.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", f.atelier}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v : %v\n%s", args, err, out)
		}
	}
	return f
}

// Open and in WAL mode, as the Python version left its base.
func (f *fixture) build(statements ...string) *sql.DB {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.old), 0o700); err != nil {
		f.t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", "file:"+f.old+"?_pragma=journal_mode(WAL)")
	if err != nil {
		f.t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	for _, s := range append([]string{oldSchema}, statements...) {
		if _, err := conn.Exec(s); err != nil {
			conn.Close()
			f.t.Fatalf("%s: %v", s, err)
		}
	}
	return conn
}

func (f *fixture) sample() []string {
	return []string{
		`INSERT INTO projects VALUES ('` + f.root + `','Projects','a_classer','regle'),
			('` + f.atelier + `','atelier','perso','regle'),
			('` + f.jardin + `','jardin','pro','regle'),
			('` + f.away + `','jardin','a_classer','regle')`,
		`INSERT INTO accesses VALUES ('` + f.atelier + `','` + f.jardin + `')`,
		`INSERT INTO episodes VALUES
			(7,'s-un','2026-11-02T10:00:00.000Z','2026-11-02T11:00:00.000Z','Atelier','Le résumé',
			 '["` + f.atelier + `","` + f.jardin + `","` + f.root + `"]','2026-11-02T12:00:00.000Z',1),
			(9,'s-deux','2026-11-03T10:00:00.000Z','2026-11-03T11:00:00.000Z',NULL,NULL,'[]','2026-11-03T12:00:00.000Z',0)`,
		`INSERT INTO points VALUES
			(1,7,'sujet','` + f.atelier + `','le banc de scie',4,'2026-11-02T10:05:00.000Z','2026-11-02T12:00:01.000Z',NULL,0),
			(2,7,'decision','` + f.jardin + `','arroser le soir',3,'2026-11-02T10:06:00.000Z','2026-11-02T12:00:02.000Z',NULL,0),
			(3,7,'suspens','` + f.root + `','ranger le garage',2,'2026-11-02T10:07:00.000Z','2026-11-02T12:00:03.000Z','2026-11-04T08:00:00.000Z',1),
			(5,9,'fait','` + f.away + `','la terre est argileuse',5,'2026-11-03T10:01:00.000Z','2026-11-03T12:00:04.000Z',NULL,0)`,
		`INSERT INTO forgotten_sessions VALUES ('s-oubli','2026-11-05T09:00:00.000Z')`,
		`INSERT INTO summary_failures VALUES ('s-echec',2,'2026-11-05T10:00:00.000Z')`,
		`INSERT INTO meta VALUES ('summaries_since','2026-11-01T00:00:00.000Z')`,
	}
}

func (f *fixture) migrate() string {
	f.t.Helper()
	var out bytes.Buffer
	if err := Run(f.old, f.root, f.p, &out); err != nil {
		f.t.Fatal(err)
	}
	return out.String()
}

func (f *fixture) open() *sql.DB {
	f.t.Helper()
	conn, err := db.Open(f.p, time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	return conn
}

func (f *fixture) writeVocal(lines ...string) {
	f.t.Helper()
	path := filepath.Join(filepath.Dir(f.old), "vocal", "conversation.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func column(t *testing.T, conn *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, orNULL(v))
	}
	return out
}

func orNULL(v sql.NullString) string {
	if !v.Valid {
		return "NULL"
	}
	return v.String
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

var uidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestMigrateTranslatesValues(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	f.migrate()
	conn := f.open()

	equal(t, column(t, conn, `SELECT id||' '||episode_id||' '||kind||' '||text||' '||importance||' '||source_ts||' '||created_at||' '||
		COALESCE(closed_at,'-')||' '||corrected FROM points ORDER BY id`), []string{
		"1 7 topic le banc de scie 4 2026-11-02T10:05:00.000Z 2026-11-02T12:00:01.000Z - 0",
		"2 7 decision arroser le soir 3 2026-11-02T10:06:00.000Z 2026-11-02T12:00:02.000Z - 0",
		"3 7 pending ranger le garage 2 2026-11-02T10:07:00.000Z 2026-11-02T12:00:03.000Z 2026-11-04T08:00:00.000Z 1",
		"5 9 fact la terre est argileuse 5 2026-11-03T10:01:00.000Z 2026-11-03T12:00:04.000Z - 0",
	})
	equal(t, column(t, conn, `SELECT id||' '||session_id||' '||start_ts||' '||end_ts||' '||COALESCE(title,'-')||' '||
		COALESCE(summary,'-')||' '||created_at||' '||summary_stale FROM episodes ORDER BY id`), []string{
		"7 s-un 2026-11-02T10:00:00.000Z 2026-11-02T11:00:00.000Z Atelier Le résumé 2026-11-02T12:00:00.000Z 1",
		"9 s-deux 2026-11-03T10:00:00.000Z 2026-11-03T11:00:00.000Z - - 2026-11-03T12:00:00.000Z 0",
	})
	uids := column(t, conn, "SELECT uid FROM points WHERE updated_at = created_at AND superseded_by_uid IS NULL UNION ALL SELECT uid FROM episodes WHERE updated_at = created_at")
	if len(uids) != 6 {
		t.Fatalf("%d rows with updated_at = created_at", len(uids))
	}
	for _, uid := range uids {
		if !uidV4.MatchString(uid) {
			t.Fatalf("uid %q", uid)
		}
	}
	equal(t, column(t, conn, "SELECT count(DISTINCT uid) FROM (SELECT uid FROM points UNION ALL SELECT uid FROM episodes)"), []string{"6"})
	equal(t, column(t, conn, "SELECT text FROM points_fts WHERE points_fts MATCH 'argileuse'"), []string{"la terre est argileuse"})
	equal(t, column(t, conn, "SELECT session_id||' '||at FROM forgotten_sessions"), []string{"s-oubli 2026-11-05T09:00:00.000Z"})
	equal(t, column(t, conn, "SELECT session_id||' '||attempts||' '||last_at FROM summary_failures"), []string{"s-echec 2 2026-11-05T10:00:00.000Z"})
	equal(t, column(t, conn, "SELECT key||' '||value FROM meta"), []string{"summaries_since 2026-11-01T00:00:00.000Z"})
}

func TestMigrateProjectsBecomeSubjects(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	transcript := filepath.Join(f.p.Transcripts, "x", "s-un.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o700)
	line := `{"type":"user","uuid":"u1","sessionId":"s-un","timestamp":"2026-11-02T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"bonjour"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.migrate()
	conn := f.open()

	equal(t, column(t, conn, "SELECT kind||' '||name||' '||COALESCE(repo,'-') FROM subjects ORDER BY name"), []string{
		"repo atelier github.com/exemple/atelier",
		"subject jardin -",
	})
	equal(t, column(t, conn, "SELECT p.id||' '||COALESCE(s.name,'-') FROM points p LEFT JOIN subjects s ON s.uid = p.subject_uid ORDER BY p.id"), []string{
		"1 atelier", "2 jardin", "3 -", "5 jardin",
	})
	equal(t, column(t, conn, `SELECT e.id||' '||s.name FROM episode_subjects es JOIN episodes e ON e.uid = es.episode_uid
		JOIN subjects s ON s.uid = es.subject_uid ORDER BY e.id, s.name`), []string{"7 atelier", "7 jardin"})
	equal(t, column(t, conn, "SELECT session_id||' '||role||' '||text FROM messages"), []string{"s-un user bonjour"})
	equal(t, column(t, conn, "SELECT term FROM subject_terms ORDER BY term"), []string{"atelier", "jardin"})
	for _, want := range []string{"Sessions résumées : 2", "Épisodes : 2", "Points : 4", "Sujets créés : 2", "Lignes vocales converties : 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Racine") {
		t.Errorf("root reported missing:\n%s", out)
	}
}

func TestMigrateWarnsWhenRootAbsent(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	f.root = filepath.Join(f.dir, "Ailleurs")
	out := f.migrate()
	if !strings.Contains(out, "Racine des projets absente de l'ancienne base : "+f.root) {
		t.Fatal(out)
	}
	equal(t, column(t, f.open(), "SELECT name FROM subjects WHERE name = 'Projects'"), []string{"Projects"})
}

func hash(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func tree(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != root {
			rel, _ := filepath.Rel(root, path)
			names = append(names, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestMigrateKeepsOldDatabaseUntouched(t *testing.T) {
	for _, live := range []bool{false, true} {
		f := newFixture(t)
		writer := f.build(f.sample()...)
		files := []string{f.old}
		if live {
			// A Python hook still holding the base: the last point lives in the WAL only.
			if _, err := writer.Exec(`INSERT INTO points VALUES (6,9,'fait','` + f.away + `','dans le wal',1,'a','b',NULL,0)`); err != nil {
				t.Fatal(err)
			}
			files = append(files, f.old+"-wal")
		} else {
			writer.Close()
		}
		f.writeVocal(`{"t":1700000000,"type":"toi","texte":"bonjour"}` + "\n")
		files = append(files, filepath.Join(filepath.Dir(f.old), "vocal", "conversation.jsonl"))
		before := map[string][32]byte{}
		for _, path := range files {
			before[path] = hash(t, path)
		}
		listed := tree(t, filepath.Dir(f.old))
		f.migrate()
		for _, path := range files {
			if hash(t, path) != before[path] {
				t.Fatalf("live=%v: %s changed", live, path)
			}
		}
		equal(t, tree(t, filepath.Dir(f.old)), listed)
		equal(t, tree(t, f.p.Home), []string{"index.lock", "journals", "journals/vocal.jsonl", "paul.db"})
		want := "4"
		if live {
			want = "5"
			writer.Close()
		}
		equal(t, column(t, f.open(), "SELECT count(*) FROM points"), []string{want})
	}
}

func TestMigrateRefusesTwice(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	f.writeVocal(`{"t":1700000000,"type":"toi","texte":"bonjour"}` + "\n")
	f.migrate()
	journal := filepath.Join(f.p.Journals(), "vocal.jsonl")
	dbHash, journalHash := hash(t, f.p.DB()), hash(t, journal)
	if err := Run(f.old, f.root, f.p, io.Discard); !errors.Is(err, ErrAlreadyDone) {
		t.Fatalf("got %v", err)
	}
	if hash(t, f.p.DB()) != dbHash || hash(t, journal) != journalHash {
		t.Fatal("second run changed the new files")
	}
	entries, _ := os.ReadDir(f.p.Home)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "migrate-") {
			t.Fatalf("staging left behind: %s", e.Name())
		}
	}
}

func TestMigrateRemovesLeftoverStaging(t *testing.T) {
	for _, done := range []bool{false, true} {
		f := newFixture(t)
		f.build(f.sample()...).Close()
		leftover := filepath.Join(f.p.Home, "migrate-123")
		os.MkdirAll(leftover, 0o700)
		os.WriteFile(filepath.Join(leftover, "brain.db"), []byte("ancienne mémoire"), 0o600)
		if done {
			os.WriteFile(f.p.DB(), nil, 0o600)
			if err := Run(f.old, f.root, f.p, io.Discard); !errors.Is(err, ErrAlreadyDone) {
				t.Fatalf("got %v", err)
			}
		} else {
			f.migrate()
		}
		if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("done=%v: leftover kept: %v", done, err)
		}
	}
}

func TestMigrateDoneEvenIfIndexFails(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	saved := rebuild
	t.Cleanup(func() { rebuild = saved })
	var during []string
	rebuild = func(p paths.Paths) (index.Stats, error) {
		during = tree(t, p.Home)
		return index.Stats{}, errors.New("disque plein")
	}
	out := f.migrate()
	equal(t, during, []string{"paul.db"})
	if !strings.Contains(out, "Points : 4") || !strings.Contains(out, "Index non reconstruit : il le sera au prochain lancement de paul") {
		t.Fatal(out)
	}
	conn := f.open()
	if err := index.Refresh(conn, f.p); err != nil {
		t.Fatal(err)
	}
	equal(t, column(t, conn, "SELECT count(*) FROM points"), []string{"4"})
}

func TestMigrateRelativeRoot(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	t.Chdir(f.dir)
	var out bytes.Buffer
	if err := Run(f.old, "Projects", f.p, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Racine") {
		t.Fatal(out.String())
	}
	equal(t, column(t, f.open(), "SELECT count(*) FROM points WHERE subject_uid IS NULL"), []string{"1"})
}

const childEnv = "PAUL_MIGRATE_CHILD"

// TestInterruptedChild is the child body: it only runs when re-executed.
func TestInterruptedChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("child process only")
	}
	p := paths.Paths{Home: os.Getenv("PAUL_HOME"), Transcripts: os.Getenv("PAUL_TRANSCRIPTS")}
	fmt.Println(Run(os.Getenv("OLD"), os.Getenv("ROOT"), p, io.Discard))
}

func TestMigrateInterruptedRemovesStaging(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	bin, started := t.TempDir(), filepath.Join(t.TempDir(), "git-started")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n: > '"+started+"'\nexec /bin/sleep 30\n"), 0o755)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptedChild$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1", "PATH="+bin, "OLD="+f.old, "ROOT="+f.root,
		"PAUL_HOME="+f.p.Home, "PAUL_TRANSCRIPTS="+f.p.Transcripts)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("git never started\n%s", out.String())
		}
	}
	if staging, _ := filepath.Glob(filepath.Join(f.p.Home, "migrate-*", "brain.db")); len(staging) != 1 {
		t.Fatalf("no staging copy while blocked: %v", staging)
	}
	cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	// Under git's own 5 s timeout: the interrupt must kill git, not wait for it.
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("child still running\n%s", out.String())
	}
	if !strings.Contains(out.String(), "interrompue avant la fin : rien n'est écrit, la copie de travail est effacée") {
		t.Fatalf("output:\n%s", out.String())
	}
	equal(t, tree(t, f.p.Home), nil)
}

func TestMigrateLosesToConcurrentDatabase(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	f.writeVocal(`{"t":1700000000,"type":"toi","texte":"bonjour"}` + "\n")
	staged := paths.Paths{Home: t.TempDir()}
	vocal, _, err := convertVocal(filepath.Join(filepath.Dir(f.old), "vocal", "conversation.jsonl"), staged.Journals(), nil)
	if err != nil || vocal != 1 {
		t.Fatalf("%d %v", vocal, err)
	}
	os.WriteFile(staged.DB(), nil, 0o600)
	f.p.EnsureHome()
	os.WriteFile(f.p.DB(), []byte("hook"), 0o600)
	if err := commit(staged, f.p, []string{"vocal.jsonl"}); !errors.Is(err, ErrAlreadyDone) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.p.Journals(), "vocal.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal kept: %v", err)
	}
	if b, _ := os.ReadFile(f.p.DB()); string(b) != "hook" {
		t.Fatal("paul.db replaced")
	}
}

func TestMigrateRefusesExistingJournal(t *testing.T) {
	for _, name := range []string{"vocal.jsonl", "archive.jsonl"} {
		f := newFixture(t)
		f.build(append(f.sample(), f.archived()...)...).Close()
		f.writeVocal(`{"t":1700000000,"type":"toi","texte":"bonjour"}` + "\n")
		existing := filepath.Join(f.p.Journals(), name)
		os.MkdirAll(f.p.Journals(), 0o700)
		os.WriteFile(existing, []byte("déjà là\n"), 0o600)
		err := Run(f.old, f.root, f.p, io.Discard)
		if err == nil || !strings.Contains(err.Error(), existing+" existe déjà") {
			t.Fatalf("%s: got %v", name, err)
		}
		if b, _ := os.ReadFile(existing); string(b) != "déjà là\n" {
			t.Fatalf("%s replaced", name)
		}
		equal(t, tree(t, f.p.Home), []string{"journals", "journals/" + name})
	}
}

func TestMigrateExplainsGitUnusable(t *testing.T) {
	f := newFixture(t)
	f.build(f.sample()...).Close()
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin)
	err := Run(f.old, f.root, f.p, io.Discard)
	if !errors.Is(err, repo.ErrGit) || !strings.Contains(err.Error(), "git est nécessaire pour reconnaître les dépôts") {
		t.Fatalf("got %v", err)
	}
	equal(t, tree(t, f.p.Home), nil)
}

func (f *fixture) archived() []string {
	gone := filepath.Join(f.p.Transcripts, "x", "s-archive.jsonl")
	kept := filepath.Join(f.p.Transcripts, "x", "s-un.jsonl")
	return []string{
		`INSERT INTO sessions VALUES ('s-archive','` + f.atelier + `','` + f.atelier + `','Pergola','2026-10-01T10:00:00.000Z','2026-10-01T10:00:02.000Z')`,
		`INSERT INTO messages VALUES
			(10,'u9:0','s-archive','` + gone + `','2026-10-01T10:00:00.000Z','jonathan','` + f.atelier + `','main','la pergola en bois'),
			(11,'u9:1','s-archive','` + gone + `','2026-10-01T10:00:01.000Z','outil','` + f.atelier + `','main','Bash liste'),
			(12,'report:s-archive:abc','s-archive','` + gone + `','2026-10-01T10:00:02.000Z','sous_agent','` + f.atelier + `','main','rapport de jardinage'),
			(13,'u1:0','s-un','` + kept + `','2026-11-02T10:00:00.000Z','jonathan','` + f.root + `',NULL,'bonjour')`,
	}
}

func TestMigrateArchivesMessagesOfDeletedTranscripts(t *testing.T) {
	f := newFixture(t)
	f.build(append(f.sample(), f.archived()...)...).Close()
	transcript := filepath.Join(f.p.Transcripts, "x", "s-un.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o700)
	entry := `{"type":"user","uuid":"u1","sessionId":"s-un","timestamp":"2026-11-02T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"bonjour"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.migrate()
	if !strings.Contains(out, "Messages archivés : 3") {
		t.Errorf("missing count in:\n%s", out)
	}
	archive := filepath.Join(f.p.Journals(), "archive.jsonl")
	got := readJournal(t, archive)
	want := []line{
		{1, "u9:0", "s-archive", "2026-10-01T10:00:00.000Z", "user", "la pergola en bois", "Pergola", &f.atelier},
		{1, "u9:1", "s-archive", "2026-10-01T10:00:01.000Z", "tool", "Bash liste", "", nil},
		{1, "report:s-archive:abc", "s-archive", "2026-10-01T10:00:02.000Z", "subagent", "rapport de jardinage", "", nil},
	}
	if len(got) != 3 || got[0].Cwd == nil || *got[0].Cwd != f.atelier {
		t.Fatalf("got %+v", got)
	}
	got[0].Cwd = want[0].Cwd
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if fi, _ := os.Stat(archive); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	conn := f.open()
	hits, err := search.Search(conn, "pergola", search.Filter{})
	if err != nil || len(hits) != 1 || hits[0].SessionID != "s-archive" || hits[0].Role != "user" {
		t.Fatalf("%+v %v", hits, err)
	}
	equal(t, column(t, conn, "SELECT title FROM sessions WHERE id = 's-archive'"), []string{"Pergola"})
	equal(t, column(t, conn, "SELECT s.name FROM session_repos r JOIN subjects s ON s.uid = r.subject_uid WHERE r.session_id = 's-archive'"), []string{"atelier"})
	equal(t, column(t, conn, "SELECT count(*) FROM messages WHERE session_id = 's-un'"), []string{"1"})
	equal(t, column(t, conn, "SELECT role FROM messages WHERE session_id = 's-archive' ORDER BY ts"), []string{"user", "tool", "subagent"})
}

func TestMigrateSymlinkedTranscriptsAreNotArchived(t *testing.T) {
	f := newFixture(t)
	real := f.p.Transcripts
	f.build(append(f.sample(), f.archived()...)...).Close()
	transcript := filepath.Join(real, "x", "s-un.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o700)
	entry := `{"type":"user","uuid":"u1","sessionId":"s-un","timestamp":"2026-11-02T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"bonjour"}}` + "\n"
	if err := os.WriteFile(transcript, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	f.p.Transcripts = filepath.Join(f.dir, "lien")
	if err := os.Symlink(real, f.p.Transcripts); err != nil {
		t.Fatal(err)
	}
	out := f.migrate()
	if !strings.Contains(out, "Messages archivés : 3") {
		t.Fatal(out)
	}
	equal(t, column(t, f.open(), "SELECT count(*) FROM messages WHERE text = 'bonjour'"), []string{"1"})
}

type line struct {
	V                                  int
	ID, Session, TS, Role, Text, Title string
	Cwd                                *string
}

func readJournal(t *testing.T, path string) []line {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []line
	for _, raw := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

func TestVocalConvertedKeepsSessionIDs(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC+2", 2*3600)
	t.Cleanup(func() { time.Local = saved })
	f := newFixture(t)
	f.build(append(f.sample(),
		`INSERT INTO episodes VALUES (11,'vocal-2023-11-15','2023-11-14T22:13:00.000Z','2023-11-14T22:13:21.000Z','Vocal','v','[]','2023-11-15T00:00:00.000Z',0)`,
		`INSERT INTO forgotten_sessions VALUES ('vocal-perso','2026-11-05T09:00:00.000Z')`,
		`INSERT INTO messages VALUES (20,'vocal:vocal-2023-11-15:1700000000.9999995:agent','vocal-2023-11-15','`+
			filepath.Join(filepath.Dir(f.old), "vocal", "conversation.jsonl")+`','2023-11-14T22:13:21.000Z','sous_agent','`+f.root+`',NULL,'le rapport')`)...).Close()
	f.writeVocal(
		`{"t":1763937000.5,"type":"toi","texte":"  bonjour  ","session":"vocal-perso"}`+"\n",
		`{"t":1763937001,"type":"reponse","reponse":{"action":"repondre","texte":"salut"}}`+"\n",
		`{"t":1700000000.9999996,"type":"agent","resultat":"le rapport"}`+"\n",
	)
	out := f.migrate()
	if !strings.Contains(out, "Lignes vocales converties : 2\n") || !strings.Contains(out, "Lignes vocales écartées (sessions oubliées) : 1\n") {
		t.Fatal(out)
	}
	if b, _ := os.ReadFile(filepath.Join(f.p.Journals(), "vocal.jsonl")); bytes.Contains(b, []byte("bonjour")) {
		t.Fatal("forgotten vocal session copied")
	}
	got := readJournal(t, filepath.Join(f.p.Journals(), "vocal.jsonl"))
	want := []line{
		{1, "1763937001:reponse", "vocal-2025-11-24", "2025-11-23T22:30:01.000Z", "claude", "salut", "Conversation vocale", nil},
		{1, "1700000000.9999996:agent", "vocal-2023-11-15", "2023-11-14T22:13:21.000Z", "subagent", "le rapport", "Conversation vocale", nil},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(f.p.Journals(), "vocal.jsonl")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(f.p.Journals(), "archive.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vocal lines archived: %v", err)
	}
	conn := f.open()
	equal(t, column(t, conn, "SELECT DISTINCT session_id FROM messages ORDER BY session_id"), []string{"vocal-2023-11-15", "vocal-2025-11-24"})
	pending, err := summary.PendingSessions(conn, time.Now(), summary.Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, pending, []string{"vocal-2025-11-24"})
}

func TestVocalSkipsCancelledAndDelegated(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "conversation.jsonl")
	body := strings.Join([]string{
		`{"t":1,"type":"reponse","annule":true,"reponse":{"action":"repondre","texte":"annulée"}}`,
		`{"t":2,"type":"reponse","reponse":{"action":"deleguer","texte":"je délègue"}}`,
		`{"t":3,"type":"reponse","reponse":"pas un objet"}`,
		`{"t":4,"type":"toi","texte":"   "}`,
		`{"t":true,"type":"toi","texte":"t booléen"}`,
		`{"t":"5","type":"toi","texte":"t en texte"}`,
		`{"t":6,"type":"etat","texte":"inconnu"}`,
		`pas du json`,
		``,
		`{"t":7,"type":"toi","texte":"gardée"}`,
		`{"t":9,"type":"toi","texte":"suivie d'autre chose"} reste`,
		`{"t":10,"type":"toi","texte":"deux objets"}{"t":11}`,
		`{"t":12,"type":"toi","texte":"accolade de trop"}}`,
		`{"t":1e20,"type":"toi","texte":"après l'an 9999"}`,
		`{"t":253402300800,"type":"toi","texte":"juste après l'an 9999"}`,
		`{"t":-62135596801,"type":"toi","texte":"avant l'an 1"}`,
		`{"t":8,"type":"toi","texte":"ligne incomplète"}`,
	}, "\n")
	if err := os.WriteFile(from, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, dropped, err := convertVocal(from, filepath.Join(dir, "journals"), map[string]bool{"autre": true})
	if err != nil || n != 1 || dropped != 0 {
		t.Fatalf("%d %v", n, err)
	}
	got := readJournal(t, filepath.Join(dir, "journals", "vocal.jsonl"))
	if len(got) != 1 || got[0].ID != "7:toi" || got[0].Text != "gardée" {
		t.Fatalf("%+v", got)
	}
}

func TestVocalAbsentConvertsNothing(t *testing.T) {
	dir := t.TempDir()
	n, _, err := convertVocal(filepath.Join(dir, "absent.jsonl"), filepath.Join(dir, "journals"), nil)
	if err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journals")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journals created: %v", err)
	}
}
