package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jplot/brain/internal/db"
)

var gitTimeout = 5 * time.Second

// A folder that is not a repository today may be git-init-ed tomorrow.
const negativeTTL = 24 * time.Hour

var (
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	pathPattern = regexp.MustCompile(`^[^\s/]+(/[^\s/]+)+$`)
)

type Info struct{ Identity, Name string }

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

// All git calls of one lookup share one deadline: a hook has 1 s, and three
// calls of 5 s each would take 15.
func Of(ctx context.Context, path string) (Info, bool, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, gitTimeout)
		defer cancel()
	}
	common, ok, err := runGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !ok {
		return Info{}, false, err
	}
	// A worktree's common dir is <main>/.git; a bare repository or a separate
	// git dir (submodule) has none, so the work tree itself is the repo.
	main := filepath.Dir(common)
	if filepath.Base(common) != ".git" {
		if main, ok, err = runGit(ctx, path, "rev-parse", "--show-toplevel"); err != nil || !ok {
			return Info{}, false, err
		}
	}
	remote, _, err := runGit(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		return Info{}, false, err
	}
	if id, ok := Normalize(remote); ok {
		return Info{Identity: id, Name: id[strings.LastIndex(id, "/")+1:]}, true, nil
	}
	// All refs, from the main repo: HEAD alone changes with the checked-out
	// branch (an orphan worktree has none) and would change the identity.
	roots, ok, err := runGit(ctx, main, "rev-list", "--max-parents=0", "--all")
	if err != nil || !ok || roots == "" {
		return Info{}, false, err
	}
	return Info{Identity: "commit:" + slices.Min(strings.Fields(roots)), Name: filepath.Base(main)}, true, nil
}

// Exit 128 means not a repository (or no commits), 2 no such remote; any
// other failure, such as the xcrun shim of a Mac without Command Line Tools,
// a missing git or the deadline, means no answer.
func runGit(ctx context.Context, dir string, args ...string) (string, bool, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err == nil {
		return strings.TrimSpace(string(out)), true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 128 || exit.ExitCode() == 2) && ctx.Err() == nil {
		return "", false, nil
	}
	return "", false, fmt.Errorf("git %s : %w", args[0], err)
}

func Cached(ctx context.Context, q db.Querier, path string) (string, bool, error) {
	// The shared deadline bounds git only: a write after git used the whole
	// budget must still succeed.
	bg := context.Background()
	var uid sql.NullString
	var checked string
	err := q.QueryRowContext(bg, "SELECT subject_uid, checked_at FROM repo_cache WHERE path = ?", path).Scan(&uid, &checked)
	if err == nil {
		at, perr := time.Parse(db.UTCFormat, checked)
		if uid.Valid || perr == nil && time.Since(at) < negativeTTL {
			return uid.String, uid.Valid, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	info, ok, err := Of(ctx, path)
	if err != nil {
		return "", false, err
	}
	now := db.Stamp(time.Time{})
	var found any
	if ok {
		if _, err := q.ExecContext(bg, "INSERT OR IGNORE INTO subjects(uid, kind, name, aliases, repo, created_at, updated_at) VALUES (?, 'repo', ?, '[]', ?, ?, ?)",
			db.NewUID(), info.Name, info.Identity, now, now); err != nil {
			return "", false, err
		}
		var subject string
		if err := q.QueryRowContext(bg, "SELECT uid FROM subjects WHERE repo = ?", info.Identity).Scan(&subject); err != nil {
			return "", false, err
		}
		found = subject
	}
	if _, err := q.ExecContext(bg, "INSERT OR REPLACE INTO repo_cache(path, subject_uid, checked_at) VALUES (?, ?, ?)", path, found, now); err != nil {
		return "", false, err
	}
	subject, _ := found.(string)
	return subject, ok, nil
}
