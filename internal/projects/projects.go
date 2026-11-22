package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
)

var (
	ErrNotClassifiable = errors.New("projet non classable")
	ErrInvalidAccess   = errors.New("accès impossible")
)

type ErrNotFound struct {
	Ref   string
	Close []string
}

func (e *ErrNotFound) Error() string {
	if len(e.Close) == 0 {
		return "projet introuvable : " + e.Ref
	}
	return fmt.Sprintf("projet introuvable : %s (proches : %s)", e.Ref, strings.Join(e.Close, ", "))
}

type ErrAmbiguous struct {
	Ref string
	IDs []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("nom ambigu : %s (%s)", e.Ref, strings.Join(e.IDs, ", "))
}

type Project struct {
	ID, Name, Label, LabelBy, UpdatedAt string
}

var ctx = context.Background()

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func Resolve(tx *sql.Tx, cfg config.Config, cwd string) (string, error) {
	cwd = filepath.Clean(cwd)
	dir, p, frozen, err := newLocator(tx, cfg).locate(cwd)
	if err != nil {
		return "", err
	}
	if !frozen {
		_, err := tx.ExecContext(ctx, `INSERT INTO projects(id, name, label, label_by, updated_at)
			VALUES (?, ?, ?, 'rule', ?) ON CONFLICT(id) DO NOTHING`, p.id, p.name, proposedLabel(cfg, p.owner), now())
		if err != nil {
			return "", err
		}
	}
	// The project dir is frozen too: a subfolder or worktree first met after
	// a remote rename must still join the identity of its repository.
	for _, path := range []string{cwd, dir} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO project_paths(path, project_id) VALUES (?, ?) ON CONFLICT(path) DO NOTHING", path, p.id); err != nil {
			return "", err
		}
	}
	return p.id, nil
}

func Locate(q db.Querier, cfg config.Config, cwd string) (string, error) {
	_, p, _, err := newLocator(q, cfg).locate(filepath.Clean(cwd))
	return p.id, err
}

func proposedLabel(cfg config.Config, owner string) string {
	for o, label := range cfg.Labels {
		if strings.EqualFold(o, owner) {
			return label
		}
	}
	return "unsorted"
}

func exists(q db.Querier, id string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE id = ?", id).Scan(&n)
	return n > 0, err
}

func requireKnown(q db.Querier, id string) error {
	known, err := exists(q, id)
	if err == nil && !known {
		return &ErrNotFound{Ref: id}
	}
	return err
}

func SetLabel(q db.Querier, id, label string) error {
	if id == Roots {
		return fmt.Errorf("%w : %s", ErrNotClassifiable, id)
	}
	if err := requireKnown(q, id); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, "UPDATE projects SET label = ?, label_by = 'user', updated_at = ? WHERE id = ?", label, now(), id)
	return err
}

func pairs(q db.Querier, reader, target string, both bool) ([][2]string, error) {
	list := [][2]string{{reader, target}}
	if both {
		list = append(list, [2]string{target, reader})
	}
	for _, p := range list {
		if p[0] == p[1] || p[0] == Roots || p[1] == Roots {
			return nil, fmt.Errorf("%w : %s vers %s", ErrInvalidAccess, p[0], p[1])
		}
	}
	for _, id := range []string{reader, target} {
		if err := requireKnown(q, id); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// Grant and Revoke leave an unchanged access alone so that a repeated
// command does not bump updated_at and travel through sync as a change.
func Grant(q db.Querier, reader, target string, both bool) error {
	list, err := pairs(q, reader, target, both)
	if err != nil {
		return err
	}
	for _, p := range list {
		_, err := q.ExecContext(ctx, `INSERT INTO accesses(reader, target, active, updated_at) VALUES (?, ?, 1, ?)
			ON CONFLICT(reader, target) DO UPDATE SET active = 1, updated_at = excluded.updated_at WHERE active = 0`,
			p[0], p[1], now())
		if err != nil {
			return err
		}
	}
	return nil
}

func Revoke(q db.Querier, reader, target string, both bool) error {
	list, err := pairs(q, reader, target, both)
	if err != nil {
		return err
	}
	for _, p := range list {
		_, err := q.ExecContext(ctx, "UPDATE accesses SET active = 0, updated_at = ? WHERE reader = ? AND target = ? AND active = 1",
			now(), p[0], p[1])
		if err != nil {
			return err
		}
	}
	return nil
}

func Accessible(q db.Querier, reader string) (map[string]bool, error) {
	targets, err := column(q, "SELECT target FROM accesses WHERE reader = ? AND active = 1", reader)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(targets))
	for _, t := range targets {
		set[t] = true
	}
	return set, nil
}

func List(q db.Querier) ([]Project, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, name, label, label_by, updated_at FROM projects ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Label, &p.LabelBy, &p.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func Find(q db.Querier, ref string) (string, error) {
	known, err := exists(q, ref)
	if err != nil {
		return "", err
	}
	if known {
		return ref, nil
	}
	var id string
	switch err := q.QueryRowContext(ctx, "SELECT project_id FROM project_paths WHERE path = ?", ref).Scan(&id); {
	case err == nil:
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	ids, err := column(q, "SELECT id FROM projects WHERE name = ? ORDER BY id", ref)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		names, err := column(q, "SELECT DISTINCT name FROM projects")
		if err != nil {
			return "", err
		}
		return "", &ErrNotFound{Ref: ref, Close: closeMatches(ref, names)}
	}
	return "", &ErrAmbiguous{Ref: ref, IDs: ids}
}

func column(q db.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Same suggestions as the Python version's difflib.get_close_matches:
// at most 3 names whose ratio reaches 0.6, best first.
func closeMatches(word string, names []string) []string {
	type scored struct {
		score float64
		name  string
	}
	var found []scored
	for _, n := range names {
		if s := ratio(word, n); s >= 0.6 {
			found = append(found, scored{s, n})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].score != found[j].score {
			return found[i].score > found[j].score
		}
		return found[i].name > found[j].name
	})
	var out []string
	for i := 0; i < len(found) && i < 3; i++ {
		out = append(out, found[i].name)
	}
	return out
}

// ratio is SequenceMatcher.ratio (Ratcliff/Obershelp); its junk heuristic
// only starts at 200 characters, far beyond a project name.
func ratio(a, b string) float64 {
	x, y := []rune(a), []rune(b)
	if len(x)+len(y) == 0 {
		return 1
	}
	return 2 * float64(matching(x, y)) / float64(len(x)+len(y))
}

func matching(a, b []rune) int {
	i, j, n := longestMatch(a, b)
	if n == 0 {
		return 0
	}
	return n + matching(a[:i], b[:j]) + matching(a[i+n:], b[j+n:])
}

// Ties go to the block starting earliest in a, then in b, as in difflib.
func longestMatch(a, b []rune) (bi, bj, best int) {
	prev := make([]int, len(b)+1)
	for i := range a {
		cur := make([]int, len(b)+1)
		for j := range b {
			if a[i] == b[j] {
				cur[j+1] = prev[j] + 1
				if cur[j+1] > best {
					best, bi, bj = cur[j+1], i-cur[j+1]+1, j-cur[j+1]+1
				}
			}
		}
		prev = cur
	}
	return bi, bj, best
}
