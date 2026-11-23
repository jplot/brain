package projects

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/paths"
)

var bg = context.Background()

var labels = map[string]string{"mon-compte": "personal", "mon-employeur": "work"}

type fixture struct {
	t    *testing.T
	db   *sql.DB
	root string
	cfg  config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "t")}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	root := filepath.Join(dir, "code")
	mkdir(t, root)
	return &fixture{t: t, db: d, root: root, cfg: config.Config{Roots: []string{root}, Labels: labels}}
}

func (f *fixture) resolve(cwd string) string {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	id, err := Resolve(bg, tx, f.cfg, cwd)
	if err != nil {
		tx.Rollback()
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) label(id string) [2]string {
	f.t.Helper()
	var label, by string
	if err := f.db.QueryRow("SELECT label, label_by FROM projects WHERE id = ?", id).Scan(&label, &by); err != nil {
		f.t.Fatal(err)
	}
	return [2]string{label, by}
}

func (f *fixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) three() (string, string, string) {
	a, b, c := f.resolve(filepath.Join(f.root, "a")), f.resolve(filepath.Join(f.root, "b")), f.resolve(filepath.Join(f.root, "c"))
	return a, b, c
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v : %v\n%s", args, err, out)
	}
}

func makeRepo(t *testing.T, path string) string {
	t.Helper()
	mkdir(t, filepath.Join(path, "src"))
	runGit(t, path, "init", "-q")
	return path
}

func makeRepoWithRemote(t *testing.T, path, remote string) string {
	t.Helper()
	makeRepo(t, path)
	runGit(t, path, "remote", "add", "origin", remote)
	return path
}

func addWorktree(t *testing.T, repo, path string) string {
	t.Helper()
	runGit(t, repo, "worktree", "add", "-q", "--orphan", "-b", filepath.Base(path), path)
	return path
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeRemotes(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:Owner/Repo.git",
		"https://github.com/owner/repo",
		"ssh://git@github.com/owner/repo.git",
		"GitHub.com/Owner/Repo",
		"https://github.com/owner/repo.git/",
		"ssh://git@github.com:22/owner/repo.git",
		"https://user:secret@github.com/owner/repo.git",
		" git@github.com:owner/repo.git\n",
	} {
		got, ok := Normalize(remote)
		if !ok || got != "github.com/owner/repo" {
			t.Errorf("Normalize(%q) = %q, %v", remote, got, ok)
		}
	}
	got, ok := Normalize("git@gitlab.com:Group/Sub/Repo.git")
	if !ok || got != "gitlab.com/group/sub/repo" {
		t.Errorf("subgroup: %q, %v", got, ok)
	}
	for _, remote := range []string{"", "pas une url", "/srv/git/repo.git", "../repo", "file:///srv/git/repo.git", "https://github.com/owner", "git@github.com:repo.git"} {
		if got, ok := Normalize(remote); ok {
			t.Errorf("Normalize(%q) = %q, want rejection", remote, got)
		}
	}
}

func TestOwnerLabel(t *testing.T) {
	f := newFixture(t)
	for remote, want := range map[string]string{
		"git@github.com:mon-employeur/x.git":      "work",
		"https://github.com/Mon-Compte/y":         "personal",
		"ssh://git@github.com/exemple/z.git":      "unsorted",
		"ssh://git@gitlab.com/mon-compte/a/b.git": "personal",
	} {
		repo := makeRepoWithRemote(t, filepath.Join(f.root, strings.ReplaceAll(remote, "/", "_")), remote)
		if got := f.label(f.resolve(repo)); got != [2]string{want, "rule"} {
			t.Errorf("%s: %v", remote, got)
		}
	}
}

func TestSubfolderAndWorktree(t *testing.T) {
	f := newFixture(t)
	repo := makeRepo(t, filepath.Join(f.root, "atelier"))
	expect(t, f.resolve(filepath.Join(repo, "src")), "dir:atelier")
	wt := addWorktree(t, repo, filepath.Join(f.root, "worktrees", "atelier-branche"))
	expect(t, f.resolve(wt), "dir:atelier")
	expect(t, f.label("dir:atelier")[0], "unsorted")
}

func TestWorktreeJoinsItsRepository(t *testing.T) {
	f := newFixture(t)
	repo := makeRepoWithRemote(t, filepath.Join(f.root, "atelier"), "git@github.com:mon-compte/atelier.git")
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "ailleurs"))
	expect(t, f.resolve(wt), "github.com/mon-compte/atelier")
	expect(t, f.resolve(repo), "github.com/mon-compte/atelier")
	if n := f.count("projects"); n != 1 {
		t.Fatalf("%d projects", n)
	}
}

func TestDeletedWorktreeJoinsLongestPrefix(t *testing.T) {
	f := newFixture(t)
	mkdir(t, filepath.Join(f.root, "atelier"))
	mkdir(t, filepath.Join(f.root, "atelier-web"))
	expect(t, f.resolve(filepath.Join(f.root, "worktrees", "atelier-web-fix")), "dir:atelier-web")
}

func TestDeletedWorktreeOfRepoWithRemote(t *testing.T) {
	f := newFixture(t)
	makeRepoWithRemote(t, filepath.Join(f.root, "atelier"), "git@github.com:mon-compte/atelier.git")
	expect(t, f.resolve(filepath.Join(f.root, "worktrees", "atelier-fix")), "github.com/mon-compte/atelier")
}

func TestWorktreeWithoutProjectKeepsItsPath(t *testing.T) {
	f := newFixture(t)
	expect(t, f.resolve(filepath.Join(f.root, "worktrees", "inconnu-fix")), "dir:worktrees/inconnu-fix")
}

func TestDeletedProjectIsFrozen(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(f.root, "ancien", "src")
	expect(t, f.resolve(gone), "dir:ancien")
	var id string
	if err := f.db.QueryRow("SELECT project_id FROM project_paths WHERE path = ?", gone).Scan(&id); err != nil {
		t.Fatal(err)
	}
	expect(t, id, "dir:ancien")
}

func TestPathIdentityOutsideRoots(t *testing.T) {
	f := newFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "ailleurs", "x")
	expect(t, f.resolve(elsewhere), "path:"+elsewhere)
	expect(t, f.label("path:" + elsewhere)[0], "unsorted")
}

func TestDirIdentitySameUnderTwoRoots(t *testing.T) {
	f := newFixture(t)
	other := mkdir(t, filepath.Join(t.TempDir(), "autre"))
	f.cfg.Roots = append(f.cfg.Roots, other)
	expect(t, f.resolve(mkdir(t, filepath.Join(f.root, "notes"))), "dir:notes")
	expect(t, f.resolve(mkdir(t, filepath.Join(other, "notes", "2026"))), "dir:notes")
	if n := f.count("projects"); n != 1 {
		t.Fatalf("%d projects", n)
	}
}

func TestRootsAreOneProject(t *testing.T) {
	f := newFixture(t)
	other := mkdir(t, filepath.Join(t.TempDir(), "autre"))
	f.cfg.Roots = append(f.cfg.Roots, other)
	expect(t, f.resolve(f.root), Roots)
	expect(t, f.resolve(other), Roots)
	if got := f.label(Roots); got != [2]string{"unsorted", "rule"} {
		t.Fatal(got)
	}
	if !errors.Is(SetLabel(f.db, Roots, "personal"), ErrNotClassifiable) {
		t.Fatal("roots classified")
	}
}

func TestRootReachedThroughSymlink(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(t.TempDir(), "lien")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	f.cfg.Roots = []string{link}
	makeRepo(t, filepath.Join(f.root, "atelier"))
	expect(t, f.resolve(filepath.Join(link, "atelier", "src")), "dir:atelier")
}

func TestBareRepoWorktreeIsItsOwnProject(t *testing.T) {
	f := newFixture(t)
	bare := mkdir(t, filepath.Join(f.root, "outil.git"))
	runGit(t, bare, "init", "-q", "--bare")
	wt := addWorktree(t, bare, filepath.Join(f.root, "worktrees", "outil-b"))
	expect(t, f.resolve(wt), "dir:worktrees/outil-b")
}

func TestSeparateGitDirIsItsOwnProject(t *testing.T) {
	f := newFixture(t)
	super := makeRepo(t, filepath.Join(f.root, "super"))
	mkdir(t, filepath.Join(super, ".git", "modules"))
	runGit(t, super, "init", "-q", "--separate-git-dir="+filepath.Join(super, ".git", "modules", "sub"), filepath.Join(super, "sub"))
	expect(t, f.resolve(filepath.Join(super, "sub")), "dir:super/sub")
}

func TestRemoteInsteadOfIsApplied(t *testing.T) {
	f := newFixture(t)
	repo := makeRepoWithRemote(t, filepath.Join(f.root, "atelier"), "gh:mon-compte/atelier")
	runGit(t, repo, "config", "url.git@github.com:.insteadOf", "gh:")
	expect(t, f.resolve(repo), "github.com/mon-compte/atelier")
}

func TestIdentityFrozenAfterRemoteRename(t *testing.T) {
	f := newFixture(t)
	repo := makeRepoWithRemote(t, filepath.Join(f.root, "atelier"), "git@github.com:mon-compte/atelier.git")
	expect(t, f.resolve(repo), "github.com/mon-compte/atelier")
	runGit(t, repo, "remote", "set-url", "origin", "git@github.com:exemple/autre.git")
	expect(t, f.resolve(repo), "github.com/mon-compte/atelier")
	expect(t, f.resolve(filepath.Join(repo, "src")), "github.com/mon-compte/atelier")
	wt := addWorktree(t, repo, filepath.Join(f.root, "worktrees", "atelier-fix"))
	expect(t, f.resolve(wt), "github.com/mon-compte/atelier")
	located, err := Locate(bg, f.db, f.cfg, mkdir(t, filepath.Join(repo, "doc")))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, located, "github.com/mon-compte/atelier")
}

func fakeGit(t *testing.T, script string) {
	t.Helper()
	bin := mkdir(t, filepath.Join(t.TempDir(), "bin"))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestDegradedFirstEncounterFreezesNothing(t *testing.T) {
	for name, degrade := range map[string]func(t *testing.T){
		"git missing": func(t *testing.T) { t.Setenv("PATH", "") },
		"git broken":  func(t *testing.T) { fakeGit(t, "exit 1") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			repo := makeRepoWithRemote(t, filepath.Join(f.root, "atelier"), "git@github.com:mon-compte/atelier.git")
			path := os.Getenv("PATH")
			degrade(t)
			expect(t, f.resolve(filepath.Join(repo, "src")), "dir:atelier")
			if n := f.count("projects") + f.count("project_paths"); n != 0 {
				t.Fatalf("%d rows written", n)
			}
			t.Setenv("PATH", path)
			expect(t, f.resolve(filepath.Join(repo, "src")), "github.com/mon-compte/atelier")
			expect(t, f.resolve(repo), "github.com/mon-compte/atelier")
		})
	}
}

func TestDeadlineBoundsAllGitCalls(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.root, "atelier", "src"))
	fakeGit(t, "exec /bin/sleep 10")
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	id, err := Resolve(ctx, tx, f.cfg, cwd)
	elapsed := time.Since(start)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err != nil || id != "dir:atelier" || elapsed > 500*time.Millisecond {
		t.Fatalf("%q, %v after %v", id, err, elapsed)
	}
	if n := f.count("projects") + f.count("project_paths"); n != 0 {
		t.Fatalf("%d rows written", n)
	}
}

func TestDefaultGitBudgetIsShared(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.root, "atelier", "src"))
	fakeGit(t, "exec /bin/sleep 10")
	saved := gitTimeout
	gitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitTimeout = saved })
	start := time.Now()
	id, err := Locate(bg, f.db, f.cfg, cwd)
	if elapsed := time.Since(start); err != nil || id != "dir:atelier" || elapsed > 350*time.Millisecond {
		t.Fatalf("%q, %v after %v", id, err, elapsed)
	}
}

func TestNestedRootsDeepestWins(t *testing.T) {
	f := newFixture(t)
	sub := mkdir(t, filepath.Join(f.root, "sub"))
	x := mkdir(t, filepath.Join(sub, "x", "src"))
	y := mkdir(t, filepath.Join(f.root, "y"))
	for _, roots := range [][]string{{f.root, sub}, {sub, f.root}} {
		cfg := f.cfg
		cfg.Roots = roots
		for cwd, want := range map[string]string{x: "dir:x", y: "dir:y", sub: Roots, f.root: Roots} {
			got, err := Locate(bg, f.db, cfg, cwd)
			if err != nil || got != want {
				t.Errorf("roots %v: Locate(%s) = %q, %v; want %q", roots, cwd, got, err, want)
			}
		}
		s, err := ScopeFor(bg, f.db, cfg, x)
		if err != nil || s.Projects == nil || s.Current != "dir:x" {
			t.Errorf("roots %v: scope %+v, %v", roots, s, err)
		}
	}
}

func TestLocateWritesNothing(t *testing.T) {
	f := newFixture(t)
	elsewhere := mkdir(t, filepath.Join(t.TempDir(), "ailleurs"))
	id, err := Locate(bg, f.db, f.cfg, elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, id, "path:"+elsewhere)
	if f.count("projects")+f.count("project_paths") != 0 {
		t.Fatal("Locate wrote rows")
	}
}

func TestProposalThenChoiceNeverOverwritten(t *testing.T) {
	f := newFixture(t)
	p := f.resolve(makeRepoWithRemote(t, filepath.Join(f.root, "depot"), "git@github.com:mon-employeur/x.git"))
	if got := f.label(p); got != [2]string{"work", "rule"} {
		t.Fatal(got)
	}
	if err := SetLabel(f.db, p, "personal"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec("DELETE FROM project_paths"); err != nil {
		t.Fatal(err)
	}
	expect(t, f.resolve(filepath.Join(f.root, "depot")), p)
	if got := f.label(p); got != [2]string{"personal", "user"} {
		t.Fatal(got)
	}
}

func TestNoRemoteOrUnknownOwnerIsUnsorted(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		f.resolve(makeRepo(t, filepath.Join(f.root, "atelier"))),
		f.resolve(makeRepoWithRemote(t, filepath.Join(f.root, "depot"), "git@github.com:exemple/x.git")),
	} {
		if got := f.label(p); got != [2]string{"unsorted", "rule"} {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestSetLabelUnknownProject(t *testing.T) {
	f := newFixture(t)
	var nf *ErrNotFound
	if !errors.As(SetLabel(f.db, "path:/nulle/part", "work"), &nf) {
		t.Fatal("expected ErrNotFound")
	}
}

func TestSetLabelBumpsUpdatedAt(t *testing.T) {
	f := newFixture(t)
	a, _, _ := f.three()
	if _, err := f.db.Exec("UPDATE projects SET updated_at = '2000-01-01T00:00:00.000Z'"); err != nil {
		t.Fatal(err)
	}
	if err := SetLabel(f.db, a, "work"); err != nil {
		t.Fatal(err)
	}
	var at string
	f.db.QueryRow("SELECT updated_at FROM projects WHERE id = ?", a).Scan(&at)
	checkStamp(t, at)
}

func checkStamp(t *testing.T, at string) {
	t.Helper()
	moment, err := time.Parse("2006-01-02T15:04:05.000Z", at)
	if err != nil || time.Since(moment) > time.Minute {
		t.Fatalf("updated_at %q not bumped", at)
	}
}

func accessible(t *testing.T, f *fixture, reader string) []string {
	t.Helper()
	set, err := Accessible(f.db, reader)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func TestAccessOneWayAndBoth(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.three()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(Grant(f.db, a, b, false))
	if got := accessible(t, f, a); !slices.Equal(got, []string{b}) || len(accessible(t, f, b)) != 0 {
		t.Fatal(got)
	}
	must(Grant(f.db, b, c, true))
	if !slices.Contains(accessible(t, f, b), c) || !slices.Contains(accessible(t, f, c), b) {
		t.Fatal("both")
	}
	must(Revoke(f.db, a, b, false))
	if len(accessible(t, f, a)) != 0 {
		t.Fatal("revoke")
	}
	must(Revoke(f.db, b, c, true))
	if len(accessible(t, f, b))+len(accessible(t, f, c)) != 0 {
		t.Fatal("revoke both")
	}
}

func TestRevokeKeepsTheTraceAndGrantReactivates(t *testing.T) {
	f := newFixture(t)
	a, b, _ := f.three()
	stamp := func() (active int, at string) {
		t.Helper()
		if err := f.db.QueryRow("SELECT active, updated_at FROM accesses WHERE reader = ? AND target = ?", a, b).Scan(&active, &at); err != nil {
			t.Fatal(err)
		}
		return
	}
	age := func() {
		t.Helper()
		if _, err := f.db.Exec("UPDATE accesses SET updated_at = '2000-01-01T00:00:00.000Z'"); err != nil {
			t.Fatal(err)
		}
	}
	if err := Grant(f.db, a, b, false); err != nil {
		t.Fatal(err)
	}
	age()
	if err := Revoke(f.db, a, b, false); err != nil {
		t.Fatal(err)
	}
	active, at := stamp()
	if active != 0 {
		t.Fatal("still active")
	}
	checkStamp(t, at)
	age()
	if err := Grant(f.db, a, b, false); err != nil {
		t.Fatal(err)
	}
	active, at = stamp()
	if active != 1 {
		t.Fatal("not reactivated")
	}
	checkStamp(t, at)
	age()
	if err := Grant(f.db, a, b, false); err != nil {
		t.Fatal(err)
	}
	if _, at = stamp(); at != "2000-01-01T00:00:00.000Z" {
		t.Fatalf("repeated grant bumped updated_at to %s", at)
	}
	expect(t, strings.Join(accessible(t, f, a), ","), b)
}

func TestInvalidAccesses(t *testing.T) {
	f := newFixture(t)
	a, _, _ := f.three()
	f.resolve(f.root)
	for _, err := range []error{
		Grant(f.db, a, a, false),
		Grant(f.db, Roots, a, false),
		Revoke(f.db, a, a, false),
		Grant(f.db, a, Roots, true),
		Grant(f.db, a, Roots, false),
		Revoke(f.db, Roots, a, false),
	} {
		if !errors.Is(err, ErrInvalidAccess) {
			t.Errorf("got %v", err)
		}
	}
	if len(accessible(t, f, a)) != 0 || f.count("accesses") != 0 {
		t.Fatal("access written")
	}
}

func TestAccessToUnknownProject(t *testing.T) {
	f := newFixture(t)
	a, _, _ := f.three()
	var nf *ErrNotFound
	if !errors.As(Grant(f.db, a, "path:/nulle/part", false), &nf) {
		t.Fatal("expected ErrNotFound")
	}
}

func TestFind(t *testing.T) {
	f := newFixture(t)
	elsewhere := t.TempDir()
	ambiguous := []string{filepath.Join(elsewhere, "x", "api"), filepath.Join(elsewhere, "y", "api")}
	atelierPath := filepath.Join(f.root, "atelier")
	atelier := f.resolve(atelierPath)
	for _, p := range ambiguous {
		f.resolve(p)
	}
	for _, ref := range []string{"atelier", atelier, atelierPath} {
		got, err := Find(f.db, ref)
		if err != nil || got != atelier {
			t.Fatalf("Find(%q) = %q, %v", ref, got, err)
		}
	}
	_, err := Find(f.db, "ateler")
	var nf *ErrNotFound
	if !errors.As(err, &nf) || !slices.Equal(nf.Close, []string{"atelier"}) || !strings.Contains(err.Error(), "atelier") {
		t.Fatalf("typo: %v", err)
	}
	_, err = Find(f.db, "zzz")
	if !errors.As(err, &nf) || len(nf.Close) != 0 {
		t.Fatalf("zzz: %v", err)
	}
	_, err = Find(f.db, "api")
	var amb *ErrAmbiguous
	if !errors.As(err, &amb) || len(amb.IDs) != 2 {
		t.Fatalf("api: %v", err)
	}
	for _, p := range ambiguous {
		if !strings.Contains(err.Error(), "path:"+p) {
			t.Fatalf("%v misses %s", err, p)
		}
	}
}

func TestCloseMatchesLikeDifflib(t *testing.T) {
	if r := ratio("abcd", "bcde"); r != 0.75 {
		t.Fatalf("ratio %v", r)
	}
	got := closeMatches("appel", []string{"ape", "apple", "peach", "puppy"})
	if !slices.Equal(got, []string{"apple", "ape"}) {
		t.Fatal(got)
	}
	got = closeMatches("atelir", []string{"atelier", "ateliers", "atelie", "atelier-web", "autre"})
	if !slices.Equal(got, []string{"atelier", "ateliers", "atelie"}) {
		t.Fatal(got)
	}
	got = closeMatches("abc", []string{"abd", "abe", "abf", "abg"})
	if !slices.Equal(got, []string{"abg", "abf", "abe"}) {
		t.Fatal(got)
	}
	for word, want := range map[string][]string{"paul": {"upal"}, "beace": {"bcdeb"}, "ecac": nil} {
		candidates := map[string][]string{"paul": {"lapu", "upal"}, "beace": {"bcdeb"}, "ecac": {"acadac"}}[word]
		if got := closeMatches(word, candidates); !slices.Equal(got, want) {
			t.Errorf("closeMatches(%q, %v) = %v, want %v", word, candidates, got, want)
		}
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	f.resolve(makeRepoWithRemote(t, filepath.Join(f.root, "zeta"), "git@github.com:mon-compte/zeta.git"))
	f.resolve(makeRepo(t, filepath.Join(f.root, "atelier")))
	list, err := List(f.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != (Project{ID: "dir:atelier", Name: "atelier", Label: "unsorted", LabelBy: "rule", UpdatedAt: list[0].UpdatedAt}) || list[1].ID != "github.com/mon-compte/zeta" || list[1].Name != "zeta" {
		t.Fatalf("%+v", list)
	}
	checkStamp(t, list[0].UpdatedAt)
}

func TestScope(t *testing.T) {
	f := newFixture(t)
	a, b, _ := f.three()
	if err := Grant(f.db, a, b, false); err != nil {
		t.Fatal(err)
	}
	s, err := ScopeFor(bg, f.db, f.cfg, filepath.Join(f.root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Current != a || len(s.Projects) != 2 || !s.Projects[a] || !s.Projects[b] {
		t.Fatalf("%+v", s)
	}
	s, err = ScopeFor(bg, f.db, f.cfg, f.root)
	if err != nil || s.Projects != nil || s.Current != Roots {
		t.Fatalf("%+v %v", s, err)
	}
	if Everything.Projects != nil || Everything.Current != "" {
		t.Fatal(Everything)
	}
}
