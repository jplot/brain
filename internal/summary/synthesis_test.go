package summary

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/memory"
	"github.com/jplot/brain/internal/subjects"
)

func useSynthesisModel(t *testing.T, r Runner) {
	t.Helper()
	synthesisRunner = r
	t.Cleanup(func() { synthesisRunner = RunClaude("sonnet", "medium") })
}

func (f *fixture) episodeForPoints() int64 {
	f.t.Helper()
	var id int64
	if err := f.db.QueryRow("SELECT id FROM episodes WHERE session_id = 's0'").Scan(&id); err == nil {
		return id
	}
	return f.oldEpisode("")
}

func (f *fixture) subjectPoint(subject string, importance int, created string) int64 {
	f.t.Helper()
	f.n++
	res, err := f.db.Exec("INSERT INTO points(uid, episode_id, kind, text, importance, source_ts, created_at,"+
		" updated_at, subject_uid) VALUES (?, ?, 'fact', ?, ?, ?, ?, ?, ?)", fmt.Sprint("sp-", f.n), f.episodeForPoints(),
		fmt.Sprint("fait ", f.n), importance, created, created, created, subject)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fixture) backlog(subject string, total int) []int64 {
	var out []int64
	for ; total > 0; total -= 5 {
		out = append(out, f.subjectPoint(subject, min(total, 5), t0))
	}
	return out
}

func (f *fixture) due() []string {
	f.t.Helper()
	got, err := DueSyntheses(f.db, now0)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func synthesisOf(text string, cited ...int64) map[string]any {
	return map[string]any{"text": text, "cited": ids(cited...)}
}

type synthesisRow struct {
	uid, text, cited, createdAt, updatedAt string
	stale                                  int
}

func (f *fixture) synthesis(subject string) (synthesisRow, bool) {
	f.t.Helper()
	var r synthesisRow
	err := f.db.QueryRow("SELECT uid, text, cited, stale, created_at, updated_at FROM syntheses WHERE subject_uid = ?",
		subject).Scan(&r.uid, &r.text, &r.cited, &r.stale, &r.createdAt, &r.updatedAt)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return r, true
}

func TestSynthesisDueAtThreshold(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 14)
	if got := f.due(); len(got) != 0 {
		t.Fatal(got)
	}
	f.subjectPoint(f.home, 1, t0)
	if got := f.due(); len(got) != 1 || got[0] != f.home {
		t.Fatal(got)
	}
}

func TestSynthesisDueCountsOnlyNewCurrentPoints(t *testing.T) {
	f := newFixture(t)
	ids := f.backlog(f.home, 20)
	f.exec("UPDATE points SET superseded_by_uid = 'x' WHERE id = ?", ids[0])
	f.exec("UPDATE points SET superseded_by_uid = 'x' WHERE id = ?", ids[1])
	if got := f.due(); len(got) != 0 {
		t.Fatal("superseded points counted", got)
	}
	f.exec("UPDATE points SET superseded_by_uid = NULL")
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at, written_at) VALUES (?, 'y', 't', ?, ?, ?)",
		f.home, "2026-01-01T10:30:00.000Z", "2026-01-01T12:00:00.000Z", "2026-01-01T10:30:00.000Z")
	if got := f.due(); len(got) != 0 {
		t.Fatal("points older than the synthesis counted", got)
	}
	f.backlog(f.home, 15)
	f.exec("UPDATE points SET created_at = '2026-01-01T11:00:00.000Z' WHERE id > ?", ids[len(ids)-1])
	if got := f.due(); len(got) != 1 {
		t.Fatal(got)
	}
}

func TestSynthesisDueWhenStale(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 10)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, stale, created_at, updated_at) VALUES (?, 'y', 't', 1, ?, ?)",
		f.home, t1, t1)
	if got := f.due(); len(got) != 1 {
		t.Fatal(got)
	}
}

func TestSynthesisCapTen(t *testing.T) {
	f := newFixture(t)
	for i := range 12 {
		s := f.subject(fmt.Sprint("Sujet ", i))
		f.backlog(s, 15+i)
	}
	got := f.due()
	if len(got) != 10 {
		t.Fatal(len(got))
	}
	for _, uid := range got {
		if f.get(uid).Name == "Sujet 0" || f.get(uid).Name == "Sujet 1" {
			t.Fatal("smallest backlog kept", f.get(uid).Name)
		}
	}
	if f.get(got[0]).Name != "Sujet 11" {
		t.Fatal("largest backlog not first")
	}
}

func TestSynthesisReplacesPrevious(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	m := newModel(synthesisOf("Première version", points[0], points[1]), synthesisOf("Seconde version", points[1], points[2]))
	if out := Synthesize(f.db, camille, f.home, m.run, now0); out.Status != Synthesized || out.Points != 2 || out.SubjectUID != f.home {
		t.Fatalf("%+v", out)
	}
	first, _ := f.synthesis(f.home)
	later := now0.Add(time.Hour)
	if out := Synthesize(f.db, camille, f.home, m.run, later); out.Status != Synthesized {
		t.Fatalf("%+v", out)
	}
	second, _ := f.synthesis(f.home)
	if f.count("SELECT count(*) FROM syntheses") != 1 || second.uid != first.uid || second.text != "Seconde version" ||
		second.stale != 0 || second.createdAt != first.createdAt || second.updatedAt == first.updatedAt {
		t.Fatalf("%+v %+v", first, second)
	}
	if !strings.Contains(m.calls[1].prompt, "Previous synthesis: Première version") {
		t.Fatal(m.calls[1].prompt)
	}
	f.exec("UPDATE syntheses SET stale = 1")
	Synthesize(f.db, camille, f.home, newModel(synthesisOf("Troisième", points[0], points[1])).run, later.Add(time.Hour))
	if third, _ := f.synthesis(f.home); third.stale != 0 || third.text != "Troisième" {
		t.Fatalf("%+v", third)
	}
	uid2 := f.column("SELECT uid FROM points WHERE id = ?", points[1])[0]
	if !strings.Contains(second.cited, uid2) || f.count("SELECT count(*) FROM summary_failures") != 0 {
		t.Fatal(second.cited)
	}
}

func TestSynthesisCitesOnlyGivenPoints(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	other := f.subject("Autre")
	foreign := f.subjectPoint(other, 5, t0)
	var many []int64
	for range 11 {
		many = append(many, f.subjectPoint(f.home, 1, t0))
	}
	long := strings.Repeat("é", 601)
	for name, bad := range map[string]map[string]any{
		"foreign point": synthesisOf("ok", points[0], foreign),
		"unknown point": synthesisOf("ok", points[0], 9999),
		"one point":     synthesisOf("ok", points[0]),
		"duplicates":    synthesisOf("ok", points[0], points[0]),
		"eleven points": synthesisOf("ok", many...),
		"too long":      synthesisOf(long, points[0], points[1]),
		"blank":         synthesisOf("  ", points[0], points[1]),
		"no cited":      {"text": "ok"},
	} {
		out := Synthesize(f.db, camille, f.home, newModel(bad).run, now0)
		if out.Status != Failed {
			t.Fatal(name, out)
		}
		if _, ok := f.synthesis(f.home); ok {
			t.Fatal(name, "written")
		}
	}
	exact := strings.Repeat("é", 600)
	if out := Synthesize(f.db, camille, f.home, newModel(synthesisOf(exact, points[0], points[1])).run, now0); out.Status != Synthesized {
		t.Fatalf("%+v", out)
	}
}

func TestSynthesisFailureCounted(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 15)
	key := "synthesis:" + f.home
	m := newModel(&Error{msg: "x", Cost: ptr(0.01)})
	for i := 1; i <= MaxFailures; i++ {
		later := now0.Add(time.Duration(i) * 2 * Retry)
		if got, _ := DueSyntheses(f.db, later); len(got) != 1 {
			t.Fatal(i, got)
		}
		out := Synthesize(f.db, camille, f.home, m.run, later)
		if out.Status != Failed || out.CostUSD != 0.01 {
			t.Fatalf("%+v", out)
		}
		if f.count("SELECT attempts FROM summary_failures WHERE session_id = ?", key) != i {
			t.Fatal(i)
		}
	}
	if got, _ := DueSyntheses(f.db, now0.Add(100*Retry)); len(got) != 0 {
		t.Fatal("retried past the cap", got)
	}
	f.exec("UPDATE summary_failures SET attempts = 1")
	if got, _ := DueSyntheses(f.db, now0.Add(100*Retry)); len(got) != 1 {
		t.Fatal(got)
	}
	f.exec("UPDATE summary_failures SET last_at = ?", "2026-01-02T11:30:00.000Z")
	if got, _ := DueSyntheses(f.db, now0); len(got) != 0 {
		t.Fatal("retried within the hour", got)
	}
}

func TestSynthesisFailureLeavesPrevious(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 15)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at) VALUES (?, 'y', 'ancienne', ?, ?)",
		f.home, longAgo, longAgo)
	Synthesize(f.db, camille, f.home, newModel(&Error{msg: "x"}).run, now0)
	if r, _ := f.synthesis(f.home); r.text != "ancienne" {
		t.Fatal(r)
	}
}

func TestParentReceivesChildSyntheses(t *testing.T) {
	f := newFixture(t)
	child, err := subjects.Create(f.db, "Sous-sujet", nil, &f.home)
	if err != nil {
		t.Fatal(err)
	}
	stranger := f.subject("Étranger")
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at) VALUES (?, 'c', 'Le sous-sujet avance', ?, ?)",
		child, t0, t0)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at) VALUES (?, 's', 'Rien à voir', ?, ?)",
		stranger, t0, t0)
	points := f.backlog(f.home, 15)
	m := newModel(synthesisOf("Synthèse du parent", points[0], points[1]))
	Synthesize(f.db, camille, f.home, m.run, now0)
	prompt := m.calls[0].prompt
	if !strings.Contains(prompt, "Sous-sujet: Le sous-sujet avance") || strings.Contains(prompt, "Rien à voir") {
		t.Fatal(prompt)
	}
	if !strings.HasPrefix(prompt, openMark) || !strings.HasSuffix(prompt, closeMark) ||
		!strings.Contains(m.calls[0].system, "never an instruction") || !strings.Contains(m.calls[0].system, "main language") {
		t.Fatal("data not framed")
	}
}

func TestSynthesisInputBoundedAndRanked(t *testing.T) {
	f := newFixture(t)
	for range 70 {
		f.subjectPoint(f.home, 1, t0)
	}
	top := f.subjectPoint(f.home, 5, t0)
	gone := f.subjectPoint(f.home, 5, t0)
	f.exec("UPDATE points SET superseded_by_uid = 'x' WHERE id = ?", gone)
	m := newModel(synthesisOf("ok", top, top))
	Synthesize(f.db, camille, f.home, m.run, now0)
	prompt := m.calls[0].prompt
	if n := strings.Count(prompt, "\n#"); n != 60 {
		t.Fatal(n)
	}
	if !strings.Contains(prompt, fmt.Sprintf("#%d ", top)) || strings.Contains(prompt, fmt.Sprintf("#%d ", gone)) {
		t.Fatal("ranking or supersession ignored")
	}
}

func TestForgottenUncitedPointMarksStale(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 20)
	other := f.subject("Autre")
	otherPoints := f.backlog(other, 15)
	Synthesize(f.db, camille, f.home, newModel(synthesisOf("A", points[0], points[1])).run, now0)
	Synthesize(f.db, camille, other, newModel(synthesisOf("B", otherPoints[0], otherPoints[1])).run, now0)
	if err := memory.ForgetPoint(f.db, points[2]); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.synthesis(f.home); r.stale != 1 {
		t.Fatal("not stale")
	}
	if r, _ := f.synthesis(other); r.stale != 0 {
		t.Fatal("other subject staled")
	}
	if got := f.due(); len(got) != 1 || got[0] != f.home {
		t.Fatal(got)
	}
}

func TestSynthesisDroppedWhenCitedPointForgottenDuringCall(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	run := func(prompt, system string, schema any) (Result, error) {
		if err := memory.ForgetPoint(f.db, points[0]); err != nil {
			t.Fatal(err)
		}
		return Result{Data: synthesisOf("ok", points[0], points[1])}, nil
	}
	if out := Synthesize(f.db, camille, f.home, run, now0); out.Status != Already {
		t.Fatalf("%+v", out)
	}
	if _, ok := f.synthesis(f.home); ok {
		t.Fatal("written")
	}
}

func (f *fixture) family() (parent, child string, childPoints []int64) {
	f.t.Helper()
	child, err := subjects.Create(f.db, "Sous-sujet", nil, &f.home)
	if err != nil {
		f.t.Fatal(err)
	}
	childPoints = f.backlog(child, 15)
	Synthesize(f.db, camille, child, newModel(synthesisOf("Enfant", childPoints[0], childPoints[1])).run, now0)
	return f.home, child, childPoints
}

func TestForgottenChildPointStalesAncestors(t *testing.T) {
	f := newFixture(t)
	grand := f.subject("Racine")
	f.exec("UPDATE subjects SET parent_uid = ? WHERE uid = ?", grand, f.home)
	parent, _, childPoints := f.family()
	points := f.backlog(parent, 15)
	for _, uid := range []string{parent, grand} {
		f.exec("INSERT INTO syntheses(subject_uid, uid, text, cited, created_at, updated_at) VALUES (?, ?, 'résumé', ?, ?, ?)",
			uid, "y-"+uid, fmt.Sprintf(`["sp-%d"]`, points[0]), t0, t0)
	}
	if err := memory.ForgetPoint(f.db, childPoints[0]); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{parent, grand} {
		if r, _ := f.synthesis(uid); r.stale != 1 {
			t.Fatal("ancestor not stale", uid)
		}
	}
}

func TestForgottenSessionStalesAncestors(t *testing.T) {
	f := newFixture(t)
	parent, _, childPoints := f.family()
	points := f.backlog(parent, 15)
	Synthesize(f.db, camille, parent, newModel(synthesisOf("Parent", points[0], points[1])).run, now0)
	f.exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, created_at, updated_at) VALUES ('e-oubli', 's9', ?, ?, ?, ?)", t0, t0, t0, t0)
	f.exec("UPDATE points SET episode_id = (SELECT id FROM episodes WHERE uid = 'e-oubli') WHERE id = ?", childPoints[2])
	if err := memory.ForgetSession(f.db, f.p, "s9"); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.synthesis(parent); r.stale != 1 {
		t.Fatal("parent not stale")
	}
}

func TestSynthesisDroppedWhenUncitedPointForgottenDuringCall(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	run := func(prompt, system string, schema any) (Result, error) {
		if err := memory.ForgetPoint(f.db, points[2]); err != nil {
			t.Fatal(err)
		}
		return Result{Data: synthesisOf("ok", points[0], points[1])}, nil
	}
	if out := Synthesize(f.db, camille, f.home, run, now0); out.Status != Already {
		t.Fatalf("%+v", out)
	}
	if _, ok := f.synthesis(f.home); ok {
		t.Fatal("written")
	}
}

func TestSynthesisDroppedWhenChildPointForgottenDuringCall(t *testing.T) {
	f := newFixture(t)
	parent, _, childPoints := f.family()
	points := f.backlog(parent, 15)
	run := func(prompt, system string, schema any) (Result, error) {
		if !strings.Contains(prompt, "Enfant") {
			t.Fatal("child synthesis not given", prompt)
		}
		if err := memory.ForgetPoint(f.db, childPoints[0]); err != nil {
			t.Fatal(err)
		}
		return Result{Data: synthesisOf("ok", points[0], points[1])}, nil
	}
	if out := Synthesize(f.db, camille, parent, run, now0); out.Status != Already {
		t.Fatalf("%+v", out)
	}
	if _, ok := f.synthesis(parent); ok {
		t.Fatal("written")
	}
}

func TestSynthesisDroppedWhenPreviousStalesDuringCall(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	Synthesize(f.db, camille, f.home, newModel(synthesisOf("Ancienne", points[0], points[1])).run, now0)
	child, err := subjects.Create(f.db, "Sous-sujet", nil, &f.home)
	if err != nil {
		t.Fatal(err)
	}
	childPoint := f.subjectPoint(child, 3, t0)
	run := func(prompt, system string, schema any) (Result, error) {
		if !strings.Contains(prompt, "Previous synthesis: Ancienne") {
			t.Fatal("previous synthesis not given", prompt)
		}
		if err := memory.ForgetPoint(f.db, childPoint); err != nil {
			t.Fatal(err)
		}
		return Result{Data: synthesisOf("Nouvelle", points[0], points[1])}, nil
	}
	if out := Synthesize(f.db, camille, f.home, run, now0); out.Status != Already {
		t.Fatalf("%+v", out)
	}
	if r, _ := f.synthesis(f.home); r.text != "Ancienne" || r.stale != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestSynthesisRunsWithSonnetMedium(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	dir := rawClaude(t, fmt.Sprintf(`{"structured_output":{"text":"Synthèse","cited":[%d,%d]},"total_cost_usd":0.03}`,
		points[0], points[1]), 0)
	outcomes, ok, err := RunCatchup(f.p, Options{All: true})
	if err != nil || !ok || len(outcomes) != 1 || outcomes[0].Status != Synthesized || outcomes[0].CostUSD != 0.03 {
		t.Fatalf("%+v %v", outcomes, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if got := string(args); !strings.Contains(got, "--model\x00sonnet\x00--effort\x00medium\x00") {
		t.Fatalf("%q", got)
	}
}

func TestCatchupSynthesizesAfterSummaries(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 15)
	f.seed("a", msg{longAgo, "user", "a"})
	var order []string
	useModel(t, func(prompt, system string, schema any) (Result, error) {
		order = append(order, "summary")
		return Result{Data: answer()}, nil
	})
	useSynthesisModel(t, func(prompt, system string, schema any) (Result, error) {
		order = append(order, "synthesis")
		return Result{Data: synthesisOf("ok", 1, 2)}, nil
	})
	outcomes, _, _ := RunCatchup(f.p, Options{All: true})
	if strings.Join(order, ",") != "summary,synthesis" || len(outcomes) != 2 || outcomes[1].Status != Synthesized {
		t.Fatal(order, outcomes)
	}
}

func TestSynthesisBacklogIgnoresStaleMarkMove(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 10)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at, written_at) VALUES (?, 'y', 't', ?, ?, ?)",
		f.home, t1, "2026-01-02T00:00:00.000Z", t1)
	f.backlog(f.home, 15)
	f.exec("UPDATE points SET created_at = '2026-01-01T11:00:00.000Z' WHERE id > 2")
	if got := f.due(); len(got) != 1 {
		t.Fatal("updated_at moved the boundary", got)
	}
}

func TestStaleSynthesisNotSentBack(t *testing.T) {
	f := newFixture(t)
	child, _ := subjects.Create(f.db, "Sous-sujet", nil, &f.home)
	points := f.backlog(f.home, 15)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, stale, created_at, updated_at) VALUES (?, 'y', 'cite le point oublié', 1, ?, ?)",
		f.home, longAgo, longAgo)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, stale, created_at, updated_at) VALUES (?, 'c', 'enfant périmé', 1, ?, ?)",
		child, longAgo, longAgo)
	m := newModel(synthesisOf("Neuve", points[0], points[1]))
	Synthesize(f.db, camille, f.home, m.run, now0)
	prompt := m.calls[0].prompt
	if strings.Contains(prompt, "oublié") || strings.Contains(prompt, "enfant périmé") ||
		!strings.Contains(prompt, "Previous synthesis: (none)") {
		t.Fatal(prompt)
	}
}

func TestForgottenSessionMarksCitingSynthesesStale(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 15)
	other := f.subject("Autre")
	otherPoints := f.backlog(other, 15)
	Synthesize(f.db, camille, f.home, newModel(synthesisOf("A", points[0], points[1])).run, now0)
	f.exec("UPDATE episodes SET session_id = 's9' WHERE id = (SELECT episode_id FROM points WHERE id = ?)", otherPoints[0])
	f.exec("UPDATE points SET episode_id = (SELECT episode_id FROM points WHERE id = ?) WHERE id = ?", otherPoints[0], points[0])
	f.exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, created_at, updated_at) VALUES ('e-reste', 's-reste', ?, ?, ?, ?)", t0, t0, t0, t0)
	f.exec("UPDATE points SET episode_id = (SELECT id FROM episodes WHERE uid = 'e-reste') WHERE id = ?", points[1])
	if err := memory.ForgetSession(f.db, f.p, "s9"); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.synthesis(f.home); r.stale != 1 {
		t.Fatal("not stale")
	}
}

func TestStaleSynthesisWithTooFewPointsDeleted(t *testing.T) {
	f := newFixture(t)
	points := f.backlog(f.home, 20)
	Synthesize(f.db, camille, f.home, newModel(synthesisOf("A", points[0], points[1])).run, now0)
	for _, id := range points[1:] {
		if err := memory.ForgetPoint(f.db, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runSyntheses(f.db, camille, now0); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.synthesis(f.home); ok {
		t.Fatal("kept")
	}
}

func TestSynthesisRetriedAfterCapOnNewPoint(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 15)
	f.exec("INSERT INTO summary_failures(session_id, attempts, last_at) VALUES (?, 3, ?)", "synthesis:"+f.home, "2026-01-01T12:00:00.000Z")
	if got := f.due(); len(got) != 0 {
		t.Fatal(got)
	}
	f.subjectPoint(f.home, 1, "2026-01-01T13:00:00.000Z")
	if got := f.due(); len(got) != 1 {
		t.Fatal(got)
	}
}

func TestSynthesisFailureLogNamesSubject(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 15)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	Synthesize(f.db, camille, f.home, newModel(&Error{msg: "x"}).run, now0)
	if got := buf.String(); !strings.Contains(got, "synthesis failed: "+f.home) || strings.Contains(got, "summary failed") {
		t.Fatal(got)
	}
}

func TestSynthesisReadErrorIsBusy(t *testing.T) {
	f := newFixture(t)
	f.backlog(f.home, 15)
	f.db.Close()
	m := newModel(&Error{msg: "x"})
	if out := Synthesize(f.db, camille, f.home, m.run, now0); out.Status != Busy || len(m.calls) != 0 {
		t.Fatalf("%+v", out)
	}
}
