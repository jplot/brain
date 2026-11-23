package memory

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

const t0 = "2026-01-01T10:00:00.000Z"

type fixture struct {
	t  *testing.T
	p  paths.Paths
	db *sql.DB
	n  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "t")}
	f := &fixture{t: t, p: p, db: f2(t, p)}
	return f
}

func f2(t *testing.T, p paths.Paths) *sql.DB {
	t.Helper()
	d, err := db.Open(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) episode(session, start, title, summary string) int64 {
	f.t.Helper()
	f.n++
	res, err := f.db.Exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, title, summary, created_at, updated_at)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?)", fmt.Sprint("e", f.n), session, start, start, title, summary, start, start)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fixture) defaultEpisode() int64 {
	return f.episode("s1", t0, "Titre", "Résumé")
}

type pt struct {
	kind, text string
	importance int
	created    string
	subject    string
	supersede  string
}

func (f *fixture) point(episode int64, o pt) int64 {
	f.t.Helper()
	o.kind = cmp(o.kind, "fact")
	o.text = cmp(o.text, "un fait")
	o.created = cmp(o.created, t0)
	if o.importance == 0 {
		o.importance = 3
	}
	f.n++
	res, err := f.db.Exec("INSERT INTO points(uid, episode_id, kind, subject_uid, text, importance, source_ts, created_at, updated_at, superseded_by_uid)"+
		" VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''))", fmt.Sprint("p", f.n), episode, o.kind, o.subject, o.text, o.importance, o.created, o.created, o.created, o.supersede)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func cmp(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (f *fixture) subject(name string, parent *string) string {
	f.t.Helper()
	uid, err := subjects.Create(f.db, name, nil, parent)
	if err != nil {
		f.t.Fatal(err)
	}
	return uid
}

func (f *fixture) visible(flt PointFilter) []Point {
	f.t.Helper()
	got, err := VisiblePoints(f.db, flt)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func texts(ps []Point) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Text)
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func sorted(s []string) []string {
	slices.Sort(s)
	return s
}

func (f *fixture) ftsCount(term string) int {
	return f.count("SELECT count(*) FROM points_fts WHERE points_fts MATCH ?", term)
}

func (f *fixture) rawBytes() []byte {
	f.t.Helper()
	data, err := os.ReadFile(f.p.DB())
	if err != nil {
		f.t.Fatal(err)
	}
	wal, err := os.ReadFile(f.p.DB() + "-wal")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	return append(data, wal...)
}

func (f *fixture) assertAbsentFromFiles(marker string) {
	f.t.Helper()
	f.exec("VACUUM")
	f.exec("PRAGMA wal_checkpoint(TRUNCATE)")
	f.db.Close()
	if bytes.Contains(f.rawBytes(), []byte(marker)) {
		f.t.Fatalf("%s still in the database files", marker)
	}
}

func (f *fixture) message(session, text string) {
	f.exec("INSERT INTO messages(key, session_id, file, ts, role, text) VALUES (?, ?, 'f', ?, 'user', ?)",
		fmt.Sprint("k", session, text), session, t0, text)
}

func TestRebuildKeepsMemories(t *testing.T) {
	f := newFixture(t)
	f.point(f.defaultEpisode(), pt{})
	f.exec("INSERT INTO forgotten_sessions(session_id, at) VALUES ('x', 't')")
	f.exec("INSERT INTO summary_failures(session_id, attempts, last_at) VALUES ('y', 1, 't')")
	if err := db.RebuildIndex(f.db); err != nil {
		t.Fatal(err)
	}
	if n := len(f.visible(PointFilter{})); n != 1 {
		t.Fatalf("points: %d", n)
	}
	eps, err := VisibleEpisodes(f.db, "s1")
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes: %v %v", eps, err)
	}
	if f.count("SELECT count(*) FROM forgotten_sessions") != 1 || f.count("SELECT count(*) FROM summary_failures") != 1 || f.ftsCount("fait") != 1 {
		t.Fatal("rebuild lost state")
	}
}

func TestEpisodesCarryTitleAndSummary(t *testing.T) {
	f := newFixture(t)
	f.episode("s1", "2026-01-01T10:00:00.000Z", "premier", "secret")
	f.episode("s1", "2026-01-01T11:00:00.000Z", "second", "ok")
	got, err := VisibleEpisodes(f.db, "s1")
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if *got[0].Title != "premier" || *got[0].Summary != "secret" || *got[1].Title != "second" || *got[1].Summary != "ok" {
		t.Fatalf("%+v", got)
	}
}

func TestCorrectionMarksPoint(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{})
	text, imp := "nouveau texte", 5
	if err := CorrectPoint(f.db, id, &text, &imp); err != nil {
		t.Fatal(err)
	}
	got := f.visible(PointFilter{})[0]
	if got.Text != text || got.Importance != 5 || !got.Corrected {
		t.Fatalf("%+v", got)
	}
	if f.ftsCount("nouveau") != 1 || f.ftsCount("un") != 0 {
		t.Fatal("index not updated")
	}
}

func TestCorrectionMasksSecrets(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{})
	text := "clé sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	if err := CorrectPoint(f.db, id, &text, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.visible(PointFilter{})[0].Text, "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("secret kept")
	}
}

func TestCorrectionInvalid(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{})
	if err := CorrectPoint(f.db, id, nil, nil); !errors.Is(err, ErrNothingToCorrect) {
		t.Fatalf("%v", err)
	}
	x := "x"
	for _, bad := range []int{0, 6} {
		if err := CorrectPoint(f.db, id, &x, &bad); err == nil {
			t.Fatalf("importance %d accepted", bad)
		}
	}
	got := f.visible(PointFilter{})[0]
	if got.Text != "un fait" || got.Corrected {
		t.Fatalf("%+v", got)
	}
}

func TestSearchPointsWithoutAccents(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	f.point(e, pt{text: "Décision sur la mémoire"})
	f.point(e, pt{text: "autre chose"})
	equal(t, texts(f.visible(PointFilter{Text: "memoire decision"})), []string{"Décision sur la mémoire"})
	if n := len(f.visible(PointFilter{Text: "   "})); n != 2 {
		t.Fatalf("blank text filtered: %d", n)
	}
	equal(t, texts(f.visible(PointFilter{Text: `"(*)!`})), []string{})
}

func TestFiltersAndOrder(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	f.point(e, pt{kind: "pending", text: "a", importance: 2, created: "2026-01-01T10:00:00.000Z"})
	b := f.point(e, pt{kind: "pending", text: "b", importance: 5, created: "2026-01-01T09:00:00.000Z"})
	f.point(e, pt{kind: "pending", text: "c", importance: 5, created: "2026-01-01T11:00:00.000Z"})
	f.point(e, pt{kind: "decision", text: "d", importance: 1})
	equal(t, texts(f.visible(PointFilter{})), []string{"c", "b", "a", "d"})
	equal(t, texts(f.visible(PointFilter{Kind: "pending", Limit: 2})), []string{"c", "b"})
	equal(t, texts(f.visible(PointFilter{PointID: b})), []string{"b"})
	equal(t, texts(f.visible(PointFilter{EpisodeID: e + 1})), []string{})
	if err := ClosePoint(f.db, b); err != nil {
		t.Fatal(err)
	}
	equal(t, texts(f.visible(PointFilter{OpenOnly: true})), []string{"c", "a"})
}

func TestSinceUntilAreInclusiveDays(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	for _, ts := range []string{"2025-12-31T00:00:00.000Z", "2026-01-15T12:00:00.000Z", "2026-02-20T12:00:00.000Z"} {
		f.point(e, pt{text: ts[:10], created: ts})
	}
	equal(t, texts(f.visible(PointFilter{Since: "2026-01-15", Until: "2026-01-15"})), []string{"2026-01-15"})
	if _, err := VisiblePoints(f.db, PointFilter{Since: "hier"}); err == nil {
		t.Fatal("bad date accepted")
	}
}

func TestForgetPointStalesSummary(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	id := f.point(e, pt{text: "zebre"})
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if len(f.visible(PointFilter{})) != 0 || f.ftsCount("zebre") != 0 {
		t.Fatal("point survives")
	}
	eps, _ := VisibleEpisodes(f.db, "s1")
	if !eps[0].SummaryStale {
		t.Fatal("summary not stale")
	}
}

func TestForgetPointLeavesTrace(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{})
	var uid string
	if err := f.db.QueryRow("SELECT uid FROM points WHERE id = ?", id).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM forgotten_points WHERE uid = ?", uid) != 1 {
		t.Fatal("no forgotten_points row")
	}
}

func TestWritesBumpUpdatedAt(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{})
	text := "corrigé"
	if err := CorrectPoint(f.db, id, &text, nil); err != nil {
		t.Fatal(err)
	}
	first := f.visible(PointFilter{})[0]
	if first.UpdatedAt == t0 {
		t.Fatal("correction kept updated_at")
	}
	f.exec("UPDATE points SET updated_at = ? WHERE id = ?", t0, id)
	if err := ClosePoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if got := f.visible(PointFilter{})[0]; got.UpdatedAt == t0 || got.ClosedAt == nil {
		t.Fatalf("close kept updated_at: %+v", got)
	}
}

func TestForgetSessionSurvivesRebuild(t *testing.T) {
	f := newFixture(t)
	f.message("s", "bonjour")
	f.exec("INSERT INTO sessions(id) VALUES ('s')")
	if err := ForgetSession(f.db, f.p, "s"); err != nil {
		t.Fatal(err)
	}
	if err := db.RebuildIndex(f.db); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM forgotten_sessions WHERE session_id = 's'") != 1 || f.count("SELECT count(*) FROM messages") != 0 {
		t.Fatal("tombstone lost or messages back")
	}
}

func TestForgetSessionAbsentFromFile(t *testing.T) {
	f := newFixture(t)
	marker := "zephyrmarqueur"
	f.message("s9", "message "+marker)
	f.exec("INSERT INTO sessions(id) VALUES ('s9')")
	e := f.episode("s9", t0, "titre "+marker, "resume "+marker)
	f.point(e, pt{text: "point " + marker})
	f.exec("INSERT INTO summary_failures(session_id, attempts, last_at) VALUES ('s9', 1, 't')")
	if err := ForgetSession(f.db, f.p, "s9"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages", "sessions", "episodes", "points", "summary_failures"} {
		if n := f.count("SELECT count(*) FROM " + table); n != 0 {
			t.Fatalf("%s: %d rows", table, n)
		}
	}
	if f.ftsCount(marker) != 0 {
		t.Fatal("points_fts keeps the marker")
	}
	f.assertAbsentFromFiles(marker)
}

func TestForgetSessionRewritesTheArchive(t *testing.T) {
	f := newFixture(t)
	marker := "zephyrarchive"
	line := func(id, session, text string) string {
		return `{"v":1,"id":"` + id + `","session":"` + session + `","ts":"2026-01-01T10:00:00Z","role":"user","text":"` + text + `"}` + "\n"
	}
	os.MkdirAll(f.p.Journals(), 0o700)
	os.WriteFile(f.p.Archive(), []byte(line("a1", "s-garde", "gardé")+line("a2", "s-oubli", "message "+marker)+line("a3", "s-garde", "encore")), 0o600)
	vocal := filepath.Join(f.p.Journals(), "vocal.jsonl")
	os.WriteFile(vocal, []byte(line("v1", "s-oubli", "dit à voix haute")), 0o600)
	index.All(f.db, f.p)
	if n := f.count("SELECT count(*) FROM messages WHERE session_id = 's-oubli'"); n != 2 {
		t.Fatalf("%d messages before", n)
	}
	if err := ForgetSession(f.db, f.p, "s-oubli"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.p.Archive()); string(b) != line("a1", "s-garde", "gardé")+line("a3", "s-garde", "encore") {
		t.Fatalf("archive %q", b)
	}
	if fi, _ := os.Stat(f.p.Archive()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(vocal); string(b) != line("v1", "s-oubli", "dit à voix haute") {
		t.Fatal("vocal.jsonl touched")
	}
	if entries, _ := os.ReadDir(f.p.Journals()); len(entries) != 2 {
		t.Fatalf("%d entries in journals", len(entries))
	}
	if n := f.count("SELECT count(*) FROM files WHERE path = ?", f.p.Archive()); n != 0 {
		t.Fatal("archive offset kept")
	}
	index.All(f.db, f.p)
	if n := f.count("SELECT count(*) FROM messages WHERE session_id = 's-oubli'"); n != 0 {
		t.Fatalf("%d forgotten messages back", n)
	}
	if n := f.count("SELECT count(*) FROM messages WHERE session_id = 's-garde'"); n != 2 {
		t.Fatalf("%d kept messages", n)
	}
	f.assertAbsentFromFiles(marker)
}

func TestForgetUnknownSessionLaysTombstone(t *testing.T) {
	f := newFixture(t)
	for range 2 {
		if err := ForgetSession(f.db, f.p, "never-seen"); err != nil {
			t.Fatal(err)
		}
	}
	if f.count("SELECT count(*) FROM forgotten_sessions WHERE session_id = 'never-seen'") != 1 {
		t.Fatal("tombstone missing or duplicated")
	}
}

func TestUnknownPointReported(t *testing.T) {
	f := newFixture(t)
	two := 2
	for name, err := range map[string]error{
		"close": ClosePoint(f.db, 99), "forget": ForgetPoint(f.db, 99), "correct": CorrectPoint(f.db, 99, nil, &two),
	} {
		var nf *ErrPointNotFound
		if !errors.As(err, &nf) || nf.ID != 99 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestWritesCommit(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{})
	if err := ClosePoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	other := f2(t, f.p)
	var closed bool
	if err := other.QueryRow("SELECT closed_at IS NOT NULL FROM points").Scan(&closed); err != nil || !closed {
		t.Fatalf("%v %v", closed, err)
	}
}

func TestForgetPointAbsentFromFile(t *testing.T) {
	f := newFixture(t)
	marker := "zephyrpoint"
	id := f.point(f.defaultEpisode(), pt{text: "point " + marker})
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	f.assertAbsentFromFiles(marker)
}

func TestCorrectionAbsentFromFile(t *testing.T) {
	f := newFixture(t)
	marker := "zephyrcorrige"
	id := f.point(f.defaultEpisode(), pt{text: "point " + marker})
	text := "texte propre"
	if err := CorrectPoint(f.db, id, &text, nil); err != nil {
		t.Fatal(err)
	}
	f.assertAbsentFromFiles(marker)
}

func TestForgetSessionRefusesOpenTransaction(t *testing.T) {
	f := newFixture(t)
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO meta(key, value) VALUES ('k', 'v')"); err != nil {
		t.Fatal(err)
	}
	if err := ForgetSession(tx, f.p, "s1"); !errors.Is(err, ErrInTransaction) {
		t.Fatalf("%v", err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM forgotten_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestFailedCorrectionChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.db.SetMaxOpenConns(1)
	id := f.point(f.defaultEpisode(), pt{})
	f.exec("CREATE TEMP TRIGGER boom BEFORE UPDATE OF importance ON points BEGIN SELECT RAISE(ABORT, 'boom'); END")
	text, imp := "nouveau", 4
	if err := CorrectPoint(f.db, id, &text, &imp); err == nil {
		t.Fatal("expected failure")
	}
	got := f.visible(PointFilter{})[0]
	if got.Text != "un fait" || got.Importance != 3 || got.Corrected {
		t.Fatalf("%+v", got)
	}
}

func TestForgetSessionReachesFileWithAnotherConnectionOpen(t *testing.T) {
	f := newFixture(t)
	marker := "zephyrautreconnexion"
	f.message("s9", "message "+marker)
	e := f.episode("s9", t0, "titre "+marker, "resume "+marker)
	f.point(e, pt{text: "point " + marker})
	f2(t, f.p)
	if err := ForgetSession(f.db, f.p, "s9"); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.rawBytes(), []byte(marker)) {
		t.Fatal("marker still in files")
	}
}

func TestForgetPointReachesFileWithAnotherConnectionOpen(t *testing.T) {
	f := newFixture(t)
	marker := "zephyrpointautre"
	id := f.point(f.defaultEpisode(), pt{text: "point " + marker})
	f2(t, f.p)
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.rawBytes(), []byte(marker)) {
		t.Fatal("marker still in files")
	}
}

func TestForgetDoesNotBlockOnBusyCheckpoint(t *testing.T) {
	f := newFixture(t)
	id := f.point(f.defaultEpisode(), pt{text: "point occupe"})
	reader := f2(t, f.p)
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM points").Scan(&n); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	start := time.Now()
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("forget waited %v on the busy checkpoint", time.Since(start))
	}
	if !strings.Contains(logged.String(), "point de contrôle") {
		t.Fatalf("log %q", logged.String())
	}
}

func TestForgetSessionPurgesRepoHints(t *testing.T) {
	f := newFixture(t)
	f.exec("INSERT INTO pending_paths VALUES ('s', '/d', 0, 't'), ('o', '/d', 0, 't')")
	f.exec("INSERT INTO session_repos VALUES ('s', 'u1'), ('o', 'u1')")
	f.exec("INSERT INTO injected VALUES ('s', 'u1'), ('o', 'u1')")
	if err := ForgetSession(f.db, f.p, "s"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"pending_paths", "session_repos", "injected"} {
		if f.count("SELECT count(*) FROM "+table+" WHERE session_id = 's'") != 0 || f.count("SELECT count(*) FROM "+table+" WHERE session_id = 'o'") != 1 {
			t.Fatalf("%s not purged for the forgotten session only", table)
		}
	}
}

func at(t *testing.T, hoursAfter float64) time.Time {
	t.Helper()
	base, err := time.Parse(db.UTCFormat, t0)
	if err != nil {
		t.Fatal(err)
	}
	return base.Add(time.Duration(hoursAfter * float64(time.Hour)))
}

func stamp(t *testing.T, hoursAfter float64) *string {
	s := db.Stamp(at(t, hoursAfter))
	return &s
}

func TestScoreCombinesThree(t *testing.T) {
	now := at(t, 200)
	points := []Point{
		{ID: 1, Importance: 1, CreatedAt: t0, LastUsedAt: stamp(t, 200)},
		{ID: 2, Importance: 5, CreatedAt: db.Stamp(at(t, 100))},
		{ID: 3, Importance: 3, CreatedAt: t0},
	}
	relevance := map[int64]float64{1: 0.5, 2: 1}
	got := Score(points, relevance, now)
	want := []struct {
		id    int64
		score float64
	}{
		{2, 2.6057704364907277},
		{1, 1.5},
		{3, 0.8669578217261671},
	}
	for i, w := range want {
		if got[i].ID != w.id || math.Abs(got[i].Score-w.score) > 1e-9 {
			t.Errorf("rank %d: point %d score %.10f, want point %d score %.10f", i, got[i].ID, got[i].Score, w.id, w.score)
		}
	}
}

func TestRecencyUsesLastUse(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	month := db.Stamp(time.Now().Add(-30 * 24 * time.Hour))
	old := f.point(e, pt{text: "ancien", created: month})
	f.point(e, pt{text: "récent", created: db.Stamp(time.Now().Add(-24 * time.Hour))})
	if err := MarkUsed(f.db, []string{"p3"}, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	equal(t, texts(f.visible(PointFilter{Order: "score"})), []string{"récent", "ancien"})
	if err := MarkUsed(f.db, []string{"p2"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := f.visible(PointFilter{Order: "score"})
	equal(t, texts(got), []string{"ancien", "récent"})
	if got[0].ID != old || got[0].LastUsedAt == nil {
		t.Fatalf("%+v", got[0])
	}
}

func TestRelevanceNormalized(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	one := f.point(e, pt{text: "pomme pomme pomme"})
	two := f.point(e, pt{text: "pomme et poire dans un long texte sans rapport avec le reste"})
	none := f.point(e, pt{text: "citron"})
	ids := []int64{one, two, none}
	got, err := Relevance(f.db, []string{"pomme"}, ids)
	if err != nil {
		t.Fatal(err)
	}
	var raw1, raw2 float64
	f.db.QueryRow("SELECT -bm25(points_fts) FROM points_fts WHERE points_fts MATCH 'pomme' AND rowid = ?", one).Scan(&raw1)
	f.db.QueryRow("SELECT -bm25(points_fts) FROM points_fts WHERE points_fts MATCH 'pomme' AND rowid = ?", two).Scan(&raw2)
	if raw1 <= raw2 || raw2 <= 0 {
		t.Fatalf("raw bm25 %v %v", raw1, raw2)
	}
	want := map[int64]float64{one: 1, two: raw2 / raw1, none: 0}
	for id, w := range want {
		if math.Abs(got[id]-w) > 1e-9 {
			t.Errorf("point %d: %v, want %v", id, got[id], w)
		}
	}
	for _, c := range []struct {
		terms []string
		ids   []int64
		want  map[int64]float64
	}{
		{nil, ids, map[int64]float64{one: 0, two: 0, none: 0}},
		{[]string{"pomme"}, []int64{none}, map[int64]float64{none: 0}},
		{[]string{"pomme"}, []int64{one}, map[int64]float64{one: 1}},
		{[]string{"pomme", "citron"}, []int64{two, none}, map[int64]float64{}},
	} {
		got, err := Relevance(f.db, c.terms, c.ids)
		if err != nil {
			t.Fatal(err)
		}
		for id, w := range c.want {
			if got[id] != w {
				t.Errorf("%v %v: point %d is %v, want %v", c.terms, c.ids, id, got[id], w)
			}
		}
		if len(got) != len(c.ids) {
			t.Errorf("%v: %d entries for %d candidates", c.terms, len(got), len(c.ids))
		}
	}
}

func TestScoreOrderUsesTermsOfText(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	f.point(e, pt{text: "pomme poire", importance: 3})
	f.point(e, pt{text: "poire", importance: 3})
	equal(t, texts(f.visible(PointFilter{Order: "score", Text: "pomme"})), []string{"pomme poire"})
	f.point(e, pt{text: "citron", importance: 5})
	equal(t, texts(f.visible(PointFilter{Order: "score", Limit: 1})), []string{"citron"})
}

func TestMarkUsedLocalOnly(t *testing.T) {
	f := newFixture(t)
	f.point(f.defaultEpisode(), pt{})
	if err := MarkUsed(f.db, []string{"p2", "p2"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM point_usage"); n != 1 {
		t.Fatalf("usage rows: %d", n)
	}
	if err := db.RebuildIndex(f.db); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM point_usage"); n != 0 {
		t.Fatalf("usage survives rebuild: %d", n)
	}
	if n := len(f.visible(PointFilter{})); n != 1 {
		t.Fatalf("points: %d", n)
	}
}

func TestForgetDropsUsage(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	id := f.point(e, pt{})
	f.point(e, pt{})
	if err := MarkUsed(f.db, []string{"p2", "p3"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM point_usage"); n != 1 {
		t.Fatalf("after forgetting a point: %d", n)
	}
	if err := ForgetSession(f.db, f.p, "s1"); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM point_usage"); n != 0 {
		t.Fatalf("after forgetting the session: %d", n)
	}
}

func TestSupersededHiddenUnlessHistory(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	f.point(e, pt{text: "ancien", supersede: "p2"})
	f.point(e, pt{text: "nouveau"})
	equal(t, texts(f.visible(PointFilter{})), []string{"nouveau"})
	equal(t, sorted(texts(f.visible(PointFilter{History: true}))), []string{"ancien", "nouveau"})
}

func TestPointsBySubject(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	maison := f.subject("Maison", nil)
	reseau := f.subject("Réseau", &maison)
	outil := f.subject("Outil", nil)
	f.point(e, pt{text: "toit", subject: maison})
	f.point(e, pt{text: "routeur", subject: reseau})
	f.point(e, pt{text: "marteau", subject: outil})
	equal(t, sorted(texts(f.visible(PointFilter{Subject: "maison"}))), []string{"routeur", "toit"})
	equal(t, texts(f.visible(PointFilter{Subject: "RESEAU"})), []string{"routeur"})
	equal(t, texts(f.visible(PointFilter{Subject: outil})), []string{"marteau"})
	var unknown *subjects.ErrUnknownSubject
	if _, err := VisiblePoints(f.db, PointFilter{Subject: "mason"}); !errors.As(err, &unknown) || unknown.Term != "mason" {
		t.Fatalf("err %v", err)
	}
	got := f.visible(PointFilter{Subject: "outil"})[0]
	if got.SubjectName == nil || *got.SubjectName != "Outil" || got.SubjectUID == nil || *got.SubjectUID != outil {
		t.Fatalf("%+v", got)
	}
}

func TestPointsAmbiguousSubject(t *testing.T) {
	f := newFixture(t)
	f.subject("Lune", nil)
	if _, err := subjects.Create(f.db, "Satellite", []string{"lune"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := VisiblePoints(f.db, PointFilter{Subject: "lune"})
	var amb *subjects.ErrAmbiguousSubject
	if !errors.As(err, &amb) || len(amb.Candidates) != 2 {
		t.Fatalf("err %v", err)
	}
}

func TestPointWithoutSubjectListed(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	outil := f.subject("Outil", nil)
	f.point(e, pt{text: "orphelin"})
	f.point(e, pt{text: "rangé", subject: outil})
	equal(t, sorted(texts(f.visible(PointFilter{}))), []string{"orphelin", "rangé"})
	equal(t, texts(f.visible(PointFilter{Subject: "outil"})), []string{"rangé"})
	if got := f.visible(PointFilter{Text: "orphelin"})[0]; got.SubjectUID != nil || got.SubjectName != nil {
		t.Fatalf("%+v", got)
	}
}

func TestEpisodesCarrySubjectNames(t *testing.T) {
	f := newFixture(t)
	f.defaultEpisode()
	f.episode("s2", t0, "autre", "")
	f.exec("INSERT INTO episode_subjects(episode_uid, subject_uid) VALUES ('e1', ?), ('e1', ?)",
		f.subject("Zèbre", nil), f.subject("Abeille", nil))
	got, err := VisibleEpisodes(f.db, "s1")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got[0].Subjects, []string{"Abeille", "Zèbre"})
	other, _ := VisibleEpisodes(f.db, "s2")
	if len(other[0].Subjects) != 0 {
		t.Fatalf("%v", other[0].Subjects)
	}
}

func TestSynthesisReadersSkipStale(t *testing.T) {
	f := newFixture(t)
	parent := f.subject("Parent", nil)
	child := f.subject("Enfant", &parent)
	for _, s := range []struct {
		subject string
		stale   int
	}{{parent, 0}, {child, 1}} {
		f.exec("INSERT INTO syntheses(subject_uid, uid, text, stale, created_at, updated_at) VALUES (?, ?, 'texte', ?, 'a', 'a')",
			s.subject, s.subject, s.stale)
	}
	if text, err := Synthesis(f.db, parent); err != nil || text != "texte" {
		t.Fatal(text, err)
	}
	if text, _ := Synthesis(f.db, child); text != "" {
		t.Fatal("stale read", text)
	}
	if got, _ := ChildSyntheses(f.db, parent, 10); len(got) != 0 {
		t.Fatal(got)
	}
	f.exec("UPDATE syntheses SET stale = 0")
	if got, _ := ChildSyntheses(f.db, parent, 10); len(got) != 1 || got[0].Name != "Enfant" {
		t.Fatal(got)
	}
}

func TestForgetSessionDropsTheGraphItCreated(t *testing.T) {
	f := newFixture(t)
	gone := f.episode("s-oubli", t0, "t", "r")
	kept := f.episode("s-reste", "2026-01-02T10:00:00.000Z", "t", "r")
	invented, err := subjects.Create(f.db, "Personne Inventée", []string{"avocat"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	shared, family, office := f.subject("Partagé", nil), f.subject("Famille", nil), f.subject("Cabinet", nil)
	origin := subjects.Origin{EpisodeUID: new("e1")}
	for _, err := range []error{subjects.SetParent(f.db, invented, office, subjects.Origin{EpisodeUID: new("e2")}),
		subjects.SetParent(f.db, shared, family, origin), subjects.Link(f.db, invented, "about", shared, origin)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.exec("INSERT INTO episode_subjects VALUES ('e1', ?), ('e1', ?), ('e1', ?), ('e2', ?)", invented, shared, office, family)
	f.point(gone, pt{subject: invented})
	f.point(gone, pt{subject: shared})
	f.point(kept, pt{subject: shared})
	f.point(kept, pt{subject: family})
	if err := ForgetSession(f.db, f.p, "s-oubli"); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{invented, office} {
		if _, err := subjects.Get(f.db, uid); !errors.Is(err, subjects.ErrNotFound) {
			t.Fatal("subject of the forgotten session kept", err)
		}
	}
	if found, _ := subjects.ByTerms(f.db, []string{"avocat", "personne inventee"}); len(found) != 0 {
		t.Fatal("alias still injects", found)
	}
	if n := f.count("SELECT count(*) FROM links") + f.count("SELECT count(*) FROM episode_subjects WHERE episode_uid = 'e1'"); n != 0 {
		t.Fatalf("%d graph rows of the forgotten episode", n)
	}
	got, err := subjects.Get(f.db, shared)
	if err != nil || got.ParentUID != nil {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := subjects.Get(f.db, family); err != nil {
		t.Fatal(err)
	}
}

func TestForgetPointRestoresWhatItReplaced(t *testing.T) {
	f := newFixture(t)
	e := f.defaultEpisode()
	f.point(e, pt{text: "ancien", supersede: "p3"})
	id := f.point(e, pt{text: "nouveau"})
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	equal(t, texts(f.visible(PointFilter{})), []string{"ancien"})
}

func TestForgetSessionRestoresWhatItReplaced(t *testing.T) {
	f := newFixture(t)
	f.point(f.defaultEpisode(), pt{text: "ancien", supersede: "p4"})
	f.point(f.episode("s-oubli", t0, "t", "r"), pt{text: "nouveau"})
	if err := ForgetSession(f.db, f.p, "s-oubli"); err != nil {
		t.Fatal(err)
	}
	equal(t, texts(f.visible(PointFilter{})), []string{"ancien"})
}
