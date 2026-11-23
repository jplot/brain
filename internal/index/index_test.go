package index

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/transcript"
)

type fixture struct {
	t  *testing.T
	p  paths.Paths
	db *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "transcripts")}
	d, err := db.Open(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &fixture{t: t, p: p, db: d}
}

func (f *fixture) all() Stats { return All(f.db, f.p) }

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) one(query string, args ...any) string {
	f.t.Helper()
	var v sql.NullString
	if err := f.db.QueryRow(query, args...).Scan(&v); err != nil {
		f.t.Fatal(err)
	}
	return v.String
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) messages() int { return f.count("SELECT count(*) FROM messages") }

func (f *fixture) texts() []string {
	f.t.Helper()
	rows, err := f.db.Query("SELECT text FROM messages ORDER BY text")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (f *fixture) offset(path string) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow("SELECT offset FROM files WHERE path = ?", path).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) transcript(folder, id string, lines ...any) string {
	f.t.Helper()
	path := filepath.Join(f.p.Transcripts, folder, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		f.t.Fatal(err)
	}
	appendLines(f.t, path, lines...)
	return path
}

func appendLines(t *testing.T, path string, lines ...any) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}
	appendRaw(t, path, b.String())
}

func appendRaw(t *testing.T, path, text string) {
	t.Helper()
	h, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

type obj = map[string]any

var counter atomic.Int64

func base(kind string, extra obj) obj {
	m := obj{"type": kind, "uuid": fmt.Sprintf("u%d", counter.Add(1)), "sessionId": "s",
		"timestamp": "2026-10-26T23:45:57Z", "cwd": "/p", "gitBranch": "main"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func user(text string) obj {
	return base("user", obj{"origin": obj{"kind": "human"}, "message": obj{"role": "user", "content": text}})
}

func at(ts string, m obj) obj { m["timestamp"] = ts; return m }

func withCwd(m obj, cwd string) obj { m["cwd"] = cwd; return m }

func withoutCwd(m obj) obj { delete(m, "cwd"); return m }

func title(text string) obj { return obj{"type": "ai-title", "sessionId": "s", "aiTitle": text} }

func assistantUse(id string) obj {
	return base("assistant", obj{"message": obj{"role": "assistant", "content": []any{
		obj{"type": "tool_use", "id": id, "name": "Agent", "input": obj{}}}}})
}

func agentResult(id, text string) obj {
	return base("user", obj{"toolUseResult": obj{}, "message": obj{"role": "user", "content": []any{
		obj{"type": "tool_result", "tool_use_id": id, "content": text}}}})
}

func TestDeepNestingInMessageKeepsSession(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s", user("avant"), user(strings.Repeat("token=a(", 997)), user("apres"))
	stats := f.all()
	if stats.Failed != 0 || f.messages() != 3 {
		t.Fatalf("stats %+v, messages %d", stats, f.messages())
	}
}

func TestIncremental(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("un"))
	if got := f.all().Messages; got != 1 {
		t.Fatalf("first pass: %d", got)
	}
	appendLines(t, path, user("deux"))
	if got := f.all().Messages; got != 1 {
		t.Fatalf("second pass: %d", got)
	}
	if f.messages() != 2 {
		t.Fatalf("messages %d", f.messages())
	}
}

func TestTruncatedLastLineWaits(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("un"))
	raw, _ := json.Marshal(user("deux"))
	appendRaw(t, path, string(raw[:10]))
	stats := f.all()
	if f.messages() != 1 || stats.Malformed != 0 {
		t.Fatalf("messages %d, stats %+v", f.messages(), stats)
	}
	appendRaw(t, path, string(raw[10:])+"\n")
	if got := f.all().Messages; got != 1 {
		t.Fatalf("completed line: %d", got)
	}
	if got := f.all().Messages; got != 0 || f.messages() != 2 {
		t.Fatalf("third pass %d, messages %d", got, f.messages())
	}
}

func TestShortenedFileStartsOver(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("ancien un"), user("ancien deux"))
	f.all()
	f.transcript("-p", "s1", user("neuf"))
	stats := f.all()
	if got := f.texts(); len(got) != 1 || got[0] != "neuf" || stats.Messages != 1 {
		t.Fatalf("texts %v, stats %+v", got, stats)
	}
	if info, _ := os.Stat(path); f.offset(path) != info.Size() {
		t.Fatalf("offset %d", f.offset(path))
	}
}

func TestShortenedFileForgetsOldTextInSearch(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s1", user("ancienne pomme"), user("ancienne poire"))
	f.all()
	f.transcript("-p", "s1", user("neuf"))
	f.all()
	match := "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?"
	if f.count(match, "ancienne") != 0 || f.count(match, "neuf") != 1 {
		t.Fatal("search index not refreshed")
	}
}

func TestShortenedFileRecomputesSession(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s1", at("2026-01-01T00:00:00Z", user("ancien")), title("Ancien titre"))
	f.all()
	f.transcript("-p", "s1", at("2026-02-01T00:00:00Z", user("neuf")))
	f.all()
	if got := f.one("SELECT started_at FROM sessions"); got != "2026-02-01T00:00:00.000Z" {
		t.Fatalf("started_at %q", got)
	}
	if got := f.one("SELECT title FROM sessions"); got != "" {
		t.Fatalf("title %q", got)
	}
}

func TestDisappearedFileKeepsItsMessages(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("un"))
	f.all()
	os.Remove(path)
	f.all()
	if f.messages() != 1 {
		t.Fatalf("messages %d", f.messages())
	}
}

func TestDanglingJournalIsSkipped(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.p.Journals(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.p.Home, "fantome"), filepath.Join(f.p.Journals(), "x.jsonl")); err != nil {
		t.Fatal(err)
	}
	if stats := f.all(); stats != (Stats{}) {
		t.Fatalf("stats %+v", stats)
	}
}

func TestSubagentTranscriptsIgnored(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s1", user("principal"))
	f.transcript("-p/s1/subagents", "agent-x", user("interne"))
	f.all()
	if got := f.texts(); len(got) != 1 || got[0] != "principal" {
		t.Fatalf("texts %v", got)
	}
}

func TestInvalidLineSkipped(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("avant"))
	appendRaw(t, path, "{cassé\n\xff\xfe invalide\n")
	appendLines(t, path, user("apres"))
	stats := f.all()
	if got := f.texts(); len(got) != 2 || got[0] != "apres" || got[1] != "avant" {
		t.Fatalf("texts %v", got)
	}
	if stats.Malformed != 2 {
		t.Fatalf("malformed %d", stats.Malformed)
	}
}

func TestInvalidUTF8InsideStringIsRepairedNotMalformed(t *testing.T) {
	f := newFixture(t)
	raw, _ := json.Marshal(user("octet ZZ mauvais"))
	path := f.transcript("-p", "s1")
	appendRaw(t, path, strings.Replace(string(raw), "ZZ", "\xff", 1)+"\n")
	stats := f.all()
	got := f.texts()
	if stats.Malformed != 0 || len(got) != 1 || !utf8.ValidString(got[0]) || !strings.Contains(got[0], "\uFFFD") {
		t.Fatalf("stats %+v, texts %q", stats, got)
	}
}

func TestBlankLineIgnored(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("un"))
	appendRaw(t, path, "\n  \n")
	if got := f.all().Malformed; got != 0 {
		t.Fatalf("malformed %d", got)
	}
}

func TestNulInCwdCountsAsMalformed(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", withCwd(user("mauvais"), "/p/a\x00b"))
	appendLines(t, path, user("bon"))
	stats := f.all()
	if got := f.texts(); stats.Malformed != 1 || stats.Failed != 0 || len(got) != 1 || got[0] != "bon" {
		t.Fatalf("stats %+v, texts %v", stats, got)
	}
}

func TestPathologicalLinesNeverBlockFile(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("avant"))
	appendRaw(t, path, strings.Repeat("[", 300000)+"\n")
	appendLines(t, path, withCwd(user("nul"), "a\x00b"))
	appendRaw(t, path, "{pas du json\n")
	appendLines(t, path, user("apres"))
	stats := f.all()
	if got := f.texts(); len(got) != 2 || got[0] != "apres" || got[1] != "avant" {
		t.Fatalf("texts %v", got)
	}
	if stats.Malformed != 3 || stats.Failed != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if again := f.all(); again.Malformed != 0 || again.Messages != 0 {
		t.Fatalf("second pass %+v", again)
	}
}

func TestFailureRollsBackOnlyThatFile(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", user("un"))
	f.all()
	before := f.offset(path)
	appendLines(t, path, user("deux secret"), user("trois"))
	other := f.transcript("-q", "s2", user("ailleurs"))
	f.exec("CREATE TRIGGER boom BEFORE INSERT ON messages WHEN new.text = 'trois' BEGIN SELECT RAISE(ABORT, 'secret'); END")
	var logged strings.Builder
	stats := f.allCapturingLog(&logged)
	if f.offset(path) != before || stats.Failed != 1 {
		t.Fatalf("offset %d (was %d), stats %+v", f.offset(path), before, stats)
	}
	for _, text := range f.texts() {
		if text == "deux secret" {
			t.Fatal("partial file kept")
		}
	}
	if info, _ := os.Stat(other); f.offset(other) != info.Size() {
		t.Fatal("other file not indexed")
	}
	if !strings.Contains(logged.String(), path) || strings.Contains(logged.String(), "secret") {
		t.Fatalf("log %q", logged.String())
	}
}

func TestSessionAndTitle(t *testing.T) {
	f := newFixture(t)
	first := withCwd(at("2026-01-02T10:00:00Z", user("un")), "/a")
	second := withCwd(at("2026-01-01T09:00:00Z", user("deux")), "/b")
	last := at("2026-01-03T11:00:00Z", user("trois"))
	f.transcript("-p", "s1", first, title("Premier"), second, last, title("Dernier"))
	f.all()
	got := []string{f.one("SELECT started_at FROM sessions"), f.one("SELECT ended_at FROM sessions"),
		f.one("SELECT launch_cwd FROM sessions"), f.one("SELECT title FROM sessions")}
	want := []string{"2026-01-01T09:00:00.000Z", "2026-01-03T11:00:00.000Z", "/a", "Dernier"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", got)
	}
}

func TestTitleBeforeAnyMessage(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", title("Tôt"))
	f.all()
	if f.one("SELECT title FROM sessions") != "Tôt" || f.one("SELECT launch_cwd FROM sessions") != "" || f.one("SELECT started_at FROM sessions") != "" {
		t.Fatal("title-only session wrong")
	}
	appendLines(t, path, at("2026-01-01T00:00:00Z", user("un")))
	f.all()
	if f.one("SELECT title FROM sessions") != "Tôt" || f.one("SELECT launch_cwd FROM sessions") != "/p" || f.one("SELECT started_at FROM sessions") != "2026-01-01T00:00:00.000Z" {
		t.Fatal("session not completed")
	}
}

func TestAgentReportAcrossPasses(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", assistantUse("toolu_1"))
	f.all()
	appendLines(t, path, agentResult("toolu_1", "rapport final"))
	f.all()
	if got := f.one("SELECT role FROM messages WHERE text = 'rapport final'"); got != "subagent" {
		t.Fatal(got)
	}
}

func TestForgottenSessionsIgnored(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1", title("Oublié"), user("secret"))
	f.exec("INSERT INTO forgotten_sessions(session_id, at) VALUES ('s', '2026-01-01T00:00:00.000Z')")
	f.all()
	appendLines(t, path, user("encore"))
	f.all()
	if f.messages() != 0 || f.count("SELECT count(*) FROM sessions") != 0 {
		t.Fatal("forgotten session indexed")
	}
}

func (f *fixture) journal(lines ...any) string {
	f.t.Helper()
	path := filepath.Join(f.p.Journals(), "journal.jsonl")
	if err := os.MkdirAll(f.p.Journals(), 0o700); err != nil {
		f.t.Fatal(err)
	}
	appendLines(f.t, path, lines...)
	return path
}

func journalLine(id, role, text string) obj {
	return obj{"v": 1, "id": id, "session": "vs", "ts": "2026-10-26T23:45:57Z", "role": role, "text": text}
}

func TestJournalIndexed(t *testing.T) {
	f := newFixture(t)
	path := f.journal(journalLine("1", "user", "bonjour"), journalLine("2", "claude", "salut"))
	stats := f.all()
	if stats.Messages != 2 || stats.Files != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if f.one("SELECT role FROM messages WHERE text = 'bonjour'") != "user" || f.one("SELECT file FROM messages WHERE text = 'salut'") != path {
		t.Fatal("journal messages wrong")
	}
	appendLines(t, path, journalLine("3", "user", "trois"))
	if got := f.all().Messages; got != 1 {
		t.Fatalf("second pass %d", got)
	}
}

func TestJournalMalformedLineCounted(t *testing.T) {
	f := newFixture(t)
	path := f.journal(journalLine("1", "user", "ok"))
	appendRaw(t, path, "pas du json\n")
	appendLines(t, path, obj{"v": 2, "id": "x"})
	stats := f.all()
	if stats.Messages != 1 || stats.Malformed != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestRefreshDoesNothingWhileLockTaken(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s1", user("un"))
	release, acquired, err := lock.Writer(f.p.IndexLock(), true)
	if err != nil || !acquired {
		t.Fatal(err)
	}
	if err := Refresh(f.db, f.p); err != nil {
		t.Fatal(err)
	}
	release()
	if f.messages() != 0 {
		t.Fatal("indexed under a foreign lock")
	}
	if err := Refresh(f.db, f.p); err != nil || f.messages() != 1 {
		t.Fatalf("err %v, messages %d", err, f.messages())
	}
}

func TestRunTakesTheLock(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s1", user("un"))
	stats, err := Run(f.p)
	if err != nil || stats.Messages != 1 {
		t.Fatalf("stats %+v, err %v", stats, err)
	}
}

func TestConcurrentRunsNeverIndexTwiceTheSameLine(t *testing.T) {
	f := newFixture(t)
	path := f.transcript("-p", "s1")
	var inFlight, peak atomic.Int64
	parseClaude = func(raw []byte, is func(string) bool) ([]transcript.Item, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		time.Sleep(50 * time.Microsecond)
		return transcript.ParseClaudeLine(raw, is)
	}
	t.Cleanup(func() { parseClaude = transcript.ParseClaudeLine })

	const lines = 600
	var done atomic.Bool
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pass := true; pass || !done.Load(); pass = false {
				stats, err := Run(f.p)
				if err != nil || stats.Failed != 0 {
					failed.Add(1)
				}
			}
		}()
	}
	h, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	for i := range lines {
		raw, _ := json.Marshal(user(fmt.Sprintf("ligne %d", i)))
		raw = append(raw, '\n')
		h.Write(raw[:len(raw)/2])
		h.Write(raw[len(raw)/2:])
	}
	h.Close()
	time.Sleep(200 * time.Millisecond)
	done.Store(true)
	wg.Wait()
	if _, err := Run(f.p); err != nil {
		t.Fatal(err)
	}
	if failed.Load() != 0 || peak.Load() != 1 {
		t.Fatalf("failed %d, peak parallel indexers %d", failed.Load(), peak.Load())
	}
	if f.messages() != lines {
		t.Fatalf("messages %d", f.messages())
	}
	if info, _ := os.Stat(path); f.offset(path) != info.Size() {
		t.Fatal("offset not at end")
	}
}

func TestConcurrentFirstRuns(t *testing.T) {
	for attempt := range 15 {
		dir := t.TempDir()
		p := paths.Paths{Home: filepath.Join(dir, fmt.Sprint(attempt), "paul"), Transcripts: filepath.Join(dir, "t")}
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		start := make(chan struct{})
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := Run(p)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("attempt %d: %v", attempt, err)
			}
		}
	}
}

const secret = "ghp_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"

func (f *fixture) oldBase(version int) {
	f.t.Helper()
	path := f.transcript("-p", "s1", user("clé "+secret+" fin"))
	f.exec(`INSERT INTO messages(key, session_id, file, ts, role, text)
		VALUES ('vieux', 's', ?, '2026-01-01T00:00:00.000Z', 'user', ?)`, path, "clé "+secret+" fin")
	f.exec(fmt.Sprintf("PRAGMA user_version=%d", version))
	f.exec("PRAGMA wal_checkpoint(TRUNCATE)")
}

func (f *fixture) version() int { return f.count("PRAGMA user_version") }

func (f *fixture) fileBytes() string {
	f.t.Helper()
	data, err := os.ReadFile(f.p.DB())
	if err != nil {
		f.t.Fatal(err)
	}
	wal, _ := os.ReadFile(f.p.DB() + "-wal")
	return string(data) + string(wal)
}

func (f *fixture) refresh() {
	f.t.Helper()
	if err := Refresh(f.db, f.p); err != nil {
		f.t.Fatal(err)
	}
}

func TestUpgradeLeavesNoSecretInFile(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newFixture(t)
			f.oldBase(version)
			if !strings.Contains(f.fileBytes(), secret[10:]) {
				t.Fatal("fixture does not hold the secret")
			}
			f.refresh()
			if f.version() != db.IndexVersion || strings.Contains(f.fileBytes(), secret[10:]) {
				t.Fatalf("version %d, secret still in file", f.version())
			}
			if f.messages() != 1 {
				t.Fatalf("messages %d", f.messages())
			}
		})
	}
}

func TestUpgradeKeepsState(t *testing.T) {
	f := newFixture(t)
	f.oldBase(1)
	f.exec("INSERT INTO subjects(uid, kind, name, aliases, created_at, updated_at) VALUES ('s1', 'subject', 'p', '[]', 't', 't')")
	f.exec("INSERT INTO links VALUES ('s1', 'about', 's1', 1, 'user', NULL, 't')")
	f.refresh()
	if f.count("SELECT count(*) FROM subjects") != 1 || f.count("SELECT count(*) FROM links") != 1 || f.version() != db.IndexVersion {
		t.Fatal("state or version wrong")
	}
}

func TestFreshBaseTakesVersionWithoutRebuild(t *testing.T) {
	f := newFixture(t)
	f.refresh()
	if f.version() != db.IndexVersion || f.messages() != 0 {
		t.Fatal("fresh base wrong")
	}
}

func TestSingleRebuildAcrossConcurrentRuns(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		lines := make([]any, 20)
		for j := range lines {
			lines[j] = user(fmt.Sprintf("message %d %d", i, j))
		}
		f.transcript("-p", fmt.Sprintf("s%d", i), lines...)
	}
	f.exec("INSERT INTO messages(key, session_id, file, ts, role, text) VALUES ('vieux', 's', 'f', '2026-01-01T00:00:00.000Z', 'user', 'x')")
	f.exec("PRAGMA user_version=1")
	var parsed atomic.Int64
	parseClaude = func(raw []byte, is func(string) bool) ([]transcript.Item, error) {
		parsed.Add(1)
		return transcript.ParseClaudeLine(raw, is)
	}
	t.Cleanup(func() { parseClaude = transcript.ParseClaudeLine })
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Run(f.p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if parsed.Load() != 100 || f.messages() != 100 {
		t.Fatalf("parsed %d lines, messages %d", parsed.Load(), f.messages())
	}
}

func TestRunReportsRebuildStats(t *testing.T) {
	f := newFixture(t)
	f.oldBase(1)
	stats, err := Run(f.p)
	if err != nil || stats.Messages != 1 || stats.Files != 1 {
		t.Fatalf("stats %+v, err %v", stats, err)
	}
}

func TestInterruptedUpgradeStillCleans(t *testing.T) {
	f := newFixture(t)
	f.exec("PRAGMA secure_delete=OFF")
	f.exec("INSERT INTO messages(key, session_id, file, ts, role, text) VALUES ('k', 's', 'f', '2026-01-01T00:00:00.000Z', 'user', ?)", secret+" "+strings.Repeat("x", 20000))
	if err := db.RebuildIndex(f.db); err != nil {
		t.Fatal(err)
	}
	f.exec("PRAGMA user_version=0")
	f.exec("PRAGMA wal_checkpoint(TRUNCATE)")
	f.refresh()
	if f.version() != db.IndexVersion || strings.Contains(f.fileBytes(), secret[10:]) {
		t.Fatal("leftover not cleaned")
	}
}

func TestVacuumReleasesFreePages(t *testing.T) {
	f := newFixture(t)
	for i := range 50 {
		f.exec("INSERT INTO messages(key, session_id, file, ts, role, text) VALUES (?, 's', 'f', '2026-01-01T00:00:00.000Z', 'user', ?)", fmt.Sprint(i), strings.Repeat("x", 5000))
	}
	f.exec("DELETE FROM messages")
	if f.count("PRAGMA freelist_count") == 0 {
		t.Fatal("no free pages to release")
	}
	if err := vacuum(f.db); err != nil {
		t.Fatal(err)
	}
	if f.count("PRAGMA freelist_count") != 0 {
		t.Fatal("free pages kept")
	}
}

func TestFailedCleanupIsRetried(t *testing.T) {
	f := newFixture(t)
	f.oldBase(1)
	real := vacuum
	vacuum = func(*sql.DB) error { return errors.New("refusé") }
	t.Cleanup(func() { vacuum = real })
	f.refresh()
	if f.version() == db.IndexVersion {
		t.Fatal("version set after a failed cleanup")
	}
	vacuum = real
	f.refresh()
	if f.version() != db.IndexVersion || strings.Contains(f.fileBytes(), secret[10:]) {
		t.Fatal("retry did not clean")
	}
}

func TestCheckpointBlockedByReaderIsAFailure(t *testing.T) {
	f := newFixture(t)
	f.oldBase(1)
	f.exec("PRAGMA busy_timeout=0")
	reader, err := db.Open(f.p, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	conn, err := reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatal(err)
	}
	f.refresh()
	blocked := f.version()
	conn.ExecContext(t.Context(), "ROLLBACK")
	conn.Close()
	if blocked == db.IndexVersion {
		t.Fatal("version set while a reader holds the WAL")
	}
	f.refresh()
	if f.version() != db.IndexVersion || strings.Contains(f.fileBytes(), secret[10:]) {
		t.Fatal("retry did not clean")
	}
}

func TestRefreshWithoutLockDoesNotRebuild(t *testing.T) {
	f := newFixture(t)
	f.oldBase(1)
	release, _, _ := lock.Writer(f.p.IndexLock(), true)
	f.refresh()
	release()
	if f.version() != 1 {
		t.Fatalf("version %d", f.version())
	}
}

func (f *fixture) allCapturingLog(w *strings.Builder) Stats {
	log.SetOutput(w)
	defer log.SetOutput(os.Stderr)
	return f.all()
}

func TestJournalCwdAndTitle(t *testing.T) {
	f := newFixture(t)
	line := journalLine("1", "user", "bonjour")
	line["cwd"] = "/a"
	line["title"] = "Mon titre"
	f.journal(line)
	f.all()
	if f.one("SELECT title FROM sessions") != "Mon titre" || f.one("SELECT launch_cwd FROM sessions") != "/a" {
		t.Fatal("journal cwd or title not applied")
	}
}

func TestForgottenSessionStaysForgottenAfterShrink(t *testing.T) {
	f := newFixture(t)
	path := f.journal(journalLine("1", "user", "ancien texte nettement plus long que le nouveau"))
	f.all()
	f.exec("INSERT INTO forgotten_sessions(session_id, at) VALUES ('vs', '2026-01-01T00:00:00.000Z')")
	f.exec("DELETE FROM messages")
	f.exec("DELETE FROM sessions")
	short := journalLine("2", "user", "nv")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path, short)
	f.all()
	if f.messages() != 0 || f.count("SELECT count(*) FROM sessions") != 0 {
		t.Fatal("forgotten session came back after the file shrank")
	}
}

func TestWriteLockTakenBeforeReadingOffset(t *testing.T) {
	f := newFixture(t)
	f.transcript("-p", "s1", user("un"))
	other, err := db.Open(f.p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var once sync.Once
	wrote := make(chan error, 1)
	parseClaude = func(raw []byte, is func(string) bool) ([]transcript.Item, error) {
		once.Do(func() {
			go func() {
				_, err := other.Exec("INSERT INTO meta(key, value) VALUES ('k', 'v')")
				wrote <- err
			}()
			select {
			case err := <-wrote:
				wrote <- err
			case <-time.After(100 * time.Millisecond):
			}
		})
		return transcript.ParseClaudeLine(raw, is)
	}
	t.Cleanup(func() { parseClaude = transcript.ParseClaudeLine })
	stats := f.all()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 0 || f.messages() != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func gitRepo(t *testing.T, path, remote string) string {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v : %v\n%s", args, err, out)
		}
	}
	return path
}

func toolOn(path string) obj {
	return base("assistant", obj{"message": obj{"role": "assistant", "content": []any{
		obj{"type": "tool_use", "id": "t", "name": "Read", "input": obj{"file_path": path}}}}})
}

func (f *fixture) repoNames(session string) string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT s.name FROM session_repos r JOIN subjects s ON s.uid = r.subject_uid
		WHERE r.session_id = ? ORDER BY s.name`, session)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	return strings.Join(names, " ")
}

func TestSessionReposFromCwdAndTools(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	a := gitRepo(t, filepath.Join(dir, "a"), "git@example.org:o/alpha.git")
	b := gitRepo(t, filepath.Join(dir, "b"), "git@example.org:o/beta.git")
	plain := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(b, "main.go")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.transcript("-p", "s1", withCwd(user("un"), a), withCwd(toolOn(file), plain), withCwd(user("deux"), "relative/dir"))
	if stats := f.all(); stats.Failed != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if got := f.repoNames("s"); got != "alpha beta" {
		t.Fatalf("got %q", got)
	}
	f.all()
	if f.count("SELECT count(*) FROM session_repos") != 2 {
		t.Fatal("second pass duplicated rows")
	}
}

func TestDeletedWorktreeIsSkipped(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(t.TempDir(), "gone")
	f.transcript("-p", "s1", withCwd(toolOn(filepath.Join(gone, "x.go")), gone))
	stats := f.all()
	if stats.Failed != 0 || stats.Messages != 1 || f.count("SELECT count(*) FROM session_repos") != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestJournalsDirIndexed(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	line := journalLine("1", "user", "bonjour")
	line["cwd"] = a
	f.journal(line)
	if stats := f.all(); stats.Messages != 1 || stats.Failed != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if got := f.repoNames("vs"); got != "alpha" {
		t.Fatalf("got %q", got)
	}
}

func TestGitMissingDoesNotBlockIndexing(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	path := f.transcript("-p", "s1", withCwd(user("un"), a))
	original := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	stats := f.all()
	if stats.Failed != 0 || stats.Messages != 1 || f.messages() != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if f.count("SELECT count(*) FROM session_repos") != 0 || f.count("SELECT count(*) FROM pending_paths") != 1 {
		t.Fatal("candidate not left pending")
	}
	t.Setenv("PATH", original)
	if got := f.all(); got.Failed != 0 || got.Messages != 0 {
		t.Fatalf("retry stats %+v", got)
	}
	if got := f.repoNames("s"); got != "alpha" || f.offset(path) == 0 || f.count("SELECT count(*) FROM pending_paths") != 0 {
		t.Fatalf("retry found %q", got)
	}
}

func TestFailingGitNeverRereadsTheTranscript(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	var parsed atomic.Int64
	real := parseClaude
	parseClaude = func(raw []byte, is func(string) bool) ([]transcript.Item, error) {
		parsed.Add(1)
		return real(raw, is)
	}
	defer func() { parseClaude = real }()
	path := f.transcript("-p", "s1", withCwd(user("un"), a), withCwd(user("deux"), a))
	original := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	f.all()
	if f.count("SELECT count(*) FROM pending_paths") != 1 {
		t.Fatal("nothing pending after the first pass")
	}
	appendLines(t, path, withCwd(user("trois"), a))
	f.all()
	appendLines(t, path, withCwd(user("quatre"), a))
	f.all()
	info, _ := os.Stat(path)
	if parsed.Load() != 4 || f.offset(path) != info.Size() || f.messages() != 4 {
		t.Fatalf("parsed %d, offset %d of %d", parsed.Load(), f.offset(path), info.Size())
	}
	t.Setenv("PATH", original)
	f.all()
	if got := f.repoNames("s"); got != "alpha" || f.count("SELECT count(*) FROM pending_paths") != 0 {
		t.Fatalf("resolved %q", got)
	}
}

func slowGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	budget := gitBudget
	gitBudget = 50 * time.Millisecond
	t.Cleanup(func() { gitBudget = budget })
}

func TestPathTimingOutThreeTimesIsDropped(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	f.transcript("-p", "s1", withCwd(user("un"), a))
	slowGit(t)
	for want := 1; want <= 2; want++ {
		f.all()
		if got := f.count("SELECT attempts FROM pending_paths"); got != want {
			t.Fatalf("pass %d: %d attempts", want, got)
		}
	}
	f.all()
	if f.count("SELECT count(*) FROM pending_paths") != 0 || f.count("SELECT count(*) FROM session_repos") != 0 {
		t.Fatal("path kept after three timeouts")
	}
}

func TestAbsentGitNeverUsesUpAttempts(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	f.transcript("-p", "s1", withCwd(user("un"), a))
	t.Setenv("PATH", t.TempDir())
	for range 3 {
		f.all()
	}
	if f.count("SELECT count(*) FROM pending_paths WHERE attempts = 0") != 1 {
		t.Fatal("hint lost or counted while git was absent")
	}
}

func TestForgottenSessionGetsNoRepoBack(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	f.transcript("-p", "s1", withCwd(user("un"), a))
	original := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	f.all()
	f.exec("INSERT INTO forgotten_sessions(session_id, at) VALUES ('s', '2026-01-01T00:00:00.000Z')")
	t.Setenv("PATH", original)
	f.all()
	if f.count("SELECT count(*) FROM session_repos") != 0 || f.count("SELECT count(*) FROM pending_paths") != 0 {
		t.Fatal("forgotten session got a repo link back")
	}
}

func TestRebuildKeepsStateAndRecomputesRepos(t *testing.T) {
	f := newFixture(t)
	a := gitRepo(t, filepath.Join(t.TempDir(), "a"), "git@example.org:o/alpha.git")
	f.transcript("-p", "s1", withCwd(user("un"), a))
	f.all()
	subject := f.one("SELECT uid FROM subjects")
	if err := db.RebuildIndex(f.db); err != nil {
		t.Fatal(err)
	}
	if f.messages() != 0 || f.count("SELECT count(*) FROM session_repos") != 0 {
		t.Fatal("index not emptied")
	}
	f.all()
	if f.one("SELECT subject_uid FROM session_repos") != subject || f.count("SELECT count(*) FROM subjects") != 1 {
		t.Fatal("repos differ after rebuild")
	}
}
