package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

const (
	s1 = "aaaaaaaa-1111"
	s2 = "bbbbbbbb-2222"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cli-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	scripts := map[string]string{
		"claude": "#!/bin/sh\nexit 97\n",
		"git":    "#!/bin/sh\ncase \"$*\" in *user.name*) echo 'Camille Dupont';; *) exit 1;; esac\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fixture struct {
	t    *testing.T
	p    paths.Paths
	n    int
	conn *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "transcripts")}
	t.Setenv("PAUL_HOME", p.Home)
	t.Setenv("PAUL_TRANSCRIPTS", p.Transcripts)
	saved := time.Local
	time.Local = time.FixedZone("CET", 3600)
	t.Cleanup(func() { time.Local = saved })
	return &fixture{t: t, p: p}
}

func (f *fixture) run(args ...string) (code int, out, errOut string) {
	f.t.Helper()
	var o, e bytes.Buffer
	code = Main(args, &o, &e)
	return code, o.String(), e.String()
}

func (f *fixture) ok(args ...string) string {
	f.t.Helper()
	code, out, errOut := f.run(args...)
	if code != 0 {
		f.t.Fatalf("paul %v: code %d\n%s%s", args, code, out, errOut)
	}
	return out
}

func (f *fixture) fails(want int, args ...string) string {
	f.t.Helper()
	code, out, errOut := f.run(args...)
	if code != want {
		f.t.Fatalf("paul %v: code %d, want %d\n%s%s", args, code, want, out, errOut)
	}
	return out + errOut
}

func (f *fixture) db() *sql.DB {
	f.t.Helper()
	if f.conn != nil {
		return f.conn
	}
	d, err := db.Open(f.p, 5*time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { d.Close() })
	f.conn = d
	return d
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db().Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(query string, args ...any) (n int) {
	f.t.Helper()
	if err := f.db().QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) subject(name string, aliases ...string) string {
	f.t.Helper()
	uid, err := subjects.Create(f.db(), name, aliases, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return uid
}

func (f *fixture) episode(session, title, summary string, subjectUIDs ...string) int64 {
	f.t.Helper()
	f.n++
	uid := fmt.Sprint("e", f.n)
	ts := "2026-10-26T23:45:57.000Z"
	res, err := f.db().Exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, title, summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		uid, session, ts, ts, title, summary, ts, ts)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, s := range subjectUIDs {
		f.exec("INSERT INTO episode_subjects VALUES (?, ?)", uid, s)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fixture) point(episode int64, subject, kind, text string) int64 {
	f.t.Helper()
	f.n++
	var subjectUID any
	if subject != "" {
		subjectUID = subject
	}
	ts := "2026-10-26T23:45:57.000Z"
	res, err := f.db().Exec("INSERT INTO points(uid, episode_id, kind, subject_uid, text, importance, source_ts, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 3, ?, ?, ?)",
		fmt.Sprint("p", f.n), episode, kind, subjectUID, text, ts, ts, ts)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

type obj = map[string]any

var counter atomic.Int64

func message(session, ts, text string) obj {
	return obj{"type": "user", "uuid": fmt.Sprintf("u%d", counter.Add(1)), "sessionId": session,
		"timestamp": ts, "cwd": "/p", "gitBranch": "main", "origin": obj{"kind": "human"},
		"message": obj{"role": "user", "content": text}}
}

func (f *fixture) transcript(session string, lines ...obj) string {
	f.t.Helper()
	path := filepath.Join(f.p.Transcripts, "-work", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			f.t.Fatal(err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) say(session, text string) {
	f.t.Helper()
	f.transcript(session, message(session, "2026-10-26T23:45:57Z", text))
}

func contains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}

func lacks(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Fatalf("%q should not appear in\n%s", u, got)
		}
	}
}

func TestSearchPrintsHitsAndRefreshesTheIndex(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	contains(t, f.ok("search", "pomme"), "pomme")
	f.transcript(s2, message(s2, "2026-10-26T23:45:57Z", "une poire mûre"))
	out := f.ok("search", "poire")
	if want := "2026-10-27 00:45  -  bbbbbbbb  user  une **poire** mûre\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestSearchWithoutResultIsNotAnError(t *testing.T) {
	f := newFixture(t)
	contains(t, f.ok("search", "introuvable"), "No results")
}

func TestSearchSinceAndUntil(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	lacks(t, f.ok("search", "pomme", "--since", "2026-10-28"), "pomme")
	contains(t, f.ok("search", "pomme", "--since", "2026-10-27", "--until", "2026-10-27"), "pomme")
}

func TestSearchJoinedWordsAndLimitAfterTheWords(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme rouge")
	contains(t, f.ok("search", "pomme", "rouge", "--limit", "1"), "**pomme**")
}

func TestSearchSubjectFlag(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.transcript(s2, message(s2, "2026-10-26T23:45:57Z", "une pomme verte"))
	f.ok("index")
	atelier := f.subject("Atelier")
	f.episode(s1, "Pommes", "r", atelier)
	out := f.ok("search", "pomme", "--subject", "atelier")
	contains(t, out, "00:45  Atelier  aaaaaaaa")
	lacks(t, out, "bbbbbbbb")
	contains(t, f.fails(2, "search", "pomme", "--subject", "inconnu"), "unknown subject: inconnu")
}

func TestSearchListsPointsAndMarksThemUsed(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	atelier := f.subject("Atelier")
	f.point(f.episode(s1, "Pommes", "r", atelier), atelier, "decision", "Cueillir la pomme en octobre")
	contains(t, f.ok("search", "pomme"), "Memories:", "decision  Atelier  Cueillir la pomme en octobre")
	if n := f.count("SELECT count(*) FROM point_usage"); n != 1 {
		t.Fatalf("%d points marked used", n)
	}
}

func TestEmptyQueryIsAUsageError(t *testing.T) {
	f := newFixture(t)
	contains(t, f.fails(2, "search", ""), "empty query")
	f.fails(2, "search")
}

func TestInvalidOptionsAreUsageErrors(t *testing.T) {
	f := newFixture(t)
	contains(t, f.fails(2, "search", "x", "--since", "hier"), "invalid date: hier (expected YYYY-MM-DD)")
	contains(t, f.fails(2, "search", "x", "--limit", "0"), "invalid number: 0")
	contains(t, f.fails(2, "souvenirs", "--kind", "sujet"), "invalid kind: sujet")
	contains(t, f.fails(2, "search", "x", "--nope"), "unknown option")
	contains(t, f.fails(2, "search", "x", "--limit"), "missing value")
	contains(t, f.fails(2, "day", "pas-une-date"), "invalid date")
}

func TestDay(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	atelier := f.subject("Atelier")
	f.episode(s1, "Pommes", "r", atelier)
	f.exec("UPDATE sessions SET title = 'Pommes du jour' WHERE id = ?", s1)
	out := f.ok("day", "2026-10-27")
	if want := "2026-10-27 00:45  00:45  aaaaaaaa  Atelier  Pommes du jour  (1 messages)\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	contains(t, f.ok("day", "2026-01-01"), "No sessions that day.")
}

func TestSessionShowsSummaryPointsAndMessages(t *testing.T) {
	f := newFixture(t)
	f.transcript(s1, message(s1, "2026-10-26T23:45:57Z", "une pomme"), obj{"type": "ai-title", "sessionId": s1, "aiTitle": "Les pommes"})
	f.ok("index")
	atelier := f.subject("Atelier")
	id := f.point(f.episode(s1, "Résumé des pommes", "On a parlé de pommes.", atelier), atelier, "fact", "Les pommes sont rouges")
	out := f.ok("session", "aaaa")
	contains(t, out, "Les pommes\n", "Summary: Résumé des pommes [Atelier]\n", "  On a parlé de pommes.\n",
		fmt.Sprintf("  %d  fact  Atelier  Les pommes sont rouges\n", id), "\n2026-10-27 00:45  user\nune pomme")
}

func TestSessionHidesSummaryAfterForgettingAPoint(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	ep := f.episode(s1, "Titre périmé", "Résumé périmé")
	f.point(ep, "", "fact", "Reste")
	id := f.point(ep, "", "fact", "Oublié")
	f.ok("forget", "point", fmt.Sprint(id), "--yes")
	out := f.ok("session", s1)
	lacks(t, out, "Titre périmé", "Résumé périmé")
	contains(t, out, "Reste")
}

func TestSessionWithoutMessagesShownAndForgotten(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	f.point(f.episode(s1, "Résumé gardé", "Les pommes."), "", "fact", "Un fait gardé")
	os.RemoveAll(f.p.Transcripts)
	f.ok("rebuild")
	contains(t, f.ok("session", "aaaa"), "Summary: Résumé gardé", "Un fait gardé")
	contains(t, f.ok("forget", "--yes", "session", "aaaa"), "Forgotten: session aaaaaaaa")
	if n := f.count("SELECT count(*) FROM points"); n != 0 {
		t.Fatalf("%d points", n)
	}
}

func TestSessionUntitledAndMissing(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	contains(t, f.ok("session", s1), "(untitled)")
	contains(t, f.ok("session", "zzzz"), "Session not found.")
}

func TestSessionAmbiguousIsAUsageError(t *testing.T) {
	f := newFixture(t)
	f.say("aaaa-1", "une pomme")
	f.say("aaaa-2", "une poire")
	contains(t, f.fails(2, "session", "aaaa"), "ambigu")
}

func TestControlCharactersAreStripped(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	atelier := f.subject("Ate\x1b[31mlier")
	f.point(f.episode(s1, "t", "r", atelier), atelier, "fact", "ligne\x1b[2J un\nsaut")
	out := f.ok("souvenirs")
	lacks(t, out, "\x1b")
	contains(t, out, "ligne [2J un saut")
}

func TestIndexAndRebuild(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	out := f.ok("index")
	contains(t, out, "files: 1\n", "messages: 1\n", "malformed lines: 0\n", "failed files: 0\n")
	f.exec("DELETE FROM messages")
	f.exec("DELETE FROM files")
	contains(t, f.ok("rebuild"), "messages: 1\n")
	if n := f.count("SELECT count(*) FROM messages"); n != 1 {
		t.Fatalf("%d messages", n)
	}
}

func TestIndexFailureExitsWithOne(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.p.Transcripts, "-work", "x.jsonl")
	f.say("x", "une pomme")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	contains(t, f.fails(1, "index"), "failed files: 1")
}

func TestSouvenirsListsFiltersAndMarksUsed(t *testing.T) {
	f := newFixture(t)
	atelier := f.subject("Atelier")
	ep := f.episode(s1, "t", "r", atelier)
	f.point(ep, atelier, "decision", "Garder sqlite")
	pending := f.point(ep, atelier, "pending", "Écrire la doc")
	closed := f.point(ep, atelier, "pending", "Déjà fait")
	f.exec("UPDATE points SET closed_at = '2026-10-27T00:00:00.000Z' WHERE id = ?", closed)
	all := f.ok("souvenirs")
	contains(t, all, "decision  Atelier  Garder sqlite", fmt.Sprintf("%d  pending  Atelier  Écrire la doc\n", pending),
		fmt.Sprintf("%d  pending  Atelier  Déjà fait  [closed]", closed))
	open := f.ok("souvenirs", "--open")
	lacks(t, open, "Garder sqlite", "Déjà fait")
	contains(t, open, "Écrire la doc")
	only := f.ok("souvenirs", "sqlite", "--kind", "decision", "--limit", "5")
	lacks(t, only, "Écrire la doc")
	if n := f.count("SELECT count(*) FROM point_usage"); n != 3 {
		t.Fatalf("%d points marked used, want 3", n)
	}
	contains(t, f.ok("souvenirs", "rien", "de", "tel"), "No memories.")
}

func TestSouvenirsHistoryFlag(t *testing.T) {
	f := newFixture(t)
	atelier := f.subject("Atelier")
	ep := f.episode(s1, "t", "r", atelier)
	old := f.point(ep, atelier, "fact", "Le port vaut 80")
	f.point(ep, atelier, "fact", "Le port vaut 8080")
	f.exec("UPDATE points SET superseded_by_uid = 'p9', superseded_at = '2026-10-28T00:00:00.000Z' WHERE id = ?", old)
	lacks(t, f.ok("souvenirs", "port"), "vaut 80\n")
	contains(t, f.ok("souvenirs", "port", "--history"), "Le port vaut 80\n", "Le port vaut 8080")
}

func TestSouvenirsSubjectShowsSynthesisAndDescendants(t *testing.T) {
	f := newFixture(t)
	maison := f.subject("Maison")
	reseau := f.subject("Réseau")
	if err := subjects.SetParent(f.db(), reseau, maison, subjects.Origin{}); err != nil {
		t.Fatal(err)
	}
	f.point(f.episode(s1, "t", "r", reseau), reseau, "fact", "Le routeur est au salon")
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at) VALUES (?, 'y', 'La maison est calme.', 't', 't')", maison)
	out := f.ok("souvenirs", "--subject", "maison")
	contains(t, out, "Synthesis: La maison est calme.\n", "Le routeur est au salon")
	contains(t, f.fails(2, "souvenirs", "--subject", "nulle part"), "unknown subject")
}

func TestSubjectsTree(t *testing.T) {
	f := newFixture(t)
	maison := f.subject("Maison", "domotique")
	reseau := f.subject("Réseau")
	routeur := f.subject("Routeur")
	if err := subjects.SetParent(f.db(), reseau, maison, subjects.Origin{}); err != nil {
		t.Fatal(err)
	}
	if err := subjects.Link(f.db(), reseau, "depends_on", routeur, subjects.Origin{}); err != nil {
		t.Fatal(err)
	}
	want := "Maison  [domotique]\n  Réseau\n    depends_on Routeur\nRouteur\n"
	if out := f.ok("subjects"); out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if out := f.ok("subjects", "domotique"); out != "Maison  [domotique]\n  Réseau\n    depends_on Routeur\n" {
		t.Fatalf("got %q", out)
	}
	contains(t, f.fails(2, "subjects", "inconnu"), "unknown subject")
}

func TestSubjectsEmpty(t *testing.T) {
	contains(t, newFixture(t).ok("subjects"), "No subjects.")
}

func TestMemoForTheCurrentRepoAndForANamedSubject(t *testing.T) {
	f := newFixture(t)
	repo, err := subjects.EnsureRepo(f.db(), "example/widget", "widget")
	if err != nil {
		t.Fatal(err)
	}
	f.point(f.episode(s1, "t", "r", repo), repo, "decision", "Utiliser sqlite")
	dir := t.TempDir()
	f.exec("INSERT INTO repo_cache VALUES (?, ?, 't')", dir, repo)
	t.Chdir(dir)
	contains(t, f.ok("memo"), "Subject \"widget\"", "Utiliser sqlite")
	contains(t, f.ok("memo", "--subject", "widget"), "Subject \"widget\"", "Utiliser sqlite")
	contains(t, f.fails(2, "memo", "--subject", "inconnu"), "unknown subject")
	if n := f.count("SELECT count(*) FROM point_usage"); n != 0 {
		t.Fatalf("paul memo marked %d points", n)
	}
	t.Chdir(t.TempDir())
	contains(t, f.ok("memo"), "Latest sessions:")
	f.exec("DELETE FROM episode_subjects")
	f.exec("DELETE FROM episodes")
	f.exec("DELETE FROM sessions")
	f.exec("DELETE FROM messages")
	contains(t, f.ok("memo"), "No memories for this directory.")
}

func TestMemoWithADeletedWorkingDirectory(t *testing.T) {
	f := newFixture(t)
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	contains(t, f.fails(2, "memo"), "the current directory no longer exists")
}

func TestSummarizeTranslatesStatuses(t *testing.T) {
	f := newFixture(t)
	contains(t, f.ok("summarize"), "No session to summarize.")
	f.say(s1, "une pomme")
	out := f.fails(1, "summarize", "--all", "--session", "aaaa")
	contains(t, out, "aaaaaaaa  failed", "Total: 0 points")
}

func TestSummarizeForceNeedsASession(t *testing.T) {
	contains(t, newFixture(t).fails(2, "summarize", "--force"), "--force requires --session")
}

func TestSummarizeWhileAnotherRuns(t *testing.T) {
	f := newFixture(t)
	if err := f.p.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	release, acquired, err := lock.Writer(f.p.SummaryLock(), false)
	if err != nil || !acquired {
		t.Fatal(err, acquired)
	}
	defer release()
	contains(t, f.ok("summarize"), "A summary is already running.")
}

func TestForgetWithoutYesDescribesAndExitsWithTwo(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	id := f.point(f.episode(s1, "t", "r"), "", "fact", "Un fait")
	out := f.fails(2, "forget", "point", fmt.Sprint(id))
	contains(t, out, "Would erase from memory: point", "Un fait", "--yes")
	out = f.fails(2, "forget", "session", "aaaa")
	contains(t, out, "Would erase from memory: session aaaaaaaa", "stays on disk")
	if n := f.count("SELECT count(*) FROM points"); n != 1 {
		t.Fatalf("%d points", n)
	}
}

func TestForgetPointAndSession(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme")
	f.ok("index")
	id := f.point(f.episode(s1, "t", "r"), "", "fact", "Un fait")
	contains(t, f.ok("forget", "point", fmt.Sprint(id), "--yes"), "Forgotten: point")
	if n := f.count("SELECT count(*) FROM points"); n != 0 {
		t.Fatalf("%d points", n)
	}
	contains(t, f.ok("forget", "--yes", "session", "aaaa"), "Forgotten: session aaaaaaaa")
	if n := f.count("SELECT count(*) FROM messages"); n != 0 {
		t.Fatalf("%d messages", n)
	}
	contains(t, f.fails(2, "forget", "point", "999", "--yes"), "point not found: 999")
	contains(t, f.fails(2, "forget", "session", "zzzz", "--yes"), "session not found")
	contains(t, f.fails(2, "forget", "point", "abc"), "invalid number")
	f.fails(2, "forget", "tout", "1")
	f.fails(2, "forget", "point")
}

func TestOldCommandsAreUnknown(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"label", "access", "projects", "jour", "oublier", "resumer", "classer"} {
		contains(t, f.fails(2, name), "unknown command: "+name)
	}
}

func TestNotYetAvailableCommands(t *testing.T) {
	f := newFixture(t)
	contains(t, f.fails(2, "sync"), "not available yet")
	contains(t, f.ok("--help"), "  sync                          not available yet\n")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestWebPrintsItsAddressAndStopsOnInterrupt(t *testing.T) {
	f := newFixture(t)
	contains(t, f.fails(2, "web", "--port", "1"), "web: unknown option: -port")
	var out, errOut lockedBuffer
	done := make(chan int)
	go func() { done <- Main([]string{"web", "--no-open"}, &out, &errOut) }()
	var address string
	for deadline := time.Now().Add(5 * time.Second); address == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		address = strings.TrimSpace(out.String())
	}
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/\?token=[A-Za-z0-9_-]{43}$`).MatchString(address) {
		t.Fatalf("%q", address)
	}
	res, err := http.Get(address)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(res, err)
	}
	res.Body.Close()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 || errOut.String() != "" {
		t.Fatalf("code %d, %q", code, errOut.String())
	}
}

func TestNoCommandPrintsUsage(t *testing.T) {
	f := newFixture(t)
	contains(t, f.fails(2), "Usage: paul <command>")
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = "1.2.3"
	if out := f.ok("--version"); out != "paul 1.2.3\n" {
		t.Fatalf("got %q", out)
	}
}

func TestLogNeverOnStderr(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.p.Transcripts, "-work", "x.jsonl")
	f.say("x", "une pomme")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	var stray bytes.Buffer
	stdLogTo(&stray, t)
	code, out, errOut := f.run("index")
	if code != 1 || strings.Contains(out+errOut, "indexing failed") || stray.Len() != 0 || errOut != "" && !strings.HasPrefix(errOut, "paul: index failed: see paul.log") {
		t.Fatalf("journal outside paul.log : code %d\n%s%s\n%s", code, out, errOut, stray.String())
	}
	raw, err := os.ReadFile(f.p.Log())
	if err != nil || !strings.Contains(string(raw), "indexing failed") {
		t.Fatalf("paul.log : %q %v", raw, err)
	}
	if info, _ := os.Stat(f.p.Log()); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	log := stdLogWriter()
	if log != &stray {
		t.Fatalf("the standard log is not restored")
	}
}

func stdLogTo(w io.Writer, t *testing.T) {
	saved := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(saved) })
}

func stdLogWriter() io.Writer { return log.Writer() }

func TestHelpPrintsUsageAndExitsWithZero(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{{"-h"}, {"--help"}, {"search", "--help"}, {"souvenirs", "-h"}, {"forget", "--help"}} {
		contains(t, f.ok(args...), "Usage: paul <command>")
	}
}

func TestFailureLineNamesNoGoType(t *testing.T) {
	f := newFixture(t)
	if err := f.p.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.p.DB(), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, errOut := f.run("souvenirs")
	if errOut != "paul: souvenirs failed: see paul.log\n" {
		t.Fatalf("got %q", errOut)
	}
}

func TestDoubleDashEndsTheOptions(t *testing.T) {
	f := newFixture(t)
	f.say(s1, "une pomme since x")
	contains(t, f.ok("search", "--", "--since", "pomme"), "**pomme**")
	contains(t, f.ok("search", "--", "pomme", "--since", "x"), "**pomme**")
	contains(t, f.fails(2, "search", "pomme", "--since"), "missing value")
}

func TestForgetSessionDescriptionCountsWhatGoes(t *testing.T) {
	f := newFixture(t)
	f.transcript(s1, message(s1, "2026-10-26T23:45:57Z", "une pomme"), message(s1, "2026-10-26T23:46:57Z", "une poire"))
	f.ok("index")
	ep := f.episode(s1, "t", "r")
	f.point(ep, "", "fact", "A")
	f.point(ep, "", "fact", "B")
	contains(t, f.fails(2, "forget", "session", "aaaa"), "(2 messages, 1 episodes, 2 points)")
}

func TestAmbiguousSubjectListsCandidates(t *testing.T) {
	f := newFixture(t)
	a := f.subject("Alpha", "jumeau")
	b := f.subject("Beta", "jumeau")
	out := f.fails(2, "souvenirs", "--subject", "jumeau")
	contains(t, out, "several subjects share this name: jumeau", "Alpha "+a, "Beta "+b)
	f.fails(2, "memo", "--subject", "jumeau")
}

func TestBareUnknownAndHelpDoNotCreateTheHome(t *testing.T) {
	f := newFixture(t)
	f.fails(2)
	f.fails(2, "nimporte")
	f.ok("--help")
	f.ok("--version")
	if _, err := os.Stat(f.p.Home); !os.IsNotExist(err) {
		t.Fatalf("PAUL_HOME created: %v", err)
	}
}

func TestHookNeverUsesStderr(t *testing.T) {
	f := newFixture(t)
	blocker := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(blocker, []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAUL_HOME", blocker)
	t.Setenv("PAUL_TRANSCRIPTS", f.p.Transcripts)
	stdin := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(stdin, []byte(`{"session_id":"s","prompt":"Revoyons la facturation"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = saved; in.Close() })
	// start and end re-execute os.Executable, which is the test binary here.
	code, out, errOut := f.run("hook", "prompt")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}
