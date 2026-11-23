package hook

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

const helperEnv = "PAUL_HOOK_TEST_HELPER"

func TestMain(m *testing.M) {
	if event := os.Getenv(helperEnv); event != "" {
		p := paths.Paths{Home: os.Getenv("PAUL_HOME"), Transcripts: os.Getenv("PAUL_TRANSCRIPTS")}
		executable = func() (string, error) { return os.Getenv("PAUL_HOOK_TEST_EXE"), nil }
		os.Exit(Run(event, p, os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

type fixture struct {
	t        *testing.T
	p        paths.Paths
	rec      string
	logs     *bytes.Buffer
	launched atomic.Int32
}

// newFixture replaces the re-executed binary by a script that records its arguments, its pid and its process group.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, p: paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "t")}, rec: filepath.Join(dir, "rec"), logs: &bytes.Buffer{}}
	exe := filepath.Join(dir, "fake-paul")
	script := "#!/bin/sh\necho \"$$ $(ps -o pgid= -p $$ | tr -d ' ') $*\" >> \"$PAUL_HOOK_TEST_REC\"\nif [ -n \"$PAUL_HOOK_TEST_PIPE\" ]; then kill -PIPE $$; echo survived >> \"$PAUL_HOOK_TEST_PIPE\"; fi\nif [ -n \"$PAUL_HOOK_TEST_DONE\" ]; then sleep 1; echo done > \"$PAUL_HOOK_TEST_DONE\"; fi\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The first exec of a fresh script took over 2 s under parallel load on macOS.
	warmup := exec.Command(exe)
	warmup.Env = []string{"PAUL_HOOK_TEST_REC=" + filepath.Join(dir, "warmup")}
	if err := warmup.Run(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAUL_HOOK_TEST_REC", f.rec)
	t.Setenv("PAUL_HOOK_TEST_EXE", exe)
	t.Setenv("PAUL_HOME", f.p.Home)
	t.Setenv("PAUL_TRANSCRIPTS", f.p.Transcripts)
	saved := executable
	executable = func() (string, error) { f.launched.Add(1); return exe, nil }
	savedLog := log.Writer()
	log.SetOutput(f.logs)
	t.Cleanup(func() { executable = saved; log.SetOutput(savedLog); f.waitChildren() })
	return f
}

// A detached child still writing in the temporary folder makes its removal fail.
func (f *fixture) waitChildren() {
	deadline := time.Now().Add(10 * time.Second)
	var raw []byte
	for time.Now().Before(deadline) {
		if raw, _ = os.ReadFile(f.rec); bytes.Count(raw, []byte("\n")) >= int(f.launched.Load()) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		pid, err := strconv.Atoi(strings.SplitN(line, " ", 2)[0])
		for err == nil && time.Now().Before(deadline) && syscall.Kill(pid, 0) == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (f *fixture) db() *sql.DB {
	f.t.Helper()
	d, err := db.Open(f.p, 5*time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { d.Close() })
	return d
}

func (f *fixture) run(event, stdin string) (string, time.Duration) {
	f.t.Helper()
	var out bytes.Buffer
	start := time.Now()
	if code := Run(event, f.p, strings.NewReader(stdin), &out); code != 0 {
		f.t.Fatalf("code %d", code)
	}
	return out.String(), time.Since(start)
}

func (f *fixture) spawned() []string {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, _ := os.ReadFile(f.rec); len(raw) > 0 {
			var lines []string
			for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				lines = append(lines, strings.SplitN(l, " ", 3)[2])
			}
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (f *fixture) neverSpawned() {
	f.t.Helper()
	time.Sleep(300 * time.Millisecond)
	if raw, _ := os.ReadFile(f.rec); len(raw) > 0 {
		f.t.Fatalf("spawned: %s", raw)
	}
}

func (f *fixture) session(id, title, ts string) {
	f.t.Helper()
	for _, q := range []string{
		fmt.Sprintf("INSERT INTO sessions(id, title, started_at, ended_at) VALUES ('%s', '%s', '%s', '%s')", id, title, ts, ts),
		fmt.Sprintf("INSERT INTO messages(key, session_id, file, ts, role, text) VALUES ('k-%s', '%s', 'f', '%s', 'user', 'bonjour')", id, id, ts),
		fmt.Sprintf("INSERT INTO episodes(uid, session_id, start_ts, end_ts, title, summary, created_at, updated_at) VALUES ('e-%s', '%s', '%s', '%s', '%s', 'r', '%s', '%s')", id, id, ts, ts, title, ts, ts),
	} {
		if _, err := f.db().Exec(q); err != nil {
			f.t.Fatal(err)
		}
	}
}

func transcript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func payloadJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestHookReturnsQuickly(t *testing.T) {
	f := newFixture(t)
	for _, event := range []string{"start", "end"} {
		if _, took := f.run(event, "{}"); took > time.Second {
			t.Fatalf("%s took %v", event, took)
		}
	}
}

func TestHookStartWithin1sSilentStdin(t *testing.T) {
	f := newFixture(t)
	r, w := io.Pipe()
	defer w.Close()
	var out bytes.Buffer
	start := time.Now()
	Run("start", f.p, r, &out)
	if took := time.Since(start); took > time.Second || out.Len() != 0 {
		t.Fatalf("took %v, output %q", took, out.String())
	}
	if got := f.spawned(); len(got) != 1 || got[0] != "summarize" {
		t.Fatalf("spawned %q", got)
	}
}

func TestHookNeverWritesOnStdout(t *testing.T) {
	f := newFixture(t)
	if out, _ := f.run("end", payloadJSON(t, map[string]any{"session_id": "s1", "transcript_path": transcript(t)})); out != "" {
		t.Fatalf("stdout %q", out)
	}
}

func TestHookInvalidStdinIsHarmless(t *testing.T) {
	f := newFixture(t)
	for _, in := range []string{"pas du json", "", "[1,2]", `{"session_id": 3}`} {
		if out, _ := f.run("end", in); out != "" {
			t.Fatalf("stdin %q: stdout %q", in, out)
		}
	}
}

func TestHookStartWritesMemoOnStartupAndClear(t *testing.T) {
	for _, source := range []string{"startup", "clear"} {
		f := newFixture(t)
		f.session("s1", "Facturation", "2026-11-10T10:00:00.000Z")
		out, _ := f.run("start", payloadJSON(t, map[string]any{"source": source, "cwd": "/nowhere", "session_id": "new"}))
		var got struct {
			Out struct {
				Name    string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || got.Out.Name != "SessionStart" || !strings.Contains(got.Out.Context, "Facturation") {
			t.Fatalf("%s: %v\n%s", source, err, out)
		}
		if strings.HasSuffix(out, "\n") || !strings.HasPrefix(out, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":`) {
			t.Fatalf("shape %q", out)
		}
	}
}

func TestHookStartWritesNothingOtherwise(t *testing.T) {
	for _, source := range []string{"resume", "compact", "", "other"} {
		f := newFixture(t)
		f.session("s1", "Facturation", "2026-11-10T10:00:00.000Z")
		if out, _ := f.run("start", payloadJSON(t, map[string]any{"source": source, "cwd": "/nowhere"})); out != "" {
			t.Fatalf("source %q: %q", source, out)
		}
	}
}

func TestHookStartWritesNothingWhenMemoIsEmpty(t *testing.T) {
	f := newFixture(t)
	if out, _ := f.run("start", `{"source":"startup","cwd":"/nowhere"}`); out != "" {
		t.Fatalf("%q", out)
	}
}

func TestHookStartSilentWhenDatabaseIsUnusable(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.p.Home, []byte("a file, not a directory"), 0o600); err != nil {
		if err := os.MkdirAll(filepath.Dir(f.p.Home), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.p.Home, []byte("a file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, _ := f.run("start", `{"source":"startup"}`); out != "" {
		t.Fatalf("%q", out)
	}
	if !strings.Contains(f.logs.String(), "hook SessionStart") {
		t.Fatalf("not logged: %q", f.logs.String())
	}
}

func TestHookStartSurvivesAClosedStdout(t *testing.T) {
	f := newFixture(t)
	f.session("s1", "Facturation", "2026-11-10T10:00:00.000Z")
	r, w, _ := os.Pipe()
	r.Close()
	defer w.Close()
	if code := Run("start", f.p, strings.NewReader(`{"source":"startup","cwd":"/nowhere"}`), w); code != 0 {
		t.Fatalf("code %d", code)
	}
}

func TestHookStartSpawnsSummarize(t *testing.T) {
	f := newFixture(t)
	f.run("start", `{"source":"resume"}`)
	if got := f.spawned(); len(got) != 1 || got[0] != "summarize" {
		t.Fatalf("spawned %q", got)
	}
}

func TestHookEndSpawnsSummarizeForTheSession(t *testing.T) {
	f := newFixture(t)
	f.run("end", payloadJSON(t, map[string]any{"session_id": "abc", "transcript_path": transcript(t)}))
	if got := f.spawned(); len(got) != 1 || got[0] != "summarize --session abc" {
		t.Fatalf("spawned %q", got)
	}
}

func TestHookEndIndexesWhenTranscriptIsMissingOrIdUnknown(t *testing.T) {
	for name, in := range map[string]map[string]any{
		"missing transcript": {"session_id": "abc", "transcript_path": "/nowhere/s.jsonl"},
		"no transcript":      {"session_id": "abc"},
		"no session":         {"transcript_path": ""},
	} {
		f := newFixture(t)
		f.run("end", payloadJSON(t, in))
		if got := f.spawned(); len(got) != 1 || got[0] != "index" {
			t.Fatalf("%s: spawned %q", name, got)
		}
	}
}

func TestHookEndWithoutSessionIDOnlyIndexes(t *testing.T) {
	f := newFixture(t)
	f.run("end", payloadJSON(t, map[string]any{"transcript_path": transcript(t)}))
	if got := f.spawned(); len(got) != 1 || got[0] != "index" {
		t.Fatalf("spawned %q", got)
	}
}

func TestHookLogsALaunchFailure(t *testing.T) {
	f := newFixture(t)
	executable = func() (string, error) { return "", errors.New("introuvable") }
	f.run("end", "{}")
	if !strings.Contains(f.logs.String(), "lancement impossible") {
		t.Fatalf("log %q", f.logs.String())
	}
	executable = func() (string, error) { return "/nonexistent/paul", nil }
	f.logs.Reset()
	f.run("end", "{}")
	if !strings.Contains(f.logs.String(), "lancement impossible") {
		t.Fatalf("log %q", f.logs.String())
	}
}

func TestHookUnknownEventIsHarmless(t *testing.T) {
	f := newFixture(t)
	if out, _ := f.run("bogus", "{}"); out != "" {
		t.Fatalf("%q", out)
	}
	f.neverSpawned()
}

func TestHookSpawnIsDetachedAndSurvivesItsParent(t *testing.T) {
	f := newFixture(t)
	done := filepath.Join(t.TempDir(), "done")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=end", "PAUL_HOOK_TEST_DONE="+done)
	cmd.Stdin = strings.NewReader("{}")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(done); err == nil {
		t.Fatal("the child finished before its parent exited")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child did not survive its parent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(f.rec)
	fields := strings.Fields(string(raw))
	if len(fields) < 3 || fields[0] != fields[1] {
		t.Fatalf("child pid and process group differ, so it is not a session leader: %q", raw)
	}
	if pgid, err := strconv.Atoi(fields[1]); err != nil || pgid == os.Getpid() {
		t.Fatalf("pgid %q", fields[1])
	}
}

func bigFixture(t *testing.T, f *fixture) {
	t.Helper()
	tx, err := f.db().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if _, err := tx.Exec("INSERT INTO subjects(uid, kind, name, aliases, created_at, updated_at) VALUES (?, 'subject', ?, '[]', 't', 't')", fmt.Sprint("u", i), fmt.Sprint("sujet", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 100000 {
		if _, err := tx.Exec("INSERT INTO messages(key, session_id, file, ts, role, text) VALUES (?, ?, 'f', '2026-11-01T10:00:00.000Z', 'user', 'bonjour le monde')", i, fmt.Sprint("s", i%1000)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, created_at, updated_at) VALUES ('e', 's1', 't', 't', 't', 't')"); err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		if _, err := tx.Exec("INSERT INTO points(uid, episode_id, kind, subject_uid, text, importance, source_ts, created_at, updated_at) VALUES (?, 1, 'fact', 'u1500', ?, 3, 't', '2026-11-01T10:00:00.000Z', 't')", fmt.Sprint("p", i), fmt.Sprint("fait numero ", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := subjects.RebuildTerms(f.db()); err != nil {
		t.Fatal(err)
	}
}

func TestHookPromptWithin200ms(t *testing.T) {
	f := newFixture(t)
	bigFixture(t, f)
	out, took := f.run("prompt", payloadJSON(t, map[string]any{"session_id": "s1", "prompt": "On reprend le fait numero 7 de sujet1500 ce matin"}))
	t.Logf("prompt: %v", took)
	if took > 200*time.Millisecond || !strings.Contains(out, "sujet1500") {
		t.Fatalf("took %v\n%s", took, out)
	}
}

func TestHookPromptOutputShape(t *testing.T) {
	f := newFixture(t)
	d := f.db()
	if _, err := subjects.Create(d, "Facturation", nil, nil); err != nil {
		t.Fatal(err)
	}
	out, _ := f.run("prompt", payloadJSON(t, map[string]any{"session_id": "s1", "prompt": "Revoyons la facturation"}))
	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || len(got["hookSpecificOutput"]) != 2 ||
		got["hookSpecificOutput"]["hookEventName"] != "UserPromptSubmit" || !strings.Contains(got["hookSpecificOutput"]["additionalContext"], "Sujet « Facturation »") {
		t.Fatalf("%v\n%s", err, out)
	}
	if again, _ := f.run("prompt", payloadJSON(t, map[string]any{"session_id": "s1", "prompt": "Revoyons la facturation"})); again != "" {
		t.Fatalf("injected twice: %s", again)
	}
}

func TestHookPromptSilentOnNothingToSay(t *testing.T) {
	f := newFixture(t)
	for _, in := range []string{"{}", `{"session_id":"s1"}`, `{"prompt":"bonjour"}`, `{"session_id":"s1","prompt":"bonjour"}`} {
		if out, _ := f.run("prompt", in); out != "" {
			t.Fatalf("%s: %q", in, out)
		}
	}
}

func TestHookPromptSilentOnError(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.p.Home), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.p.Home, []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, took := f.run("prompt", `{"session_id":"s1","prompt":"Revoyons la facturation"}`)
	if out != "" || took > 200*time.Millisecond || !strings.Contains(f.logs.String(), "UserPromptSubmit") {
		t.Fatalf("%q after %v, log %q", out, took, f.logs.String())
	}
}

func TestHookPromptRecordsAfterWriting(t *testing.T) {
	f := newFixture(t)
	d := f.db()
	uid, err := subjects.Create(d, "Facturation", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := f.run("prompt", `{"session_id":"s1","prompt":"Revoyons la facturation"}`); out == "" {
		t.Fatal("no injection")
	}
	var n int
	d.QueryRow("SELECT count(*) FROM injected WHERE session_id = 's1' AND subject_uid = ?", uid).Scan(&n)
	if n != 1 {
		t.Fatalf("injected rows: %d", n)
	}
}

func TestEmitRecordsNothingAfterTheBudget(t *testing.T) {
	f := newFixture(t)
	recorded := make(chan struct{}, 1)
	var out bytes.Buffer
	emit(&out, "UserPromptSubmit", 30*time.Millisecond, func() (string, func() error, error) {
		time.Sleep(150 * time.Millisecond)
		return "texte", func() error { recorded <- struct{}{}; return nil }, nil
	})
	time.Sleep(250 * time.Millisecond)
	if out.Len() != 0 || len(recorded) != 0 || !strings.Contains(f.logs.String(), "délai") {
		t.Fatalf("out %q, recorded %d, log %q", out.String(), len(recorded), f.logs.String())
	}
}

func TestHookPromptRecordsNothingWhenTheRecordIsBlocked(t *testing.T) {
	f := newFixture(t)
	d := f.db()
	uid, err := subjects.Create(d, "Facturation", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO injected(session_id, subject_uid) VALUES ('other', 'x')"); err != nil {
		t.Fatal(err)
	}
	_, took := f.run("prompt", `{"session_id":"s1","prompt":"Revoyons la facturation"}`)
	var n int
	tx.Rollback()
	d.QueryRow("SELECT count(*) FROM injected WHERE session_id = 's1' AND subject_uid = ?", uid).Scan(&n)
	if n != 0 || took > 400*time.Millisecond || !strings.Contains(f.logs.String(), "enregistrement") {
		t.Fatalf("rows %d after %v, log %q", n, took, f.logs.String())
	}
}

func repoFixture(t *testing.T, f *fixture) (repo, point string) {
	t.Helper()
	d := f.db()
	repo, err := subjects.EnsureRepo(d, "example/widget", "widget")
	if err != nil {
		t.Fatal(err)
	}
	f.session("s0", "Autre", "2026-11-10T10:00:00.000Z")
	point = "p1"
	for _, q := range []string{
		"INSERT INTO points(uid, episode_id, kind, subject_uid, text, importance, source_ts, created_at, updated_at) VALUES ('p1', 1, 'decision', '" + repo + "', 'Use sqlite for storage', 4, 't', 't', 't')",
		"INSERT INTO repo_cache VALUES ('/work/widget', '" + repo + "', 't')",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return repo, point
}

func TestHookStartRecordsAfterWriting(t *testing.T) {
	f := newFixture(t)
	repo, point := repoFixture(t, f)
	out, _ := f.run("start", `{"source":"startup","cwd":"/work/widget","session_id":"new"}`)
	var injected, used int
	f.db().QueryRow("SELECT count(*) FROM injected WHERE session_id = 'new' AND subject_uid = ?", repo).Scan(&injected)
	f.db().QueryRow("SELECT count(*) FROM point_usage WHERE uid = ?", point).Scan(&used)
	if !strings.Contains(out, "Use sqlite") || injected != 1 || used != 1 {
		t.Fatalf("injected %d, used %d\n%s", injected, used, out)
	}
}

func TestHookStartRecordsNothingWhenTheRecordIsBlocked(t *testing.T) {
	f := newFixture(t)
	repoFixture(t, f)
	d := f.db()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO injected(session_id, subject_uid) VALUES ('other', 'x')"); err != nil {
		t.Fatal(err)
	}
	f.run("start", `{"source":"startup","cwd":"/work/widget","session_id":"new"}`)
	tx.Rollback()
	var n int
	d.QueryRow("SELECT (SELECT count(*) FROM injected WHERE session_id = 'new') + (SELECT count(*) FROM point_usage)").Scan(&n)
	if n != 0 || !strings.Contains(f.logs.String(), "enregistrement") {
		t.Fatalf("rows %d, log %q", n, f.logs.String())
	}
}

func TestHookEndIndexesWhenTranscriptIsADirectory(t *testing.T) {
	f := newFixture(t)
	f.run("end", payloadJSON(t, map[string]any{"session_id": "abc", "transcript_path": t.TempDir()}))
	if got := f.spawned(); len(got) != 1 || got[0] != "index" {
		t.Fatalf("spawned %q", got)
	}
}

func TestHookChildrenKeepTheDefaultSigpipe(t *testing.T) {
	f := newFixture(t)
	f.session("s1", "Facturation", "2026-11-10T10:00:00.000Z")
	marker := filepath.Join(t.TempDir(), "survived")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=start", "PAUL_HOOK_TEST_PIPE="+marker)
	cmd.Stdin = strings.NewReader(`{"source":"startup","cwd":"/nowhere"}`)
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "Facturation") {
		t.Fatalf("%v\n%s", err, out)
	}
	f.spawned()
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the child inherited an ignored SIGPIPE")
	}
}
