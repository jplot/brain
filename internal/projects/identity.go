package projects

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
)

const Roots = "roots"

var gitTimeout = 5 * time.Second

var (
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	pathPattern = regexp.MustCompile(`^[^\s/]+(/[^\s/]+)+$`)
)

func Normalize(remote string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(remote))
	var host, rest string
	if _, after, ok := strings.Cut(s, "://"); ok {
		host, rest, _ = strings.Cut(after, "/")
		host, _, _ = strings.Cut(host[strings.LastIndex(host, "@")+1:], ":")
	} else if before, after, ok := strings.Cut(s, ":"); ok && !strings.Contains(before, "/") {
		host, rest = before[strings.LastIndex(before, "@")+1:], after
	} else {
		host, rest, _ = strings.Cut(s, "/")
		// git reads this form as a local path; paul still takes GitHub.com/owner/repo
		// as a URL, but only with a dotted host, so repos/x/y stays a path.
		if !strings.Contains(host, ".") {
			return "", false
		}
	}
	rest = strings.TrimSuffix(strings.Trim(rest, "/"), ".git")
	if !hostPattern.MatchString(host) || !pathPattern.MatchString(rest) {
		return "", false
	}
	return host + "/" + rest, true
}

type identity struct{ id, name, owner string }

type locator struct {
	ctx      context.Context
	q        db.Querier
	roots    []string
	degraded bool
}

// git reports resolved paths (/private/var for /var on macOS), so a root
// reached through a symlink is matched under both spellings.
func newLocator(ctx context.Context, q db.Querier, cfg config.Config) *locator {
	var roots []string
	for _, r := range cfg.Roots {
		r = filepath.Clean(r)
		roots = append(roots, r)
		if real, err := filepath.EvalSymlinks(r); err == nil && real != r {
			roots = append(roots, real)
		}
	}
	return &locator{ctx: ctx, q: q, roots: roots}
}

// All git calls of one localization share one deadline (a hook has 1 s;
// three calls of 5 s would take 15). The deadline bounds git only, so the
// folder rules still answer from the database once it has passed.
func (l *locator) locate(cwd string) (dir string, p identity, frozen bool, err error) {
	if p.id, frozen, err = l.frozen(cwd); err != nil || frozen {
		return cwd, p, frozen, err
	}
	if _, ok := l.ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		l.ctx, cancel = context.WithTimeout(l.ctx, gitTimeout)
		defer cancel()
	}
	dir, repo := l.projectDir(cwd)
	if dir != cwd {
		if p.id, frozen, err = l.frozen(dir); err != nil || frozen {
			return dir, p, frozen, err
		}
	}
	return dir, l.identity(dir, repo), false, nil
}

func (l *locator) frozen(path string) (string, bool, error) {
	var id string
	err := l.q.QueryRowContext(background, "SELECT project_id FROM project_paths WHERE path = ?", path).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func (l *locator) projectDir(cwd string) (string, bool) {
	if l.isRoot(cwd) {
		return cwd, false
	}
	if top := l.repoTop(cwd); top != "" {
		return top, true
	}
	dir := l.folderProject(cwd)
	if dir != cwd {
		if top := l.repoTop(dir); top != "" {
			return top, true
		}
	}
	return dir, false
}

func (l *locator) identity(dir string, repo bool) identity {
	if l.isRoot(dir) {
		return identity{id: Roots, name: Roots}
	}
	if repo {
		if id, ok := Normalize(l.git(dir, "remote", "get-url", "origin")); ok {
			parts := strings.Split(id, "/")
			return identity{id: id, name: parts[len(parts)-1], owner: parts[1]}
		}
	}
	if _, rel, ok := l.under(dir); ok {
		return identity{id: "dir:" + rel, name: filepath.Base(dir)}
	}
	return identity{id: "path:" + dir, name: filepath.Base(dir)}
}

// A worktree's common dir is <main>/.git; a bare repository or a separate
// git dir (submodule) has none, so the work tree itself is the project.
func (l *locator) repoTop(dir string) string {
	common := l.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" {
		return ""
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return l.git(dir, "rev-parse", "--show-toplevel")
}

func (l *locator) folderProject(cwd string) string {
	root, rel, ok := l.under(cwd)
	if !ok {
		return cwd
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if parts[0] == "worktrees" && len(parts) > 1 {
		return filepath.Join(root, worktreeOwner(root, parts[1]))
	}
	return filepath.Join(root, parts[0])
}

func worktreeOwner(root, name string) string {
	best := ""
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		d := e.Name()
		if e.IsDir() && d != "worktrees" && (name == d || strings.HasPrefix(name, d+"-")) && len(d) > len(best) {
			best = d
		}
	}
	if best == "" {
		return filepath.Join("worktrees", name)
	}
	return best
}

func (l *locator) isRoot(path string) bool {
	for _, r := range l.roots {
		if path == r {
			return true
		}
	}
	return false
}

// The deepest root wins: with roots ~/code and ~/code/work, ~/code/work/x
// is the project x, not the root work.
func (l *locator) under(path string) (root, rel string, ok bool) {
	for _, r := range l.roots {
		rr, err := filepath.Rel(r, path)
		inside := err == nil && rr != "." && rr != ".." && !strings.HasPrefix(rr, ".."+string(filepath.Separator))
		if inside && (!ok || len(r) > len(root)) {
			root, rel, ok = r, rr, true
		}
	}
	return root, rel, ok
}

// git dies with 128 (not a repository) and get-url exits with 2 (no such
// remote); any other failure, such as the xcrun shim of a Mac without
// Command Line Tools, a missing git or the deadline, means no answer.
func (l *locator) git(dir string, args ...string) string {
	out, err := exec.CommandContext(l.ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 128 && exit.ExitCode() != 2 {
			l.degraded = true
		}
		return ""
	}
	return strings.TrimSpace(string(out))
}
