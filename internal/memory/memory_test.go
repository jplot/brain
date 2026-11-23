package memory

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/paths"
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
	res, err := f.db.Exec("INSERT INTO points(uid, episode_id, kind, text, importance, source_ts, created_at, updated_at)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?)", fmt.Sprint("p", f.n), episode, o.kind, o.text, o.importance, o.created, o.created, o.created)
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

func TestPointOutsideScopeInvisible(t *testing.T) {
	t.Skip("Task 9")
}

func TestFactSameLabelVisible(t *testing.T) {
	t.Skip("Task 9")
}

func TestFactOtherLabelInvisible(t *testing.T) {
	t.Skip("Task 9")
}

func TestDecisionSameLabelInvisible(t *testing.T) {
	t.Skip("Task 9")
}

func TestAccessOpensPoints(t *testing.T) {
	t.Skip("Task 9")
}

func TestAccessToProDoesNotOpenSecondPro(t *testing.T) {
	t.Skip("Task 9")
}

func TestSummaryHiddenWhenEpisodeLeavesScope(t *testing.T) {
	t.Skip("Task 9")
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

func TestPointWithoutKnownProject(t *testing.T) {
	t.Skip("Task 9")
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
	if err := ForgetSession(f.db, "s"); err != nil {
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
	if err := ForgetSession(f.db, "s9"); err != nil {
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

func TestForgetUnknownSessionLaysTombstone(t *testing.T) {
	f := newFixture(t)
	for range 2 {
		if err := ForgetSession(f.db, "never-seen"); err != nil {
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
	if err := ForgetSession(tx, "s1"); !errors.Is(err, ErrInTransaction) {
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
	if err := ForgetSession(f.db, "s9"); err != nil {
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
	start := time.Now()
	if err := ForgetPoint(f.db, id); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("forget waited %v on the busy checkpoint", time.Since(start))
	}
}
