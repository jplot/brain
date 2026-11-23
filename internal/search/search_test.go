package search

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/projects"
)

var bg = background

type world struct {
	t     *testing.T
	p     paths.Paths
	cfg   config.Config
	db    *sql.DB
	root  string
	files int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "transcripts")}
	d, err := db.Open(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	root := filepath.Join(dir, "code")
	return &world{t: t, p: p, cfg: config.Config{Roots: []string{root}}, db: d, root: root}
}

func (w *world) dir(name string) string { return filepath.Join(w.root, name) }

func entry(session, cwd, text, ts string) map[string]any {
	return map[string]any{"type": "user", "uuid": fmt.Sprintf("u-%s-%s", session, ts+text), "sessionId": session,
		"timestamp": ts, "cwd": cwd, "origin": map[string]any{"kind": "human"},
		"message": map[string]any{"role": "user", "content": text}}
}

func aiTitle(session, text string) map[string]any {
	return map[string]any{"type": "ai-title", "sessionId": session, "aiTitle": text}
}

func writeTranscript(t *testing.T, p paths.Paths, folder, session string, lines ...map[string]any) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	path := filepath.Join(p.Transcripts, folder, session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

const defaultTS = "2026-10-20T10:00:00.000Z"

func (w *world) add(cwd, session, text, ts string, extra ...map[string]any) {
	w.t.Helper()
	w.files++
	writeTranscript(w.t, w.p, fmt.Sprintf("f%d", w.files), session, append([]map[string]any{entry(session, cwd, text, ts)}, extra...)...)
	if stats := index.All(w.db, w.p, w.cfg); stats.Failed != 0 {
		w.t.Fatalf("index: %+v", stats)
	}
}

func (w *world) scope(cwd string) projects.Scope {
	w.t.Helper()
	s, err := projects.ScopeFor(bg, w.db, w.cfg, cwd)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) indexed() {
	for _, n := range []string{"A", "B", "C"} {
		w.add(w.dir(n), "sess-"+n, "mot de "+n, defaultTS)
	}
}

func (w *world) label(id, label string) {
	w.t.Helper()
	if err := projects.SetLabel(w.db, id, label); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) search(text string, s projects.Scope, f Filter) []Hit {
	w.t.Helper()
	hits, err := Search(w.db, text, s, f)
	if err != nil {
		w.t.Fatal(err)
	}
	return hits
}

func (w *world) list(s projects.Scope, f ListFilter) []SessionRow {
	w.t.Helper()
	rows, err := ListSessions(w.db, s, f)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func (w *world) day(d time.Time, s projects.Scope) []SessionRow {
	w.t.Helper()
	rows, err := DayView(w.db, d, s)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func (w *world) view(ref string, s projects.Scope) (*SessionRow, []Message) {
	w.t.Helper()
	row, msgs, err := SessionView(w.db, ref, s)
	if err != nil {
		w.t.Fatal(err)
	}
	return row, msgs
}

func projectsOf(hits []Hit) []string {
	set := map[string]bool{}
	for _, h := range hits {
		set[h.Project] = true
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func ids(rows []SessionRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func texts(ms []Message) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Text)
	}
	return out
}

func same(t *testing.T, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func day(t *testing.T, loc *time.Location, y int, m time.Month, d int) time.Time {
	t.Helper()
	return time.Date(y, m, d, 12, 0, 0, 0, loc)
}

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	return loc
}

func TestStaysInScope(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	same(t, projectsOf(w.search("mot", w.scope(w.dir("A")), Filter{})), []string{"dir:A"})
}

func TestAccessOpensOneWayWithoutPropagation(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, g := range [][2]string{{"dir:A", "dir:B"}, {"dir:B", "dir:C"}} {
		if err := projects.Grant(w.db, g[0], g[1], false); err != nil {
			t.Fatal(err)
		}
	}
	same(t, projectsOf(w.search("mot", w.scope(w.dir("A")), Filter{})), []string{"dir:A", "dir:B"})
	same(t, projectsOf(w.search("mot", w.scope(w.dir("C")), Filter{})), []string{"dir:C"})
}

func TestRevokedAccessCutsAtOnce(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	if err := projects.Grant(w.db, "dir:A", "dir:B", false); err != nil {
		t.Fatal(err)
	}
	same(t, projectsOf(w.search("mot", w.scope(w.dir("A")), Filter{})), []string{"dir:A", "dir:B"})
	if err := projects.Revoke(w.db, "dir:A", "dir:B", false); err != nil {
		t.Fatal(err)
	}
	same(t, projectsOf(w.search("mot", w.scope(w.dir("A")), Filter{})), []string{"dir:A"})
}

func TestRootsSeeEverything(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	s := w.scope(w.root)
	if s.Projects != nil {
		t.Fatalf("roots scope should be unrestricted, got %v", s.Projects)
	}
	same(t, projectsOf(w.search("mot", s, Filter{})), []string{"dir:A", "dir:B", "dir:C"})
}

func TestEverythingScope(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	same(t, projectsOf(w.search("mot", projects.Everything, Filter{})), []string{"dir:A", "dir:B", "dir:C"})
}

func TestLabelFilter(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.label("dir:B", "work")
	hits := w.search("mot", projects.Everything, Filter{Label: "work"})
	same(t, projectsOf(hits), []string{"dir:B"})
	same(t, hits[0].Label, "work")
}

func TestFilterDoesNotExceedScope(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	s := w.scope(w.dir("A"))
	same(t, len(w.search("mot", s, Filter{Project: "dir:B"})), 0)
	w.label("dir:B", "work")
	same(t, len(w.search("mot", s, Filter{Label: "work"})), 0)
}

func TestRootsMessageInvisibleElsewhere(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.add(w.root, "sess-root", "secret du general", defaultTS)
	same(t, len(w.search("secret", w.scope(w.dir("A")), Filter{})), 0)
	same(t, len(w.search("secret", w.scope(w.root), Filter{})), 1)
}

func TestMessageWithoutProjectRowStaysVisible(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	if _, err := w.db.Exec("DELETE FROM projects WHERE id = 'dir:B'"); err != nil {
		t.Fatal(err)
	}
	hits := w.search("mot", projects.Everything, Filter{})
	same(t, projectsOf(hits), []string{"dir:A", "dir:B", "dir:C"})
	for _, h := range hits {
		if h.Project == "dir:B" {
			same(t, h.Label, "unsorted")
		}
	}
	same(t, len(w.search("mot", projects.Everything, Filter{Label: "unsorted"})), 3)
	row, _ := w.view("sess-B", projects.Everything)
	same(t, row.Labels, []string{"unsorted"})
}

func TestScopeForLeavesNoRowForNewFolder(t *testing.T) {
	w := newWorld(t)
	w.scope(w.dir("nouveau"))
	for _, table := range []string{"projects", "project_paths"} {
		var n int
		if err := w.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d rows, err %v", table, n, err)
		}
	}
}

func TestSpecialCharacters(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, q := range []string{"potager-back", "C++", `dit "oui"`, "foo:bar", "*", "(", "AND", "NEAR",
		"a\x00b", "\x00", `"`, "^mot", "mot*", "-mot"} {
		if _, err := Search(w.db, q, projects.Everything, Filter{}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestSpecialWordIsFound(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.add(w.dir("A"), "sess-s", `il dit "oui" pour potager-back`, "2026-10-21T10:00:00.000Z")
	same(t, len(w.search(`dit "oui"`, projects.Everything, Filter{})), 1)
	same(t, len(w.search("potager-back", projects.Everything, Filter{})), 1)
}

func TestEmptyQuery(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, q := range []string{"   ", ""} {
		if _, err := Search(w.db, q, projects.Everything, Filter{}); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestToFTSQueryDoublesQuotes(t *testing.T) {
	got, err := ToFTSQuery(`a "b"`)
	if err != nil || got != `"a" """b"""` {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestToFTSQueryReplacesNul(t *testing.T) {
	got, _ := ToFTSQuery("a\x00b")
	same(t, got, "\"a�b\"")
}

func TestSearchIgnoresAccents(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "sess-m", "mémo de démarrage", defaultTS)
	for _, q := range []string{"memo", "MÉMO"} {
		same(t, len(w.search(q, projects.Everything, Filter{})), 1)
	}
}

func TestAccentlessMatchHighlightsOriginal(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "sess-m", "une mémoire longue", defaultTS)
	hit := w.search("memoire", projects.Everything, Filter{})[0]
	if !slices.Contains(hit.Segments, Segment{"mémoire", true}) {
		t.Fatalf("segments %v", hit.Segments)
	}
	var joined string
	for _, s := range hit.Segments {
		joined += s.Text
	}
	same(t, strings.Trim(joined, "… "), "une mémoire longue")
}

func TestSnippetHasNoMarkers(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, h := range w.search("mot", projects.Everything, Filter{}) {
		for _, s := range h.Segments {
			if strings.ContainsAny(s.Text, "\x01\x02") {
				t.Fatalf("marker left in %q", s.Text)
			}
		}
	}
}

func TestSegments(t *testing.T) {
	cases := []struct {
		snippet string
		want    []Segment
	}{
		{"", nil},
		{"plain", []Segment{{"plain", false}}},
		{"a \x01b\x02 c", []Segment{{"a ", false}, {"b", true}, {" c", false}}},
		{"\x01b\x02", []Segment{{"b", true}}},
		{"…\x01b\x02 \x01c\x02…", []Segment{{"…", false}, {"b", true}, {" ", false}, {"c", true}, {"…", false}}},
		{"a\x01b", []Segment{{"a", false}, {"b", false}}},
		{"a\x01\x02b", []Segment{{"a", false}, {"", true}, {"b", false}}},
	}
	for _, c := range cases {
		same(t, segments(c.snippet), c.want)
	}
}

func TestMoreRecentFirstAtEqualRelevance(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.add(w.dir("A"), "sess-new", "mot", "2026-10-25T10:00:00.000Z")
	w.add(w.dir("A"), "sess-old", "mot", "2026-10-22T10:00:00.000Z")
	var got []string
	for _, h := range w.search("mot", projects.Everything, Filter{}) {
		if h.SessionID == "sess-new" || h.SessionID == "sess-old" {
			got = append(got, h.SessionID)
		}
	}
	same(t, got, []string{"sess-new", "sess-old"})
}

func TestLimit(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	same(t, len(w.search("mot", projects.Everything, Filter{Limit: 2})), 2)
}

func TestDefaultLimit(t *testing.T) {
	w := newWorld(t)
	for i := range 25 {
		w.add(w.dir("A"), fmt.Sprintf("s%02d", i), "mot", defaultTS)
	}
	same(t, len(w.search("mot", projects.Everything, Filter{})), 20)
}

func TestLocalDay(t *testing.T) {
	loc := paris(t)
	w := newWorld(t)
	w.indexed()
	w.add(w.dir("A"), "sess-late", "tard", "2026-10-26T23:30:00.000Z")
	d27, d26 := day(t, loc, 2026, 10, 27), day(t, loc, 2026, 10, 26)
	same(t, ids(w.day(d27, projects.Everything)), []string{"sess-late"})
	same(t, ids(w.day(d26, projects.Everything)), []string{})
	same(t, len(w.search("tard", projects.Everything, Filter{Since: &d27})), 1)
	same(t, len(w.search("tard", projects.Everything, Filter{Until: &d26})), 0)
	same(t, len(w.search("tard", projects.Everything, Filter{Until: &d27})), 1)
}

func TestClockChange(t *testing.T) {
	loc := paris(t)
	w := newWorld(t)
	w.add(w.dir("A"), "sess-dst", "dst", "2026-10-25T23:30:00.000Z")
	w.add(w.dir("A"), "sess-dst2", "dst", "2026-10-25T22:59:00.000Z")
	same(t, ids(w.day(day(t, loc, 2026, 10, 26), projects.Everything)), []string{"sess-dst"})
	same(t, ids(w.day(day(t, loc, 2026, 10, 25), projects.Everything)), []string{"sess-dst2"})
}

func TestSinceIncludesLocalMidnight(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "pile", "minuit", "2026-10-20T00:00:00.000Z")
	d := day(t, time.UTC, 2026, 10, 20)
	same(t, len(w.search("minuit", projects.Everything, Filter{Since: &d})), 1)
}

func TestDayRespectsScope(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	d := day(t, time.UTC, 2026, 10, 20)
	same(t, ids(w.day(d, w.scope(w.dir("A")))), []string{"sess-A"})
}

func (w *world) mixed() {
	w.add(w.dir("A"), "mix", "depuis A", "2026-10-20T08:00:00.000Z")
	w.add(w.dir("C"), "mix", "depuis C", "2026-10-21T09:00:00.000Z", aiTitle("mix", "Titre mixte"))
}

func TestSessionShowsOnlyWhatIsVisible(t *testing.T) {
	w := newWorld(t)
	w.mixed()
	row, msgs := w.view("mix", w.scope(w.dir("A")))
	same(t, texts(msgs), []string{"depuis A"})
	same(t, row.Projects, []string{"dir:A"})
	same(t, row.Messages, 1)
	same(t, row.StartedAt, "2026-10-20T08:00:00.000Z")
	same(t, row.EndedAt, "2026-10-20T08:00:00.000Z")
	row, msgs = w.view("mix", projects.Everything)
	same(t, texts(msgs), []string{"depuis A", "depuis C"})
	same(t, row.Projects, []string{"dir:A", "dir:C"})
	same(t, row.EndedAt, "2026-10-21T09:00:00.000Z")
}

func TestListAndDayDoNotLeakDates(t *testing.T) {
	w := newWorld(t)
	w.mixed()
	s := w.scope(w.dir("A"))
	rows := w.list(s, ListFilter{})
	same(t, len(rows), 1)
	same(t, []any{rows[0].EndedAt, rows[0].Projects, rows[0].Messages}, []any{"2026-10-20T08:00:00.000Z", []string{"dir:A"}, 1})
	same(t, len(w.day(day(t, time.UTC, 2026, 10, 21), s)), 0)
	rows = w.day(day(t, time.UTC, 2026, 10, 20), s)
	same(t, len(rows), 1)
	same(t, []any{rows[0].EndedAt, rows[0].Projects, rows[0].Messages}, []any{"2026-10-20T08:00:00.000Z", []string{"dir:A"}, 1})
}

func TestSessionOutsideScope(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	s := w.scope(w.dir("A"))
	if row, _ := w.view("sess-C", s); row != nil {
		t.Fatal("sess-C visible")
	}
	row, _ := w.view("sess", s)
	same(t, row.ID, "sess-A")
}

func TestSessionWithoutVisibleMessageIsAbsent(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	if _, err := w.db.Exec("INSERT INTO sessions(id, title) VALUES ('sess-vide', 'Titre seul')"); err != nil {
		t.Fatal(err)
	}
	same(t, slices.Contains(ids(w.list(projects.Everything, ListFilter{})), "sess-vide"), false)
	if row, _ := w.view("sess-vide", projects.Everything); row != nil {
		t.Fatal("empty session visible")
	}
}

func TestIDPrefix(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "abcd-1", "un", defaultTS)
	w.add(w.dir("A"), "abef-2", "deux", defaultTS)
	w.add(w.dir("B"), "abgg-3", "trois", defaultTS)
	row, _ := w.view("abc", projects.Everything)
	same(t, row.ID, "abcd-1")
	_, _, err := SessionView(w.db, "ab", projects.Everything)
	var amb *ErrAmbiguousSession
	if !errors.As(err, &amb) {
		t.Fatalf("err %v", err)
	}
	same(t, amb.IDs, []string{"abcd-1", "abef-2", "abgg-3"})
	row, _ = w.view("ab", w.scope(w.dir("B")))
	same(t, row.ID, "abgg-3")
}

func TestExactIDBeatsPrefix(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "ab", "court", defaultTS)
	w.add(w.dir("A"), "abc", "long", defaultTS)
	row, _ := w.view("ab", projects.Everything)
	same(t, row.ID, "ab")
}

func TestRefHasNoWildcard(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, ref := range []string{"%", "sess-_", ""} {
		if row, _ := w.view(ref, projects.Everything); row != nil {
			t.Errorf("%q matched %s", ref, row.ID)
		}
	}
}

func TestListIsPaged(t *testing.T) {
	w := newWorld(t)
	for _, d := range []string{"20", "21", "22", "23"} {
		w.add(w.dir("A"), "s"+d, "x", "2026-10-"+d+"T10:00:00.000Z")
	}
	first := w.list(projects.Everything, ListFilter{Limit: 2})
	same(t, ids(first), []string{"s23", "s22"})
	second := w.list(projects.Everything, ListFilter{Before: first[1].EndedAt, Limit: 2})
	same(t, ids(second), []string{"s21", "s20"})
	same(t, len(w.list(projects.Everything, ListFilter{Before: second[1].EndedAt})), 0)
}

func TestListFiltersProjectAndLabel(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.label("dir:B", "work")
	same(t, ids(w.list(projects.Everything, ListFilter{Project: "dir:A"})), []string{"sess-A"})
	same(t, ids(w.list(projects.Everything, ListFilter{Label: "work"})), []string{"sess-B"})
	same(t, len(w.list(w.scope(w.dir("A")), ListFilter{Project: "dir:B"})), 0)
}

func TestFilteredListKeepsScopeRows(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "mix", "a", "2026-10-20T08:00:00.000Z")
	w.add(w.dir("B"), "mix", "b", "2026-10-21T09:00:00.000Z")
	plain := w.list(projects.Everything, ListFilter{})
	filtered := w.list(projects.Everything, ListFilter{Project: "dir:A"})
	same(t, filtered, plain)
	same(t, []any{filtered[0].EndedAt, filtered[0].Projects, filtered[0].Messages},
		[]any{"2026-10-21T09:00:00.000Z", []string{"dir:A", "dir:B"}, 2})
}

func TestFilteredListSortsAndCutsOnLastVisible(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "s1", "a", "2026-10-20T08:00:00.000Z")
	w.add(w.dir("B"), "s1", "b", "2026-10-25T08:00:00.000Z")
	w.add(w.dir("A"), "s2", "c", "2026-10-22T08:00:00.000Z")
	same(t, ids(w.list(projects.Everything, ListFilter{Project: "dir:A"})), []string{"s1", "s2"})
	same(t, ids(w.list(projects.Everything, ListFilter{Project: "dir:A", Before: "2026-10-23T00:00:00.000Z"})), []string{"s2"})
}

func TestPagingLosesNothingOnTies(t *testing.T) {
	w := newWorld(t)
	for _, n := range []string{"t1", "t2", "t3"} {
		w.add(w.dir("A"), n, "x", defaultTS)
	}
	w.add(w.dir("A"), "t0", "x", "2026-10-19T10:00:00.000Z")
	page := w.list(projects.Everything, ListFilter{Limit: 2})
	same(t, ids(page), []string{"t1", "t2", "t3"})
	same(t, ids(w.list(projects.Everything, ListFilter{Before: page[2].EndedAt})), []string{"t0"})
}

func TestTitleHiddenWhenSessionLeavesScope(t *testing.T) {
	w := newWorld(t)
	w.mixed()
	w.add(w.dir("A"), "seule", "x", defaultTS, aiTitle("seule", "Titre propre"))
	s := w.scope(w.dir("A"))
	titles := map[string]*string{}
	for _, r := range w.list(s, ListFilter{}) {
		titles[r.ID] = r.Title
	}
	if titles["mix"] != nil || titles["seule"] == nil || *titles["seule"] != "Titre propre" {
		t.Fatalf("titles %v", titles)
	}
	if row, _ := w.view("mix", s); row.Title != nil {
		t.Fatal("title leaked by view")
	}
	for _, r := range w.day(day(t, time.UTC, 2026, 10, 20), s) {
		if r.ID == "mix" && r.Title != nil {
			t.Fatal("title leaked by day")
		}
	}
	if row, _ := w.view("mix", projects.Everything); row.Title == nil || *row.Title != "Titre mixte" {
		t.Fatalf("title %v", row.Title)
	}
}

func TestScopeForLeavesCallerTransaction(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO agent_uses(id) VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := projects.ScopeFor(bg, tx, w.cfg, w.dir("A")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM agent_uses").Scan(&n); err != nil || n != 1 {
		t.Fatalf("transaction lost: %d, %v", n, err)
	}
}

func TestMessagesInTimeOrder(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "ord", "second", "2026-10-22T10:00:00.000Z")
	w.add(w.dir("A"), "ord", "premier", "2026-10-21T10:00:00.000Z")
	_, msgs := w.view("ord", projects.Everything)
	same(t, texts(msgs), []string{"premier", "second"})
}

func TestListSortsOnLastMessage(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "s1", "a", "2026-10-20T08:00:00.000Z")
	w.add(w.dir("A"), "s1", "b", "2026-10-25T08:00:00.000Z")
	w.add(w.dir("A"), "s2", "c", "2026-10-22T08:00:00.000Z")
	same(t, ids(w.list(projects.Everything, ListFilter{})), []string{"s1", "s2"})
	same(t, ids(w.list(projects.Everything, ListFilter{Before: "2026-10-23T00:00:00.000Z"})), []string{"s2"})
}

func TestLabelsAndSessionStart(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "lab", "a", "2026-10-21T08:00:00.000Z")
	w.add(w.dir("B"), "lab", "b", "2026-10-19T08:00:00.000Z")
	w.label("dir:B", "work")
	rows := w.list(projects.Everything, ListFilter{})
	same(t, rows[0].Labels, []string{"unsorted", "work"})
	same(t, []string{rows[0].StartedAt, rows[0].EndedAt}, []string{"2026-10-19T08:00:00.000Z", "2026-10-21T08:00:00.000Z"})
}

func TestNames(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	names, err := Names(w.db, []string{"dir:A", "github.com/o/potager", "path:/x/lone"})
	if err != nil {
		t.Fatal(err)
	}
	same(t, names, map[string]string{"dir:A": "A", "github.com/o/potager": "potager", "path:/x/lone": "lone"})
}

func TestConcurrentSearchesAndIndexersFromNewFolder(t *testing.T) {
	const rounds, searches, indexers = 6, 6, 2
	for n := range rounds {
		dir := t.TempDir()
		p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "t")}
		root := filepath.Join(dir, "code")
		folder := filepath.Join(root, "neuf")
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		lines := make([]map[string]any, 1500)
		for i := range lines {
			lines[i] = entry("s1", folder, fmt.Sprintf("pomme %d", i), fmt.Sprintf("2026-10-20T10:%02d:%02d.000Z", i/60%60, i%60))
			lines[i]["uuid"] = fmt.Sprintf("u%d", i)
		}
		writeTranscript(t, p, "-p", "s1", lines...)
		d, err := db.Open(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		d.Close()

		go_ := filepath.Join(dir, "go")
		var kids []*exec.Cmd
		var outs []*strings.Builder
		for i := range searches + indexers {
			role := "search"
			if i >= searches {
				role = "index"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Dir = folder
			cmd.Env = append(os.Environ(), "PAUL_TEST_CHILD="+role, "PAUL_HOME="+p.Home,
				"PAUL_TRANSCRIPTS="+p.Transcripts, "PAUL_TEST_ROOT="+root, "PAUL_TEST_GO="+go_)
			out := &strings.Builder{}
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			kids, outs = append(kids, cmd), append(outs, out)
		}
		time.Sleep(500 * time.Millisecond)
		os.WriteFile(go_, nil, 0o600)
		for i, kid := range kids {
			if err := kid.Wait(); err != nil {
				t.Errorf("round %d child %d: %v\n%s", n, i, err, outs[i])
			}
		}
		d, err = db.Open(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		d.QueryRow("SELECT count(*) FROM messages").Scan(&count)
		d.Close()
		if count != 1500 {
			t.Errorf("round %d: %d messages", n, count)
		}
	}
}

// Re-executed by the concurrency test as one of the competing processes.
func TestMain(m *testing.M) {
	role := os.Getenv("PAUL_TEST_CHILD")
	if role == "" {
		os.Exit(m.Run())
	}
	for {
		if _, err := os.Stat(os.Getenv("PAUL_TEST_GO")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := child(role); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func child(role string) error {
	p := paths.Paths{Home: os.Getenv("PAUL_HOME"), Transcripts: os.Getenv("PAUL_TRANSCRIPTS")}
	cfg := config.Config{Roots: []string{os.Getenv("PAUL_TEST_ROOT")}}
	if role == "index" {
		_, err := index.Run(p, cfg)
		return err
	}
	conn, err := db.Open(p, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := index.Refresh(conn, p, cfg); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	scope, err := projects.ScopeFor(bg, conn, cfg, cwd)
	if err != nil {
		return err
	}
	_, err = Search(conn, "pomme", scope, Filter{})
	return err
}
