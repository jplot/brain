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

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

type world struct {
	t     *testing.T
	p     paths.Paths
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
	return &world{t: t, p: p, db: d, root: root}
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
	if stats := index.All(w.db, w.p); stats.Failed != 0 {
		w.t.Fatalf("index: %+v", stats)
	}
}

func (w *world) subject(name string, parent *string) string {
	w.t.Helper()
	uid, err := subjects.Create(w.db, name, nil, parent)
	if err != nil {
		w.t.Fatal(err)
	}
	return uid
}

func (w *world) about(session string, subjectUIDs ...string) {
	w.t.Helper()
	w.files++
	uid := fmt.Sprint("ep", w.files)
	if _, err := w.db.Exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		uid, session, defaultTS, defaultTS, defaultTS, defaultTS); err != nil {
		w.t.Fatal(err)
	}
	for _, s := range subjectUIDs {
		if _, err := w.db.Exec("INSERT INTO episode_subjects(episode_uid, subject_uid) VALUES (?, ?)", uid, s); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) indexed() {
	for _, n := range []string{"A", "B", "C"} {
		w.add(w.dir(n), "sess-"+n, "mot de "+n, defaultTS)
	}
}

func (w *world) search(text string, f Filter) []Hit {
	w.t.Helper()
	hits, err := Search(w.db, text, f)
	if err != nil {
		w.t.Fatal(err)
	}
	return hits
}

func (w *world) list(f ListFilter) []SessionRow {
	w.t.Helper()
	rows, err := ListSessions(w.db, f)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func (w *world) day(d time.Time) []SessionRow {
	w.t.Helper()
	rows, err := DayView(w.db, d)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func (w *world) view(ref string) (*SessionRow, []Message) {
	w.t.Helper()
	row, msgs, err := SessionView(w.db, ref)
	if err != nil {
		w.t.Fatal(err)
	}
	return row, msgs
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

func TestSpecialCharacters(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, q := range []string{"potager-back", "C++", `dit "oui"`, "foo:bar", "*", "(", "AND", "NEAR",
		"a\x00b", "\x00", `"`, "^mot", "mot*", "-mot"} {
		if _, err := Search(w.db, q, Filter{}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
}

func TestSpecialWordIsFound(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.add(w.dir("A"), "sess-s", `il dit "oui" pour potager-back`, "2026-10-21T10:00:00.000Z")
	same(t, len(w.search(`dit "oui"`, Filter{})), 1)
	same(t, len(w.search("potager-back", Filter{})), 1)
}

func TestEmptyQuery(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, q := range []string{"   ", ""} {
		if _, err := Search(w.db, q, Filter{}); !errors.Is(err, ErrEmptyQuery) {
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
		same(t, len(w.search(q, Filter{})), 1)
	}
}

func TestAccentlessMatchHighlightsOriginal(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "sess-m", "une mémoire longue", defaultTS)
	hit := w.search("memoire", Filter{})[0]
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
	for _, h := range w.search("mot", Filter{}) {
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
	for _, h := range w.search("mot", Filter{}) {
		if h.SessionID == "sess-new" || h.SessionID == "sess-old" {
			got = append(got, h.SessionID)
		}
	}
	same(t, got, []string{"sess-new", "sess-old"})
}

func TestLimit(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	same(t, len(w.search("mot", Filter{Limit: 2})), 2)
}

func TestDefaultLimit(t *testing.T) {
	w := newWorld(t)
	for i := range 25 {
		w.add(w.dir("A"), fmt.Sprintf("s%02d", i), "mot", defaultTS)
	}
	same(t, len(w.search("mot", Filter{})), 20)
}

func TestLocalDay(t *testing.T) {
	loc := paris(t)
	w := newWorld(t)
	w.indexed()
	w.add(w.dir("A"), "sess-late", "tard", "2026-10-26T23:30:00.000Z")
	d27, d26 := day(t, loc, 2026, 10, 27), day(t, loc, 2026, 10, 26)
	same(t, ids(w.day(d27)), []string{"sess-late"})
	same(t, ids(w.day(d26)), []string{})
	same(t, len(w.search("tard", Filter{Since: &d27})), 1)
	same(t, len(w.search("tard", Filter{Until: &d26})), 0)
	same(t, len(w.search("tard", Filter{Until: &d27})), 1)
}

func TestClockChange(t *testing.T) {
	loc := paris(t)
	w := newWorld(t)
	w.add(w.dir("A"), "sess-dst", "dst", "2026-10-25T23:30:00.000Z")
	w.add(w.dir("A"), "sess-dst2", "dst", "2026-10-25T22:59:00.000Z")
	same(t, ids(w.day(day(t, loc, 2026, 10, 26))), []string{"sess-dst"})
	same(t, ids(w.day(day(t, loc, 2026, 10, 25))), []string{"sess-dst2"})
}

func TestSinceIncludesLocalMidnight(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "pile", "minuit", "2026-10-20T00:00:00.000Z")
	d := day(t, time.UTC, 2026, 10, 20)
	same(t, len(w.search("minuit", Filter{Since: &d})), 1)
}

func (w *world) mixed() {
	w.add(w.dir("A"), "mix", "depuis A", "2026-10-20T08:00:00.000Z")
	w.add(w.dir("C"), "mix", "depuis C", "2026-10-21T09:00:00.000Z", aiTitle("mix", "Titre mixte"))
}

func TestSessionViewSpansAllMessages(t *testing.T) {
	w := newWorld(t)
	w.mixed()
	row, msgs := w.view("mix")
	same(t, texts(msgs), []string{"depuis A", "depuis C"})
	same(t, row.Messages, 2)
	same(t, row.StartedAt, "2026-10-20T08:00:00.000Z")
	same(t, row.EndedAt, "2026-10-21T09:00:00.000Z")
	if row.Title == nil || *row.Title != "Titre mixte" {
		t.Fatalf("title %v", row.Title)
	}
}

func TestListAndDayRowsCarrySpan(t *testing.T) {
	w := newWorld(t)
	w.mixed()
	want := []any{"2026-10-20T08:00:00.000Z", "2026-10-21T09:00:00.000Z", 2, "Titre mixte"}
	got := func(r SessionRow) []any { return []any{r.StartedAt, r.EndedAt, r.Messages, *r.Title} }
	rows := w.list(ListFilter{})
	same(t, len(rows), 1)
	same(t, got(rows[0]), want)
	rows = w.day(day(t, time.UTC, 2026, 10, 21))
	same(t, len(rows), 1)
	same(t, got(rows[0]), want)
	same(t, len(w.day(day(t, time.UTC, 2026, 10, 22))), 0)
}

func TestSessionWithoutVisibleMessageIsAbsent(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	if _, err := w.db.Exec("INSERT INTO sessions(id, title) VALUES ('sess-vide', 'Titre seul')"); err != nil {
		t.Fatal(err)
	}
	same(t, slices.Contains(ids(w.list(ListFilter{})), "sess-vide"), false)
	if row, _ := w.view("sess-vide"); row != nil {
		t.Fatal("empty session visible")
	}
}

func TestIDPrefix(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "abcd-1", "un", defaultTS)
	w.add(w.dir("A"), "abef-2", "deux", defaultTS)
	w.add(w.dir("B"), "abgg-3", "trois", defaultTS)
	row, _ := w.view("abc")
	same(t, row.ID, "abcd-1")
	_, _, err := SessionView(w.db, "ab")
	var amb *ErrAmbiguousSession
	if !errors.As(err, &amb) {
		t.Fatalf("err %v", err)
	}
	same(t, amb.IDs, []string{"abcd-1", "abef-2", "abgg-3"})
}

func TestExactIDBeatsPrefix(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "ab", "court", defaultTS)
	w.add(w.dir("A"), "abc", "long", defaultTS)
	row, _ := w.view("ab")
	same(t, row.ID, "ab")
}

func TestRefHasNoWildcard(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	for _, ref := range []string{"%", "sess-_", ""} {
		if row, _ := w.view(ref); row != nil {
			t.Errorf("%q matched %s", ref, row.ID)
		}
	}
}

func TestListIsPaged(t *testing.T) {
	w := newWorld(t)
	for _, d := range []string{"20", "21", "22", "23"} {
		w.add(w.dir("A"), "s"+d, "x", "2026-10-"+d+"T10:00:00.000Z")
	}
	first := w.list(ListFilter{Limit: 2})
	same(t, ids(first), []string{"s23", "s22"})
	second := w.list(ListFilter{Before: first[1].EndedAt, Limit: 2})
	same(t, ids(second), []string{"s21", "s20"})
	same(t, len(w.list(ListFilter{Before: second[1].EndedAt})), 0)
}

func TestPagingLosesNothingOnTies(t *testing.T) {
	w := newWorld(t)
	for _, n := range []string{"t1", "t2", "t3"} {
		w.add(w.dir("A"), n, "x", defaultTS)
	}
	w.add(w.dir("A"), "t0", "x", "2026-10-19T10:00:00.000Z")
	page := w.list(ListFilter{Limit: 2})
	same(t, ids(page), []string{"t1", "t2", "t3"})
	same(t, ids(w.list(ListFilter{Before: page[2].EndedAt})), []string{"t0"})
}

func (w *world) aboutWorld() (maison, reseau, outil string) {
	w.indexed()
	maison = w.subject("Maison", nil)
	reseau = w.subject("Réseau", &maison)
	outil = w.subject("Outil", nil)
	w.about("sess-A", maison)
	w.about("sess-B", reseau, outil)
	return maison, reseau, outil
}

func TestSearchWithoutSubjectSeesEverything(t *testing.T) {
	w := newWorld(t)
	w.aboutWorld()
	same(t, len(w.search("mot", Filter{})), 3)
}

func TestSearchBySubjectIncludesDescendants(t *testing.T) {
	w := newWorld(t)
	_, reseau, outil := w.aboutWorld()
	sessions := func(f Filter) []string {
		var out []string
		for _, h := range w.search("mot", f) {
			out = append(out, h.SessionID)
		}
		slices.Sort(out)
		return out
	}
	same(t, sessions(Filter{Subject: "maison"}), []string{"sess-A", "sess-B"})
	same(t, sessions(Filter{Subject: "RESEAU"}), []string{"sess-B"})
	same(t, sessions(Filter{Subject: outil}), []string{"sess-B"})
	same(t, sessions(Filter{Subject: reseau}), []string{"sess-B"})
}

func TestUnknownSubjectIsAnError(t *testing.T) {
	w := newWorld(t)
	w.aboutWorld()
	var unknown *subjects.ErrUnknownSubject
	for name, run := range map[string]func() error{
		"search": func() error { _, err := Search(w.db, "mot", Filter{Subject: "mason"}); return err },
		"list":   func() error { _, err := ListSessions(w.db, ListFilter{Subject: "mason"}); return err },
	} {
		if err := run(); !errors.As(err, &unknown) || unknown.Term != "mason" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSearchAmbiguousSubject(t *testing.T) {
	w := newWorld(t)
	w.indexed()
	w.subject("Lune", nil)
	if _, err := subjects.Create(w.db, "Satellite", []string{"lune"}, nil); err != nil {
		t.Fatal(err)
	}
	var amb *subjects.ErrAmbiguousSubject
	for name, run := range map[string]func() error{
		"search": func() error { _, err := Search(w.db, "mot", Filter{Subject: "lune"}); return err },
		"list":   func() error { _, err := ListSessions(w.db, ListFilter{Subject: "lune"}); return err },
	} {
		if err := run(); !errors.As(err, &amb) || len(amb.Candidates) != 2 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRowsCarrySubjectNames(t *testing.T) {
	w := newWorld(t)
	w.aboutWorld()
	hits := w.search("mot de B", Filter{})
	same(t, hits[0].Subjects, []string{"Outil", "Réseau"})
	rows := w.list(ListFilter{})
	by := map[string][]string{}
	for _, r := range rows {
		by[r.ID] = r.Subjects
	}
	same(t, by, map[string][]string{"sess-A": {"Maison"}, "sess-B": {"Outil", "Réseau"}, "sess-C": nil})
	row, _ := w.view("sess-B")
	same(t, row.Subjects, []string{"Outil", "Réseau"})
	same(t, w.day(day(t, time.UTC, 2026, 10, 20))[0].Subjects, []string{"Maison"})
}

func TestListBySubject(t *testing.T) {
	w := newWorld(t)
	w.aboutWorld()
	same(t, ids(w.list(ListFilter{Subject: "maison"})), []string{"sess-A", "sess-B"})
	same(t, ids(w.list(ListFilter{Subject: "outil"})), []string{"sess-B"})
}

func TestFilteredListPagesByLastMessage(t *testing.T) {
	w := newWorld(t)
	sujet := w.subject("Sujet", nil)
	for _, n := range []string{"t1", "t2", "t3"} {
		w.add(w.dir("A"), n, "x", defaultTS)
		w.about(n, sujet)
	}
	w.add(w.dir("A"), "t0", "x", "2026-10-19T10:00:00.000Z")
	w.about("t0", sujet)
	w.add(w.dir("A"), "autre", "x", "2026-10-21T10:00:00.000Z")
	page := w.list(ListFilter{Subject: "sujet", Limit: 2})
	same(t, ids(page), []string{"t1", "t2", "t3"})
	same(t, ids(w.list(ListFilter{Subject: "sujet", Before: page[2].EndedAt})), []string{"t0"})
}

func TestSubjectSearchInsideTransaction(t *testing.T) {
	w := newWorld(t)
	w.aboutWorld()
	tx, err := w.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	hits, err := Search(tx, "mot", Filter{Subject: "outil"})
	if err != nil {
		t.Fatal(err)
	}
	same(t, len(hits), 1)
}

func TestMessagesInTimeOrder(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "ord", "second", "2026-10-22T10:00:00.000Z")
	w.add(w.dir("A"), "ord", "premier", "2026-10-21T10:00:00.000Z")
	_, msgs := w.view("ord")
	same(t, texts(msgs), []string{"premier", "second"})
}

func TestListSortsOnLastMessage(t *testing.T) {
	w := newWorld(t)
	w.add(w.dir("A"), "s1", "a", "2026-10-20T08:00:00.000Z")
	w.add(w.dir("A"), "s1", "b", "2026-10-25T08:00:00.000Z")
	w.add(w.dir("A"), "s2", "c", "2026-10-22T08:00:00.000Z")
	same(t, ids(w.list(ListFilter{})), []string{"s1", "s2"})
	same(t, ids(w.list(ListFilter{Before: "2026-10-23T00:00:00.000Z"})), []string{"s2"})
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
	if role == "index" {
		_, err := index.Run(p)
		return err
	}
	conn, err := db.Open(p, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := index.Refresh(conn, p); err != nil {
		return err
	}
	_, err = Search(conn, "pomme", Filter{})
	return err
}
