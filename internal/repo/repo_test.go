package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

var bg = context.Background()

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v : %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func makeRepo(t *testing.T, path string) string {
	t.Helper()
	mkdir(t, path)
	git(t, path, "init", "-q")
	return path
}

func makeRepoWithRemote(t *testing.T, path, remote string) string {
	t.Helper()
	makeRepo(t, path)
	git(t, path, "remote", "add", "origin", remote)
	return path
}

// rootCommit writes a root commit without signing: the object goes in with
// hash-object and the branch is pointed at it by update-ref.
func rootCommit(t *testing.T, repo, message string) string {
	t.Helper()
	tree := git(t, repo, "mktree")
	cmd := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("tree " + tree + "\nauthor exemple <exemple@example.org> 1700000000 +0000\ncommitter exemple <exemple@example.org> 1700000000 +0000\n\n" + message + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash-object : %v\n%s", err, out)
	}
	sha := strings.TrimSpace(string(out))
	git(t, repo, "update-ref", "refs/heads/main", sha)
	git(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	return sha
}

func addWorktree(t *testing.T, repo, path string) string {
	t.Helper()
	git(t, repo, "worktree", "add", "-q", "--orphan", "-b", filepath.Base(path), path)
	return path
}

func fakeGit(t *testing.T, script string) {
	t.Helper()
	bin := mkdir(t, filepath.Join(t.TempDir(), "bin"))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

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

func count(t *testing.T, d *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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

func TestRemoteIdentityAndName(t *testing.T) {
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "atelier"), "git@github.com:Mon-Compte/Atelier.git")
	info, ok, err := Of(bg, mkdir(t, filepath.Join(repo, "src")))
	if err != nil || !ok || info != (Info{Identity: "github.com/mon-compte/atelier", Name: "atelier", Folder: "atelier"}) {
		t.Fatalf("%+v, %v, %v", info, ok, err)
	}
}

func TestRemoteInsteadOfIsApplied(t *testing.T) {
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "atelier"), "gh:mon-compte/atelier")
	git(t, repo, "config", "url.git@github.com:.insteadOf", "gh:")
	info, ok, err := Of(bg, repo)
	if err != nil || !ok {
		t.Fatalf("%v, %v", ok, err)
	}
	expect(t, info.Identity, "github.com/mon-compte/atelier")
}

func TestWorktreeGivesMainRepo(t *testing.T) {
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "atelier"), "git@github.com:mon-compte/atelier.git")
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "ailleurs"))
	a, _, err := Of(bg, wt)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := Of(bg, repo)
	if a != b || a.Identity != "github.com/mon-compte/atelier" {
		t.Fatalf("%+v vs %+v", a, b)
	}
}

func TestWorktreeWithoutRemoteNamedAfterMainFolder(t *testing.T) {
	repo := makeRepo(t, filepath.Join(t.TempDir(), "atelier"))
	sha := rootCommit(t, repo, "debut")
	wt := filepath.Join(t.TempDir(), "ailleurs")
	git(t, repo, "worktree", "add", "-q", "-b", "autre", wt)
	info, ok, err := Of(bg, wt)
	if err != nil || !ok || info != (Info{Identity: "commit:" + sha, Name: "atelier", Folder: "atelier"}) {
		t.Fatalf("%+v, %v, %v", info, ok, err)
	}
}

func TestBareRepoWorktreeIsItsOwnRepo(t *testing.T) {
	bare := mkdir(t, filepath.Join(t.TempDir(), "outil.git"))
	git(t, bare, "init", "-q", "--bare")
	git(t, bare, "remote", "add", "origin", "git@github.com:mon-compte/outil.git")
	wt := addWorktree(t, bare, filepath.Join(t.TempDir(), "outil-b"))
	info, ok, err := Of(bg, wt)
	if err != nil || !ok || info != (Info{Identity: "github.com/mon-compte/outil", Name: "outil", Folder: "outil-b"}) {
		t.Fatalf("%+v, %v, %v", info, ok, err)
	}
}

func TestSeparateGitDirIsItsOwnRepo(t *testing.T) {
	super := makeRepo(t, filepath.Join(t.TempDir(), "super"))
	mkdir(t, filepath.Join(super, ".git", "modules"))
	sub := filepath.Join(super, "sub")
	git(t, super, "init", "-q", "--separate-git-dir="+filepath.Join(super, ".git", "modules", "sub"), sub)
	sha := rootCommit(t, sub, "debut")
	info, ok, err := Of(bg, sub)
	if err != nil || !ok || info != (Info{Identity: "commit:" + sha, Name: "sub", Folder: "sub"}) {
		t.Fatalf("%+v, %v, %v", info, ok, err)
	}
}

func TestNoRemoteUsesRootCommit(t *testing.T) {
	dir := t.TempDir()
	a := makeRepo(t, filepath.Join(dir, "origine"))
	sha := rootCommit(t, a, "debut")
	b := filepath.Join(dir, "copie")
	git(t, dir, "clone", "-q", a, b)
	git(t, b, "remote", "remove", "origin")
	ia, ok, err := Of(bg, a)
	if err != nil || !ok {
		t.Fatalf("%v, %v", ok, err)
	}
	ib, _, _ := Of(bg, b)
	expect(t, ia.Identity, "commit:"+sha)
	expect(t, ib.Identity, "commit:"+sha)
	expect(t, ia.Name, "origine")
	expect(t, ib.Name, "copie")
}

func TestSeveralRootCommitsTakeSmallest(t *testing.T) {
	repo := makeRepo(t, filepath.Join(t.TempDir(), "fusion"))
	a := rootCommit(t, repo, "un")
	tree := git(t, repo, "mktree")
	cmd := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("tree " + tree + "\nauthor exemple <exemple@example.org> 1700000001 +0000\ncommitter exemple <exemple@example.org> 1700000001 +0000\n\ndeux\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	b := strings.TrimSpace(string(out))
	merge := "tree " + tree + "\nparent " + a + "\nparent " + b + "\nauthor exemple <exemple@example.org> 1700000002 +0000\ncommitter exemple <exemple@example.org> 1700000002 +0000\n\nfusion\n"
	cmd = exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(merge)
	out, err = cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	git(t, repo, "update-ref", "refs/heads/main", strings.TrimSpace(string(out)))
	info, ok, err := Of(bg, repo)
	if err != nil || !ok {
		t.Fatalf("%v, %v", ok, err)
	}
	want := min(a, b)
	expect(t, info.Identity, "commit:"+want)
}

func TestEmptyRepoIsNoRepo(t *testing.T) {
	repo := makeRepo(t, filepath.Join(t.TempDir(), "vide"))
	if _, ok, err := Of(bg, repo); ok || err != nil {
		t.Fatalf("%v, %v", ok, err)
	}
}

func TestNotARepo(t *testing.T) {
	if _, ok, err := Of(bg, mkdir(t, filepath.Join(t.TempDir(), "notes"))); ok || err != nil {
		t.Fatalf("%v, %v", ok, err)
	}
}

func TestGitMissingIsErrorNotCached(t *testing.T) {
	d := openDB(t)
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "atelier"), "git@github.com:mon-compte/atelier.git")
	path := os.Getenv("PATH")
	t.Setenv("PATH", "")
	if _, ok, err := Cached(bg, d, repo); err == nil || ok {
		t.Fatalf("%v, %v", ok, err)
	}
	if n := count(t, d, "repo_cache") + count(t, d, "subjects"); n != 0 {
		t.Fatalf("%d rows written", n)
	}
	t.Setenv("PATH", path)
	if _, ok, err := Cached(bg, d, repo); err != nil || !ok {
		t.Fatalf("%v, %v", ok, err)
	}
}

func TestGitBrokenIsError(t *testing.T) {
	fakeGit(t, "exit 1")
	if _, ok, err := Of(bg, t.TempDir()); err == nil || ok {
		t.Fatalf("%v, %v", ok, err)
	}
}

func TestSlowGitHonoursDeadline(t *testing.T) {
	d := openDB(t)
	repo := makeRepo(t, filepath.Join(t.TempDir(), "atelier"))
	fakeGit(t, "exec /bin/sleep 10")
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, ok, err := Cached(ctx, d, repo)
	if elapsed := time.Since(start); err == nil || ok || elapsed > 500*time.Millisecond {
		t.Fatalf("%v, %v after %v", ok, err, elapsed)
	}
	if !errors.Is(err, ErrGit) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not a git deadline error: %v", err)
	}
	if n := count(t, d, "repo_cache") + count(t, d, "subjects"); n != 0 {
		t.Fatalf("%d rows written", n)
	}
}

func TestMissingGitIsErrGit(t *testing.T) {
	d := openDB(t)
	repo := makeRepo(t, filepath.Join(t.TempDir(), "atelier"))
	t.Setenv("PATH", t.TempDir())
	_, _, err := Cached(bg, d, repo)
	if !errors.Is(err, ErrGit) || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDefaultDeadlineIsShared(t *testing.T) {
	fakeGit(t, "/bin/sleep 0.6 >/dev/null 2>&1\necho /srv/x/.git")
	saved := gitTimeout
	gitTimeout = time.Second
	t.Cleanup(func() { gitTimeout = saved })
	start := time.Now()
	_, _, err := Of(bg, t.TempDir())
	if elapsed := time.Since(start); err == nil || elapsed > 1500*time.Millisecond {
		t.Fatalf("%v after %v", err, elapsed)
	}
}

func TestOrphanWorktreeKeepsMainIdentity(t *testing.T) {
	repo := makeRepo(t, filepath.Join(t.TempDir(), "atelier"))
	sha := rootCommit(t, repo, "debut")
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "orphelin"))
	info, ok, err := Of(bg, wt)
	if err != nil || !ok || info != (Info{Identity: "commit:" + sha, Name: "atelier", Folder: "atelier"}) {
		t.Fatalf("%+v, %v, %v", info, ok, err)
	}
}

func TestOrphanBranchWithSmallerRootDoesNotMoveIdentity(t *testing.T) {
	repo := makeRepo(t, filepath.Join(t.TempDir(), "atelier"))
	sha := rootCommit(t, repo, "debut")
	tree := git(t, repo, "mktree")
	var smaller string
	for i := 0; smaller == ""; i++ {
		cmd := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("tree %s\nauthor exemple <exemple@example.org> 1700000000 +0000\ncommitter exemple <exemple@example.org> 1700000000 +0000\n\npages %d\n", tree, i))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if c := strings.TrimSpace(string(out)); c < sha {
			smaller = c
		}
	}
	git(t, repo, "update-ref", "refs/heads/pages", smaller)
	wt := filepath.Join(t.TempDir(), "pages")
	git(t, repo, "worktree", "add", "-q", wt, "pages")
	for _, dir := range []string{repo, wt} {
		info, ok, err := Of(bg, dir)
		if err != nil || !ok || info.Identity != "commit:"+sha {
			t.Fatalf("%s: %+v, %v, %v", dir, info, ok, err)
		}
	}
}

func TestUnbornHeadWithHistoryOnOtherRefs(t *testing.T) {
	repo := makeRepo(t, filepath.Join(t.TempDir(), "atelier"))
	sha := rootCommit(t, repo, "debut")
	git(t, repo, "checkout", "-q", "--orphan", "vide")
	info, ok, err := Of(bg, repo)
	if err != nil || !ok || info.Identity != "commit:"+sha {
		t.Fatalf("%+v, %v, %v", info, ok, err)
	}
}

func TestStaleNegativeCacheIsRecomputed(t *testing.T) {
	d := openDB(t)
	dir := mkdir(t, filepath.Join(t.TempDir(), "futur"))
	if _, ok, err := Cached(bg, d, dir); ok || err != nil {
		t.Fatalf("%v, %v", ok, err)
	}
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", "git@github.com:mon-compte/futur.git")
	if _, ok, _ := Cached(bg, d, dir); ok {
		t.Fatal("fresh negative entry ignored")
	}
	stale := db.Stamp(time.Now().Add(-25 * time.Hour))
	if _, err := d.Exec("UPDATE repo_cache SET checked_at = ?", stale); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Cached(bg, d, dir); !ok || err != nil {
		t.Fatalf("%v, %v", ok, err)
	}
}

func TestCachedCreatesSubjectOnce(t *testing.T) {
	d := openDB(t)
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "atelier"), "git@github.com:mon-compte/atelier.git")
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "ailleurs"))
	uid, ok, err := Cached(bg, d, repo)
	if err != nil || !ok || len(uid) != 36 {
		t.Fatalf("%q, %v, %v", uid, ok, err)
	}
	other, _, _ := Cached(bg, d, wt)
	expect(t, other, uid)
	var kind, name, identity, created, updated string
	if err := d.QueryRow("SELECT kind, name, repo, created_at, updated_at FROM subjects WHERE uid = ?", uid).
		Scan(&kind, &name, &identity, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if kind != "repo" || name != "atelier" || identity != "github.com/mon-compte/atelier" || len(created) != len("2026-01-01T10:00:00.000Z") || created != updated {
		t.Fatalf("%q %q %q %q %q", kind, name, identity, created, updated)
	}
	if n := count(t, d, "subjects"); n != 1 {
		t.Fatalf("%d subjects", n)
	}
}

func TestCachedAddsMainFolderAsAlias(t *testing.T) {
	d := openDB(t)
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "verger"), "git@github.com:exemple/verger-api.git")
	wt := addWorktree(t, repo, filepath.Join(t.TempDir(), "verger-feat-x"))
	uid, ok, err := Cached(bg, d, wt)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	var name, aliases, term string
	if err := d.QueryRow("SELECT name, aliases FROM subjects WHERE uid = ?", uid).Scan(&name, &aliases); err != nil {
		t.Fatal(err)
	}
	if name != "verger-api" || aliases != `["verger"]` {
		t.Fatalf("%q %s", name, aliases)
	}
	if err := d.QueryRow("SELECT subject_uid FROM subject_terms WHERE term = 'verger'").Scan(&term); err != nil || term != uid {
		t.Fatalf("term %q %v", term, err)
	}
	same := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "Outil"), "git@github.com:exemple/outil.git")
	uid, _, _ = Cached(bg, d, same)
	d.QueryRow("SELECT aliases FROM subjects WHERE uid = ?", uid).Scan(&aliases)
	expect(t, aliases, "[]")
}

func TestCachedSkipsFolderAliasOfAnotherSubject(t *testing.T) {
	for _, other := range [][]string{{"Ruche"}, {"Apiculture", "ruche"}} {
		d := openDB(t)
		taken, err := subjects.Create(d, other[0], other[1:], nil)
		if err != nil {
			t.Fatal(err)
		}
		repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "ruche"), "git@github.com:exemple/ruche-api.git")
		uid, _, err := Cached(bg, d, repo)
		if err != nil {
			t.Fatal(err)
		}
		var aliases, term string
		d.QueryRow("SELECT aliases FROM subjects WHERE uid = ?", uid).Scan(&aliases)
		expect(t, aliases, "[]")
		if err := d.QueryRow("SELECT group_concat(subject_uid) FROM subject_terms WHERE term = 'ruche'").Scan(&term); err != nil || term != taken {
			t.Fatalf("%v: term ruche → %q %v", other, term, err)
		}
	}
}

func TestCachedNotARepoIsCached(t *testing.T) {
	d := openDB(t)
	dir := mkdir(t, filepath.Join(t.TempDir(), "notes"))
	if _, ok, err := Cached(bg, d, dir); ok || err != nil {
		t.Fatalf("%v, %v", ok, err)
	}
	t.Setenv("PATH", "")
	if _, ok, err := Cached(bg, d, dir); ok || err != nil {
		t.Fatalf("%v, %v", ok, err)
	}
	if n := count(t, d, "subjects"); n != 0 {
		t.Fatalf("%d subjects", n)
	}
}

func TestCacheHit(t *testing.T) {
	d := openDB(t)
	repo := makeRepoWithRemote(t, filepath.Join(t.TempDir(), "atelier"), "git@github.com:mon-compte/atelier.git")
	uid, _, err := Cached(bg, d, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	got, ok, err := Cached(bg, d, repo)
	if err != nil || !ok {
		t.Fatalf("%v, %v", ok, err)
	}
	expect(t, got, uid)
}
