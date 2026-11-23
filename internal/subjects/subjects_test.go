package subjects

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/paths"
)

const old = "2000-01-01T00:00:00.000Z"

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "t")}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func exec(t *testing.T, q db.Querier, query string, args ...any) {
	t.Helper()
	if _, err := q.ExecContext(bg, query, args...); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, q db.Querier, query string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(bg, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func create(t *testing.T, q db.Querier, name string, aliases ...string) string {
	t.Helper()
	uid, err := Create(q, name, aliases, nil)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func setParent(t *testing.T, q db.Querier, child, parent string, o Origin) {
	t.Helper()
	if err := SetParent(q, child, parent, o); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, q db.Querier, uid string) *Subject {
	t.Helper()
	s, err := Get(q, uid)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func parentOf(t *testing.T, q db.Querier, uid string) (parent, origin, episode sql.NullString) {
	t.Helper()
	err := q.QueryRowContext(bg, "SELECT parent_uid, parent_origin, parent_episode_uid FROM subjects WHERE uid = ?", uid).Scan(&parent, &origin, &episode)
	if err != nil {
		t.Fatal(err)
	}
	return parent, origin, episode
}

type linkRow struct {
	active  bool
	origin  string
	episode sql.NullString
}

func linkOf(t *testing.T, q db.Querier, source, typ, target string) (linkRow, bool) {
	t.Helper()
	var l linkRow
	err := q.QueryRowContext(bg, "SELECT active, origin, episode_uid FROM links WHERE source_uid = ? AND type = ? AND target_uid = ?",
		source, typ, target).Scan(&l.active, &l.origin, &l.episode)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return l, true
}

func ptr(s string) *string { return &s }

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Réseau maison":            "reseau maison",
		"réseau-maison":            "reseau maison",
		"  Fiche   Wikidata ! ":    "fiche wikidata",
		"L'Été à Noël":             "l ete a noel",
		"ÇA_VA, Ångström":          "ca va angstrom",
		"nas 2":                    "nas 2",
		"Démarche (administratif)": "demarche administratif",
		"cœur":                     "coeur",
		"Œuvre":                    "oeuvre",
		"Æther":                    "aether",
		"ﬁchier":                   "fichier",
		"ＮＡＳ":                      "nas",
		"Bureau 12 m²":             "bureau 12 m2",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateReturnsExistingByNormalizedName(t *testing.T) {
	d := openDB(t)
	network := create(t, d, "Réseau maison", "box")
	if again := create(t, d, "reseau-maison", "routeur"); again != network {
		t.Fatalf("namesake created: %s, want %s", again, network)
	}
	if other := create(t, d, "Fiche Wikidata"); other == network {
		t.Fatal("distinct subject merged by mistake")
	}
	tool, err := EnsureRepo(d, "github.com/exemple/atelier", "atelier")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := EnsureRepo(d, "github.com/exemple/atelier", "atelier"); again != tool {
		t.Fatalf("repository created twice: %s, %s", tool, again)
	}
	topic := create(t, d, "Atelier")
	if topic == tool {
		t.Fatal("subject mistaken for the namesake repository")
	}
	if again := create(t, d, "atelier"); again != topic {
		t.Fatalf("namesake of a repository created twice: %s, %s", topic, again)
	}
	if n := count(t, d, "SELECT count(*) FROM subjects"); n != 4 {
		t.Fatalf("%d subjects, want 4", n)
	}
	s := get(t, d, network)
	if s.Kind != "subject" || s.Name != "Réseau maison" || !slices.Equal(s.Aliases, []string{"box"}) {
		t.Fatalf("subject: %+v", s)
	}
	if _, err := Create(d, "Domotique", nil, ptr("inconnu")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown parent: %v", err)
	}
}

func TestSetParentRejectsCycle(t *testing.T) {
	d := openDB(t)
	home, network, nas := create(t, d, "Maison"), create(t, d, "Réseau"), create(t, d, "NAS")
	setParent(t, d, network, home, Origin{})
	setParent(t, d, nas, network, Origin{})
	for _, o := range []Origin{{}, {User: true}} {
		if err := SetParent(d, home, nas, o); !errors.Is(err, ErrCycle) {
			t.Fatalf("cycle accepted (%+v): %v", o, err)
		}
		if err := SetParent(d, home, home, o); !errors.Is(err, ErrCycle) {
			t.Fatalf("self as parent accepted (%+v): %v", o, err)
		}
	}
	if parent, _, _ := parentOf(t, d, home); parent.Valid {
		t.Fatalf("parent written despite the cycle: %s", parent.String)
	}
	if err := SetParent(d, nas, "inconnu", Origin{User: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown parent: %v", err)
	}
}

func TestModelCannotOverrideUserParent(t *testing.T) {
	d := openDB(t)
	home, office, nas := create(t, d, "Maison"), create(t, d, "Bureau"), create(t, d, "NAS")
	setParent(t, d, nas, home, Origin{User: true})
	if err := SetParent(d, nas, office, Origin{EpisodeUID: ptr("ep-1")}); !errors.Is(err, ErrUserSet) {
		t.Fatalf("user parent replaced by the model: %v", err)
	}
	setParent(t, d, nas, "", Origin{User: true})
	if err := SetParent(d, nas, office, Origin{EpisodeUID: ptr("ep-1")}); !errors.Is(err, ErrUserSet) {
		t.Fatalf("parent removed by the user restored by the model: %v", err)
	}
	if parent, origin, _ := parentOf(t, d, nas); parent.Valid || origin.String != "user" {
		t.Fatalf("parent %v, origin %v", parent, origin)
	}

	shelf := create(t, d, "Étagère")
	setParent(t, d, shelf, home, Origin{EpisodeUID: ptr("ep-1")})
	if err := SetParent(d, shelf, office, Origin{EpisodeUID: ptr("ep-2")}); !errors.Is(err, ErrSecondParent) {
		t.Fatalf("second parent from the model: %v", err)
	}
	setParent(t, d, shelf, home, Origin{EpisodeUID: ptr("ep-2")})
	if parent, _, episode := parentOf(t, d, shelf); parent.String != home || episode.String != "ep-1" {
		t.Fatalf("parent %v, episode %v", parent, episode)
	}
}

func TestUserReplacesModelParent(t *testing.T) {
	d := openDB(t)
	home, office, nas := create(t, d, "Maison"), create(t, d, "Bureau"), create(t, d, "NAS")
	setParent(t, d, nas, home, Origin{EpisodeUID: ptr("ep-1")})
	setParent(t, d, nas, office, Origin{User: true})
	parent, origin, episode := parentOf(t, d, nas)
	if parent.String != office || origin.String != "user" || episode.Valid {
		t.Fatalf("parent %v, origin %v, episode %v", parent, origin, episode)
	}
	setParent(t, d, nas, home, Origin{User: true})
	if parent, _, _ := parentOf(t, d, nas); parent.String != home {
		t.Fatalf("the user does not replace their own parent: %v", parent)
	}
}

func TestModelCannotRestoreUserRemovedLink(t *testing.T) {
	d := openDB(t)
	client, tool := create(t, d, "Client"), create(t, d, "Outil")
	if err := Link(d, client, "depends_on", tool, Origin{User: true}); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(d, client, "depends_on", tool, Origin{EpisodeUID: ptr("ep-1")}); !errors.Is(err, ErrUserSet) {
		t.Fatalf("user link removed by the model: %v", err)
	}
	if err := Unlink(d, client, "depends_on", tool, Origin{User: true}); err != nil {
		t.Fatal(err)
	}
	if err := Link(d, client, "depends_on", tool, Origin{EpisodeUID: ptr("ep-2")}); !errors.Is(err, ErrUserSet) {
		t.Fatalf("link removed by the user restored by the model: %v", err)
	}
	if l, _ := linkOf(t, d, client, "depends_on", tool); l.active || l.origin != "user" {
		t.Fatalf("link: %+v", l)
	}

	if err := Unlink(d, tool, "about", client, Origin{User: true}); err != nil {
		t.Fatal(err)
	}
	if err := Link(d, tool, "about", client, Origin{EpisodeUID: ptr("ep-3")}); !errors.Is(err, ErrUserSet) {
		t.Fatalf("link never set but refused by the user, restored by the model: %v", err)
	}
}

func TestModelLinkKeepsEpisode(t *testing.T) {
	d := openDB(t)
	client, tool := create(t, d, "Client"), create(t, d, "Outil")
	if err := Link(d, client, "depends_on", tool, Origin{EpisodeUID: ptr("ep-1")}); err != nil {
		t.Fatal(err)
	}
	l, ok := linkOf(t, d, client, "depends_on", tool)
	if !ok || !l.active || l.origin != "model" || l.episode.String != "ep-1" {
		t.Fatalf("link: %+v", l)
	}
	if err := Unlink(d, client, "depends_on", tool, Origin{EpisodeUID: ptr("ep-2")}); err != nil {
		t.Fatal(err)
	}
	if l, _ := linkOf(t, d, client, "depends_on", tool); l.active || l.episode.String != "ep-2" {
		t.Fatalf("link removed: %+v", l)
	}
	if err := Link(d, client, "depends_on", "inconnu", Origin{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cible inconnue : %v", err)
	}
}

func TestLinkRejectsUnknownType(t *testing.T) {
	d := openDB(t)
	a, b := create(t, d, "Client"), create(t, d, "Outil")
	for _, typ := range []string{"part_of", "uses", ""} {
		if err := Link(d, a, typ, b, Origin{User: true}); !errors.Is(err, ErrLinkType) {
			t.Fatalf("Link %q : %v", typ, err)
		}
		if err := Unlink(d, a, typ, b, Origin{User: true}); !errors.Is(err, ErrLinkType) {
			t.Fatalf("Unlink %q : %v", typ, err)
		}
	}
	if n := count(t, d, "SELECT count(*) FROM links"); n != 0 {
		t.Fatalf("%d links written", n)
	}
}

func TestLinkRejectsSelf(t *testing.T) {
	d := openDB(t)
	a := create(t, d, "Client")
	for _, write := range []func(db.Querier, string, string, string, Origin) error{Link, Unlink} {
		if err := write(d, a, "depends_on", a, Origin{User: true}); !errors.Is(err, ErrSelfLink) {
			t.Fatalf("link to itself: %v", err)
		}
	}
}

func TestModelRelinkWritesNothing(t *testing.T) {
	d := openDB(t)
	client, tool := create(t, d, "Client"), create(t, d, "Outil")
	for _, write := range []func(db.Querier, string, string, string, Origin) error{Link, Unlink} {
		if err := write(d, client, "depends_on", tool, Origin{EpisodeUID: ptr("ep-1")}); err != nil {
			t.Fatal(err)
		}
		exec(t, d, "UPDATE links SET updated_at = ?", old)
		if err := write(d, client, "depends_on", tool, Origin{EpisodeUID: ptr("ep-2")}); err != nil {
			t.Fatal(err)
		}
		if l, _ := linkOf(t, d, client, "depends_on", tool); l.episode.String != "ep-1" {
			t.Fatalf("episode rewritten: %+v", l)
		}
		if n := count(t, d, "SELECT count(*) FROM links WHERE updated_at = ?", old); n != 1 {
			t.Fatal("unchanged link rewritten")
		}
	}
}

func TestModelCannotRemoveParent(t *testing.T) {
	d := openDB(t)
	home, nas, rack := create(t, d, "Maison"), create(t, d, "NAS"), create(t, d, "Baie")
	setParent(t, d, nas, home, Origin{EpisodeUID: ptr("ep-1")})
	for _, child := range []string{nas, rack} {
		if err := SetParent(d, child, "", Origin{EpisodeUID: ptr("ep-2")}); !errors.Is(err, ErrRemoveParent) {
			t.Fatalf("parent removal by the model: %v", err)
		}
	}
	if parent, _, _ := parentOf(t, d, nas); parent.String != home {
		t.Fatalf("parent removed: %v", parent)
	}
}

type mergeFixture struct {
	from, into, parent, child, other, watcher string
}

func newMergeFixture(t *testing.T, q db.Querier) mergeFixture {
	t.Helper()
	f := mergeFixture{
		parent:  create(t, q, "Maison"),
		into:    create(t, q, "Réseau", "lan"),
		child:   create(t, q, "Routeur"),
		other:   create(t, q, "Outil"),
		watcher: create(t, q, "Démarche"),
	}
	from, err := EnsureRepo(q, "github.com/exemple/reseau-maison", "reseau-maison")
	if err != nil {
		t.Fatal(err)
	}
	f.from = from
	if err := AddAliases(q, from, []string{"box", "LAN"}); err != nil {
		t.Fatal(err)
	}
	setParent(t, q, from, f.parent, Origin{EpisodeUID: ptr("ep-0")})
	setParent(t, q, f.child, from, Origin{User: true})

	exec(t, q, "INSERT INTO episodes(id, session_id, start_ts, end_ts, created_at, uid, updated_at) VALUES (1, 's1', 'a', 'b', ?, 'ep-1', ?), (2, 's2', 'a', 'b', ?, 'ep-2', ?)", old, old, old, old)
	exec(t, q, "INSERT INTO points(episode_id, kind, subject_uid, text, importance, source_ts, created_at, uid, updated_at) VALUES (1, 'fact', ?, 'un fait', 3, 'a', ?, 'p-1', ?), (2, 'fact', ?, 'un autre', 3, 'a', ?, 'p-2', ?)",
		from, old, old, f.into, old, old)
	exec(t, q, "INSERT INTO episode_subjects(episode_uid, subject_uid) VALUES ('ep-1', ?), ('ep-1', ?), ('ep-2', ?)", from, f.into, from)
	exec(t, q, "INSERT INTO session_repos(session_id, subject_uid) VALUES ('s1', ?), ('s1', ?), ('s2', ?)", from, f.into, from)
	exec(t, q, "INSERT INTO injected(session_id, subject_uid) VALUES ('s1', ?)", from)
	exec(t, q, "INSERT INTO repo_cache(path, subject_uid, checked_at) VALUES ('/x', ?, ?)", from, old)

	for _, l := range []struct {
		source, typ, target string
		o                   Origin
		unlink              bool
	}{
		{from, "depends_on", f.other, Origin{EpisodeUID: ptr("ep-1")}, false},
		{f.into, "depends_on", f.other, Origin{User: true}, true},
		{f.watcher, "about", from, Origin{EpisodeUID: ptr("ep-1")}, false},
		{f.into, "about", f.watcher, Origin{EpisodeUID: ptr("ep-2")}, false},
		{from, "about", f.watcher, Origin{User: true}, false},
		{from, "depends_on", f.into, Origin{EpisodeUID: ptr("ep-1")}, false},
	} {
		write := Link
		if l.unlink {
			write = Unlink
		}
		if err := write(q, l.source, l.typ, l.target, l.o); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, q, "UPDATE subjects SET updated_at = ?", old)
	exec(t, q, "UPDATE links SET updated_at = ?", old)
	return f
}

func TestMergeMovesEverythingAndLeavesTrace(t *testing.T) {
	d := openDB(t)
	f := newMergeFixture(t, d)
	if err := Merge(d, f.from, f.into); err != nil {
		t.Fatal(err)
	}

	if _, err := Get(d, f.from); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merged subject still present: %v", err)
	}
	var into string
	if err := d.QueryRow("SELECT into_uid FROM merged WHERE from_uid = ?", f.from).Scan(&into); err != nil || into != f.into {
		t.Fatalf("merge trace: %q, %v", into, err)
	}
	s := get(t, d, f.into)
	if s.Name != "Réseau" || s.Kind != "repo" || s.Repo == nil || *s.Repo != "github.com/exemple/reseau-maison" {
		t.Fatalf("survivor: %+v", s)
	}
	if !slices.Equal(s.Aliases, []string{"lan", "box", "reseau-maison"}) {
		t.Fatalf("keywords: %q", s.Aliases)
	}
	if parent, origin, episode := parentOf(t, d, f.into); parent.String != f.parent || origin.String != "model" || episode.String != "ep-0" {
		t.Fatalf("parent taken over: %v %v %v", parent, origin, episode)
	}
	if parent, origin, _ := parentOf(t, d, f.child); parent.String != f.into || origin.String != "user" {
		t.Fatalf("enfant : %v %v", parent, origin)
	}

	for query, want := range map[string]int{
		"SELECT count(*) FROM points WHERE subject_uid = ?1":                  2,
		"SELECT count(*) FROM episode_subjects WHERE subject_uid = ?1":        2,
		"SELECT count(*) FROM session_repos WHERE subject_uid = ?1":           2,
		"SELECT count(*) FROM injected WHERE subject_uid = ?1":                1,
		"SELECT count(*) FROM repo_cache WHERE subject_uid = ?1":              1,
		"SELECT count(*) FROM links WHERE source_uid = ?1 OR target_uid = ?1": 3,
		"SELECT count(*) FROM points WHERE updated_at = ?2":                   1,
		"SELECT count(*) FROM episodes WHERE updated_at = ?2":                 0,
	} {
		if n := count(t, d, query, f.into, old); n != want {
			t.Errorf("%s: %d, want %d", query, n, want)
		}
	}
	for _, table := range []string{"points", "episode_subjects", "session_repos", "injected", "repo_cache"} {
		if n := count(t, d, "SELECT count(*) FROM "+table+" WHERE subject_uid = ?", f.from); n != 0 {
			t.Errorf("%s still points at the merged subject: %d", table, n)
		}
	}
	if n := count(t, d, "SELECT count(*) FROM links WHERE source_uid = ?1 OR target_uid = ?1", f.from); n != 0 {
		t.Errorf("links still pointing at the merged subject: %d", n)
	}
	if l, _ := linkOf(t, d, f.into, "depends_on", f.other); l.active || l.origin != "user" {
		t.Errorf("the user removal must win over the model link: %+v", l)
	}
	if l, ok := linkOf(t, d, f.watcher, "about", f.into); !ok || !l.active || l.episode.String != "ep-1" {
		t.Errorf("incoming link carried over: %+v %v", l, ok)
	}
	if l, _ := linkOf(t, d, f.into, "about", f.watcher); !l.active || l.origin != "user" {
		t.Errorf("the user link must win over the model one: %+v", l)
	}
	if _, ok := linkOf(t, d, f.into, "depends_on", f.into); ok {
		t.Error("link to itself created by the merge")
	}
	if n := count(t, d, "SELECT count(*) FROM links WHERE updated_at = ? AND NOT (source_uid = ? AND target_uid = ?)", old, f.into, f.other); n != 0 {
		t.Errorf("%d carried-over links without updated_at", n)
	}
	if n := count(t, d, "SELECT count(*) FROM subjects WHERE uid IN (?, ?) AND updated_at = ?", f.into, f.child, old); n != 0 {
		t.Errorf("%d modified subjects without updated_at", n)
	}

	if err := Merge(d, f.into, f.into); !errors.Is(err, ErrSelfMerge) {
		t.Fatalf("merge with itself: %v", err)
	}
	if err := Merge(d, f.from, f.into); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge of a vanished subject: %v", err)
	}
}

func synthesis(t *testing.T, q db.Querier, subject, text string) {
	t.Helper()
	exec(t, q, "INSERT INTO syntheses(subject_uid, uid, text, cited, created_at, updated_at) VALUES (?, ?, ?, '[]', ?, ?)",
		subject, "syn-"+text, text, old, old)
}

func TestMergeMovesSynthesis(t *testing.T) {
	d := openDB(t)
	f := newMergeFixture(t, d)
	synthesis(t, d, f.from, "du dépôt")
	if err := Merge(d, f.from, f.into); err != nil {
		t.Fatal(err)
	}
	var text string
	var stale int
	if err := d.QueryRow("SELECT text, stale FROM syntheses WHERE subject_uid = ?", f.into).Scan(&text, &stale); err != nil || text != "du dépôt" || stale != 0 {
		t.Fatalf("%q %d %v", text, stale, err)
	}
	if n := count(t, d, "SELECT count(*) FROM syntheses WHERE subject_uid = ?", f.from); n != 0 {
		t.Fatalf("synthesis left on the merged subject: %d", n)
	}
}

func TestMergeKeepsSurvivorSynthesisAndMarksItStale(t *testing.T) {
	d := openDB(t)
	f := newMergeFixture(t, d)
	synthesis(t, d, f.from, "du dépôt")
	synthesis(t, d, f.into, "du sujet")
	if err := Merge(d, f.from, f.into); err != nil {
		t.Fatal(err)
	}
	var text string
	var stale int
	if err := d.QueryRow("SELECT text, stale FROM syntheses WHERE subject_uid = ?", f.into).Scan(&text, &stale); err != nil || text != "du sujet" || stale != 1 {
		t.Fatalf("%q %d %v", text, stale, err)
	}
	if n := count(t, d, "SELECT count(*) FROM syntheses"); n != 1 {
		t.Fatalf("%d syntheses", n)
	}
}

func TestMergeIsAtomic(t *testing.T) {
	d := openDB(t)
	f := newMergeFixture(t, d)
	exec(t, d, "CREATE TRIGGER refuse BEFORE INSERT ON merged BEGIN SELECT RAISE(ABORT, 'refusé'); END")
	if err := Merge(d, f.from, f.into); err == nil {
		t.Fatal("merge succeeded despite the trace failure")
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Merge(tx, f.from, f.into); err == nil {
		t.Fatal("merge succeeded despite the trace failure (transaction)")
	}
	if n := count(t, tx, "SELECT count(*) FROM points WHERE subject_uid = ?", f.from); n != 1 {
		t.Fatalf("points moved despite the failure: %d remaining", n)
	}
	get(t, tx, f.from)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	get(t, d, f.from)
	if n := count(t, d, "SELECT count(*) FROM links WHERE source_uid = ?1 OR target_uid = ?1", f.from); n != 4 {
		t.Fatalf("links lost despite the failure: %d", n)
	}
}

func TestMergeIntoDescendantBreaksCycleOnModelEdge(t *testing.T) {
	for _, c := range []struct {
		what, from string
		users      []string
		want       map[string]string
	}{
		{"survivor edge from the model", "Maison", []string{"Baie", "Réseau"}, map[string]string{"NAS": "Quartier", "Réseau": "NAS", "Baie": "Réseau"}},
		{"model edge above the survivor", "Maison", []string{"NAS", "Réseau"}, map[string]string{"NAS": "Baie", "Baie": "", "Réseau": "NAS"}},
		{"only user edges", "Maison", []string{"NAS", "Baie", "Réseau", "Maison"}, map[string]string{"NAS": "Baie", "Baie": "Réseau", "Réseau": ""}},
		{"survivor right under", "Baie", []string{"NAS"}, map[string]string{"NAS": "Réseau"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			d := openDB(t)
			uid := map[string]string{"": ""}
			chain := []string{"Quartier", "Maison", "Réseau", "Baie", "NAS"}
			for i, name := range chain {
				uid[name] = create(t, d, name)
				if i > 0 {
					setParent(t, d, uid[name], uid[chain[i-1]], Origin{User: slices.Contains(c.users, name)})
				}
			}
			if err := Merge(d, uid[c.from], uid["NAS"]); err != nil {
				t.Fatal(err)
			}
			for child, want := range c.want {
				if parent, _, _ := parentOf(t, d, uid[child]); parent.String != uid[want] {
					t.Errorf("parent of %s: %q, want %q (%s)", child, parent.String, uid[want], want)
				}
			}
			if _, origin, _ := parentOf(t, d, uid["NAS"]); slices.Contains(c.users, "NAS") && c.from != "Baie" && origin.String != "user" {
				t.Errorf("origin of the survivor's parent: %q", origin.String)
			}
		})
	}
}

func TestMergeKeepsSurvivorParent(t *testing.T) {
	d := openDB(t)
	home, office, nas, rack := create(t, d, "Maison"), create(t, d, "Bureau"), create(t, d, "NAS"), create(t, d, "Baie")
	setParent(t, d, nas, home, Origin{User: true})
	setParent(t, d, rack, office, Origin{})
	if err := Merge(d, rack, nas); err != nil {
		t.Fatal(err)
	}
	if parent, origin, _ := parentOf(t, d, nas); parent.String != home || origin.String != "user" {
		t.Fatalf("user parent of the survivor replaced: %v %v", parent, origin)
	}
}

func treeSize(nodes []Node) int {
	n := len(nodes)
	for _, node := range nodes {
		n += treeSize(node.Children)
	}
	return n
}

func TestMergeBreaksCycleFromSync(t *testing.T) {
	for _, size := range []int{2, 3} {
		d := openDB(t)
		ring := []string{}
		for i := range size {
			ring = append(ring, create(t, d, fmt.Sprintf("Sujet %d", i)))
		}
		for i, uid := range ring {
			exec(t, d, "UPDATE subjects SET parent_uid = ?, parent_origin = 'model' WHERE uid = ?", ring[(i+1)%size], uid)
		}
		if err := Merge(d, ring[0], ring[size-1]); err != nil {
			t.Fatal(err)
		}
		if n := count(t, d, "SELECT count(*) FROM subjects WHERE parent_uid = uid"); n != 0 {
			t.Fatalf("cycle of %d: %d subjects that are their own parents", size, n)
		}
		tree, err := Tree(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(tree) != 1 || treeSize(tree) != size-1 {
			t.Fatalf("cycle of %d: %d roots, %d subjects in the tree", size, len(tree), treeSize(tree))
		}
	}
}

func TestMergeIntoAncestorKeepsTree(t *testing.T) {
	d := openDB(t)
	office, desk, lamp := create(t, d, "Bureau"), create(t, d, "Table"), create(t, d, "Lampe")
	setParent(t, d, desk, office, Origin{})
	setParent(t, d, lamp, desk, Origin{})
	if err := Merge(d, desk, office); err != nil {
		t.Fatal(err)
	}
	if parent, _, _ := parentOf(t, d, office); parent.Valid {
		t.Fatalf("the ancestor took a parent in its own subtree: %s", parent.String)
	}
	if parent, _, _ := parentOf(t, d, lamp); parent.String != office {
		t.Fatalf("petit-enfant : %v", parent)
	}
}

func TestMergeDuplicatesByNameAndRepo(t *testing.T) {
	d := openDB(t)
	network := []string{create(t, d, "Réseau maison")}
	for _, name := range []string{"reseau-maison", "RÉSEAU  MAISON"} {
		uid := db.NewUID()
		exec(t, d, "INSERT INTO subjects(uid, kind, name, aliases, created_at, updated_at) VALUES (?, 'subject', ?, '[]', ?, ?)", uid, name, old, old)
		network = append(network, uid)
	}
	exec(t, d, "INSERT INTO episodes(id, session_id, start_ts, end_ts, created_at, uid, updated_at) VALUES (1, 's1', 'a', 'b', 'a', 'ep-1', 'a')")
	exec(t, d, "INSERT INTO points(episode_id, kind, subject_uid, text, importance, source_ts, created_at, uid, updated_at) SELECT 1, 'fact', uid, 'x', 1, 'a', 'a', uid, 'a' FROM subjects")
	tool, err := EnsureRepo(d, "github.com/exemple/atelier", "atelier")
	if err != nil {
		t.Fatal(err)
	}
	toolTopic := db.NewUID()
	exec(t, d, "INSERT INTO subjects(uid, kind, name, aliases, created_at, updated_at) VALUES (?, 'subject', 'Atelier', '[]', ?, ?)", toolTopic, old, old)
	fork1, _ := EnsureRepo(d, "github.com/exemple/outil", "outil")
	fork2, _ := EnsureRepo(d, "gitlab.com/autre/outil", "outil")

	n, err := MergeDuplicates(d)
	if err != nil || n != 2 {
		t.Fatalf("MergeDuplicates = %d, %v; want 2", n, err)
	}
	survivor := slices.Min(network)
	if s := count(t, d, "SELECT count(*) FROM subjects WHERE uid IN (?, ?, ?)", network[0], network[1], network[2]); s != 1 {
		t.Fatalf("%d network subjects remaining", s)
	}
	get(t, d, survivor)
	if n := count(t, d, "SELECT count(*) FROM points WHERE subject_uid = ?", survivor); n != 3 {
		t.Fatalf("%d points on the survivor", n)
	}
	for _, uid := range []string{tool, toolTopic, fork1, fork2} {
		get(t, d, uid)
	}
	if n := count(t, d, "SELECT count(*) FROM merged"); n != 2 {
		t.Fatalf("%d merge traces", n)
	}
	if again, err := EnsureRepo(d, "github.com/exemple/atelier", "atelier"); err != nil || again != tool {
		t.Fatalf("repository found after merge: %s, %v", again, err)
	}

	if n, err := MergeDuplicates(d); err != nil || n != 0 {
		t.Fatalf("second pass: %d, %v", n, err)
	}
}

func TestMergeDuplicatesSkipsVanishedSubjects(t *testing.T) {
	d := openDB(t)
	for _, name := range []string{"Réseau maison", "reseau-maison", "RÉSEAU MAISON", "Fiche", "fiche"} {
		exec(t, d, "INSERT INTO subjects(uid, kind, name, aliases, created_at, updated_at) VALUES (?, 'subject', ?, '[]', ?, ?)", db.NewUID(), name, old, old)
	}
	stale, err := query(d, "WHERE kind = 'subject' ORDER BY uid")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := mergeDuplicates(d, stale); err != nil || n != 3 {
		t.Fatalf("first pass: %d, %v", n, err)
	}
	if n, err := mergeDuplicates(d, stale); err != nil || n != 0 {
		t.Fatalf("second pass on a stale list: %d, %v", n, err)
	}
}

func TestDescendants(t *testing.T) {
	d := openDB(t)
	home, network, nas, router, office := create(t, d, "Maison"), create(t, d, "Réseau"), create(t, d, "NAS"), create(t, d, "Routeur"), create(t, d, "Bureau")
	setParent(t, d, network, home, Origin{})
	setParent(t, d, nas, network, Origin{})
	setParent(t, d, router, network, Origin{})
	got, err := Descendants(d, home)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{network, nas, router}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Descendants = %v, want %v", got, want)
	}
	if got, _ := Descendants(d, office); len(got) != 0 {
		t.Fatalf("feuille : %v", got)
	}
}

func TestTreeAndNeighbors(t *testing.T) {
	d := openDB(t)
	home, network, nas, client := create(t, d, "Maison"), create(t, d, "Réseau"), create(t, d, "NAS"), create(t, d, "Client")
	setParent(t, d, network, home, Origin{})
	setParent(t, d, nas, network, Origin{})
	for _, typ := range []string{"depends_on", "about"} {
		if err := Link(d, nas, typ, client, Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Link(d, nas, "depends_on", home, Origin{}); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(d, nas, "depends_on", home, Origin{User: true}); err != nil {
		t.Fatal(err)
	}

	parent, deps, err := Neighbors(d, nas)
	if err != nil {
		t.Fatal(err)
	}
	if parent == nil || parent.UID != network || len(deps) != 1 || deps[0].UID != client {
		t.Fatalf("voisins : %+v %+v", parent, deps)
	}
	if parent, _, _ := Neighbors(d, home); parent != nil {
		t.Fatalf("root with a parent: %+v", parent)
	}

	tree, err := Tree(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree[0].Name != "Client" || tree[1].Name != "Maison" {
		t.Fatalf("roots: %+v", tree)
	}
	leaf := tree[1].Children[0].Children[0]
	if leaf.UID != nas || !slices.Equal(leaf.Links, []Edge{{"about", client}, {"depends_on", client}}) {
		t.Fatalf("feuille : %+v", leaf)
	}
}

func TestFindByTermAmbiguous(t *testing.T) {
	d := openDB(t)
	home := create(t, d, "Maison", "nas")
	office := create(t, d, "Bureau", "NAS")
	create(t, d, "Réseau")
	found, err := FindByTerm(d, "Nas")
	if err != nil {
		t.Fatal(err)
	}
	uids := []string{}
	for _, s := range found {
		uids = append(uids, s.UID)
	}
	want := []string{home, office}
	slices.Sort(uids)
	slices.Sort(want)
	if !slices.Equal(uids, want) {
		t.Fatalf("FindByTerm = %v, want %v", uids, want)
	}
	for _, term := range []string{"réseau", "RESEAU", home} {
		if found, _ := FindByTerm(d, term); len(found) != 1 {
			t.Fatalf("FindByTerm(%q) : %+v", term, found)
		}
	}
	if found, _ := FindByTerm(d, "garage"); len(found) != 0 {
		t.Fatalf("terme absent : %+v", found)
	}
}

func TestWritesBumpUpdatedAt(t *testing.T) {
	d := openDB(t)
	a, b, c := create(t, d, "Client"), create(t, d, "Outil"), create(t, d, "Maison")
	stale := func() {
		exec(t, d, "UPDATE subjects SET updated_at = ?", old)
		exec(t, d, "UPDATE links SET updated_at = ?", old)
	}
	bumped := func(what, table, where string, args ...any) {
		t.Helper()
		if n := count(t, d, "SELECT count(*) FROM "+table+" WHERE updated_at = '"+old+"' AND "+where, args...); n != 0 {
			t.Errorf("%s: updated_at unchanged", what)
		}
	}
	for _, step := range []struct {
		what, table, where string
		args               []any
		write              func() error
	}{
		{"Rename", "subjects", "uid = ?", []any{a}, func() error { return Rename(d, a, "Client web") }},
		{"AddAliases", "subjects", "uid = ?", []any{a}, func() error { return AddAliases(d, a, []string{"front"}) }},
		{"SetParent", "subjects", "uid = ?", []any{a}, func() error { return SetParent(d, a, c, Origin{}) }},
		{"Link", "links", "1", nil, func() error { return Link(d, a, "depends_on", b, Origin{}) }},
		{"Unlink", "links", "1", nil, func() error { return Unlink(d, a, "depends_on", b, Origin{User: true}) }},
		{"Merge", "subjects", "uid = ?", []any{b}, func() error { return Merge(d, c, b) }},
	} {
		stale()
		if err := step.write(); err != nil {
			t.Fatalf("%s : %v", step.what, err)
		}
		bumped(step.what, step.table, step.where, step.args...)
	}
	stale()
	if err := AddAliases(d, a, []string{"FRONT", "client web"}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM subjects WHERE uid = ? AND updated_at = ?", a, old); n != 1 {
		t.Error("AddAliases without a new keyword rewrote the subject")
	}
}

func termRows(t *testing.T, q db.Querier) []string {
	t.Helper()
	rows, err := q.QueryContext(bg, "SELECT term || '|' || subject_uid FROM subject_terms ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestTermsFollowEveryWrite(t *testing.T) {
	d := openDB(t)
	a := create(t, d, "Épsilon Lab", "elab")
	b := create(t, d, "Gamma")
	repo, err := EnsureRepo(d, "example/zeta", "zeta")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename(d, a, "Éta Lab"); err != nil {
		t.Fatal(err)
	}
	if err := AddAliases(d, b, []string{"Γ tool", "gamma-cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRepo(d, "example/zeta", "zeta"); err != nil {
		t.Fatal(err)
	}
	check := func(step string) {
		t.Helper()
		got := termRows(t, d)
		if err := RebuildTerms(d); err != nil {
			t.Fatal(err)
		}
		if want := termRows(t, d); !slices.Equal(got, want) {
			t.Fatalf("%s: maintained %v, rebuilt %v", step, got, want)
		}
	}
	check("writes")
	if err := Merge(d, b, repo); err != nil {
		t.Fatal(err)
	}
	check("merge")
	if n := count(t, d, "SELECT count(*) FROM subject_terms WHERE subject_uid = ?", b); n != 0 {
		t.Fatalf("merged subject keeps %d terms", n)
	}
	found, err := FindByTerm(d, "GAMMA CLI")
	if err != nil || len(found) != 1 || found[0].UID != repo {
		t.Fatalf("found %v, %v", found, err)
	}
}

func TestByTermsIsOneIndexedLookup(t *testing.T) {
	d := openDB(t)
	a := create(t, d, "Paul")
	b := create(t, d, "Paul Two", "paul")
	got, err := ByTerms(d, []string{"paul", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["paul"]) != 2 || len(got["absent"]) != 0 || got["paul"][0].UID == got["paul"][1].UID {
		t.Fatalf("got %v (%s, %s)", got, a, b)
	}
}
