package subjects

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jplot/brain/internal/db"
)

var (
	ErrCycle        = errors.New("cycle in the subject tree")
	ErrSecondParent = errors.New("the subject already has a parent")
	ErrUserSet      = errors.New("set by the user, the summarizer does not touch it")
	ErrLinkType     = errors.New("unknown link type")
	ErrSelfMerge    = errors.New("a subject cannot merge with itself")
	ErrSelfLink     = errors.New("a subject cannot link to itself")
	ErrRemoveParent = errors.New("only the user removes a parent")
)

type ErrAmbiguousSubject struct {
	Term       string
	Candidates []Subject
}

func (e *ErrAmbiguousSubject) Error() string {
	names := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		names[i] = c.Name + " " + c.UID
	}
	return fmt.Sprintf("several subjects share this name: %s (%s)", e.Term, strings.Join(names, ", "))
}

type ErrUnknownSubject struct{ Term string }

func (e *ErrUnknownSubject) Error() string { return fmt.Sprintf("unknown subject: %s", e.Term) }

// TreeByTerm resolves a name, keyword or uid to that subject and its descendants.
func TreeByTerm(q db.Querier, term string) ([]string, error) {
	found, err := FindByTerm(q, term)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, &ErrUnknownSubject{Term: term}
	case 1:
		below, err := Descendants(q, found[0].UID)
		return append([]string{found[0].UID}, below...), err
	}
	return nil, &ErrAmbiguousSubject{Term: term, Candidates: found}
}

type Origin struct {
	User       bool
	EpisodeUID *string
}

func (o Origin) name() string {
	if o.User {
		return "user"
	}
	return "model"
}

type Edge struct{ Type, Target string }

type Node struct {
	Subject
	Children []Node
	Links    []Edge
}

func column(q db.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(bg, query, args...)
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

type edge struct{ child, parent, origin string }

// up stops at a subject already seen: a cycle brought by sync must not hang a hook.
func up(q db.Querier, uid string) ([]edge, error) {
	var path []edge
	for seen := map[string]bool{}; !seen[uid]; {
		seen[uid] = true
		var parent, origin sql.NullString
		if err := q.QueryRowContext(bg, "SELECT parent_uid, parent_origin FROM subjects WHERE uid = ?", uid).Scan(&parent, &origin); err != nil {
			return nil, err
		}
		if !parent.Valid {
			break
		}
		path = append(path, edge{uid, parent.String, origin.String})
		uid = parent.String
	}
	return path, nil
}

func Descendants(q db.Querier, uid string) ([]string, error) {
	return column(q, `WITH RECURSIVE under(uid) AS (
		SELECT uid FROM subjects WHERE parent_uid = ?
		UNION SELECT s.uid FROM subjects s JOIN under ON s.parent_uid = under.uid)
		SELECT uid FROM under ORDER BY uid`, uid)
}

func exists(q db.Querier, uids ...string) error {
	for _, uid := range uids {
		var one int
		err := q.QueryRowContext(bg, "SELECT 1 FROM subjects WHERE uid = ?", uid).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// SetParent takes an empty parent as "no parent": that is how the user
// removes a parent, and the model may not put one back.
func SetParent(q db.Querier, child, parent string, o Origin) error {
	var current, origin sql.NullString
	err := q.QueryRowContext(bg, "SELECT parent_uid, parent_origin FROM subjects WHERE uid = ?", child).Scan(&current, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if parent == "" && !o.User {
		return ErrRemoveParent
	}
	var target any
	if parent != "" {
		if parent == child {
			return ErrCycle
		}
		if err := exists(q, parent); err != nil {
			return err
		}
		under, err := Descendants(q, child)
		if err != nil {
			return err
		}
		if slices.Contains(under, parent) {
			return ErrCycle
		}
		target = parent
	}
	if !o.User {
		switch {
		case origin.String == "user":
			return ErrUserSet
		case current.String == parent:
			return nil
		case current.Valid:
			return ErrSecondParent
		}
	}
	_, err = q.ExecContext(bg, "UPDATE subjects SET parent_uid = ?, parent_origin = ?, parent_episode_uid = ?, updated_at = ? WHERE uid = ?",
		target, o.name(), o.EpisodeUID, now(), child)
	return err
}

func Link(q db.Querier, source, typ, target string, o Origin) error {
	return writeLink(q, source, typ, target, true, o)
}

func Unlink(q db.Querier, source, typ, target string, o Origin) error {
	return writeLink(q, source, typ, target, false, o)
}

func writeLink(q db.Querier, source, typ, target string, active bool, o Origin) error {
	if typ != "depends_on" && typ != "about" {
		return ErrLinkType
	}
	if source == target {
		return ErrSelfLink
	}
	if err := exists(q, source, target); err != nil {
		return err
	}
	// Rewriting an unchanged link would bump updated_at and push it to sync again.
	var was bool
	var origin string
	err := q.QueryRowContext(bg, "SELECT active, origin FROM links WHERE source_uid = ? AND type = ? AND target_uid = ?", source, typ, target).Scan(&was, &origin)
	if err == nil && was == active && origin == o.name() {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	guard := ""
	if !o.User {
		guard = " WHERE links.origin = 'model'"
	}
	res, err := q.ExecContext(bg, `INSERT INTO links(source_uid, type, target_uid, active, origin, episode_uid, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_uid, type, target_uid) DO UPDATE SET active = excluded.active, origin = excluded.origin,
		episode_uid = excluded.episode_uid, updated_at = excluded.updated_at`+guard,
		source, typ, target, active, o.name(), o.EpisodeUID, now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserSet
	}
	return nil
}

func Neighbors(q db.Querier, uid string) (*Subject, []Subject, error) {
	s, err := Get(q, uid)
	if err != nil {
		return nil, nil, err
	}
	var parent *Subject
	if s.ParentUID != nil {
		if parent, err = Get(q, *s.ParentUID); err != nil {
			return nil, nil, err
		}
	}
	deps, err := query(q, "WHERE uid IN (SELECT target_uid FROM links WHERE source_uid = ? AND type = 'depends_on' AND active = 1) ORDER BY name, uid", uid)
	return parent, deps, err
}

func Tree(q db.Querier) ([]Node, error) {
	all, err := List(q)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(bg, "SELECT source_uid, type, target_uid FROM links WHERE active = 1 ORDER BY type, target_uid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := map[string][]Edge{}
	for rows.Next() {
		var source string
		var l Edge
		if err := rows.Scan(&source, &l.Type, &l.Target); err != nil {
			return nil, err
		}
		links[source] = append(links[source], l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	children := map[string][]Subject{}
	for _, s := range all {
		parent := ""
		if s.ParentUID != nil {
			parent = *s.ParentUID
		}
		children[parent] = append(children[parent], s)
	}
	var build func(parent string) []Node
	build = func(parent string) []Node {
		var nodes []Node
		for _, s := range children[parent] {
			nodes = append(nodes, Node{Subject: s, Children: build(s.UID), Links: links[s.UID]})
		}
		return nodes
	}
	return build(""), nil
}

// atomic takes BEGIN IMMEDIATE on a pool, since a deferred transaction that
// reads then writes fails at once (no busy wait) if another writer committed
// in between; inside a caller's transaction it uses a savepoint.
func atomic(q db.Querier, steps func(db.Querier) error) error {
	begin, commit, rollback := "SAVEPOINT merge", "RELEASE merge", "ROLLBACK TO merge; RELEASE merge"
	if d, ok := q.(*sql.DB); ok {
		conn, err := d.Conn(bg)
		if err != nil {
			return err
		}
		defer conn.Close()
		q, begin, commit, rollback = conn, "BEGIN IMMEDIATE", "COMMIT", "ROLLBACK"
	}
	if _, err := q.ExecContext(bg, begin); err != nil {
		return err
	}
	if err := steps(q); err != nil {
		_, undo := q.ExecContext(bg, rollback)
		return errors.Join(err, undo)
	}
	_, err := q.ExecContext(bg, commit)
	return err
}

type record struct {
	name, kind                    string
	aliases                       []string
	repo, parent, origin, episode sql.NullString
}

func load(q db.Querier, uid string) (record, error) {
	var r record
	var aliases string
	err := q.QueryRowContext(bg, "SELECT name, kind, aliases, repo, parent_uid, parent_origin, parent_episode_uid FROM subjects WHERE uid = ?", uid).
		Scan(&r.name, &r.kind, &aliases, &r.repo, &r.parent, &r.origin, &r.episode)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal([]byte(aliases), &r.aliases)
}

func Merge(q db.Querier, from, into string) error {
	if from == into {
		return ErrSelfMerge
	}
	return atomic(q, func(q db.Querier) error { return merge(q, from, into) })
}

func merge(q db.Querier, from, into string) error {
	f, err := load(q, from)
	if err != nil {
		return err
	}
	i, err := load(q, into)
	if err != nil {
		return err
	}
	fromUp, err := up(q, from)
	if err != nil {
		return err
	}
	intoUp, err := up(q, into)
	if err != nil {
		return err
	}
	path := intoUp[:slices.IndexFunc(intoUp, func(e edge) bool { return e.parent == from })+1]
	// Sync replays merges, so a merge never refuses the cycle it would make
	// when into lies under from: it drops a model edge of that path, else the
	// edge that pointed at from; into's user parent stays.
	fromUnderInto := slices.ContainsFunc(fromUp, func(e edge) bool { return e.parent == into })
	take, drop := false, ""
	switch {
	case len(path) == 1 || len(path) > 1 && path[0].origin != "user":
		take = true
	case len(path) > 1:
		drop = path[len(path)-1].child
		if k := slices.IndexFunc(path, func(e edge) bool { return e.origin != "user" }); k >= 0 {
			drop = path[k].child
		}
	case fromUnderInto:
	default:
		take = !i.parent.Valid && i.origin.String != "user"
	}
	if take {
		i.parent, i.origin, i.episode = f.parent, f.origin, f.episode
	}
	// Both under each other is a ring brought by sync (A under B under A):
	// from's parent then lies under into, so into becomes a root.
	if take && fromUnderInto {
		i.parent, i.origin, i.episode = sql.NullString{}, sql.NullString{}, sql.NullString{}
	}
	if !i.repo.Valid && f.repo.Valid {
		i.repo, i.kind = f.repo, "repo"
	}
	at := now()
	steps := [][]any{
		{"DELETE FROM subjects WHERE uid = ?", from},
		{"DELETE FROM subject_terms WHERE subject_uid = ?", from},
		{"UPDATE subjects SET kind = ?, repo = ?, aliases = ?, parent_uid = ?, parent_origin = ?, parent_episode_uid = ?, updated_at = ? WHERE uid = ?",
			i.kind, i.repo, encode(union(i.name, i.aliases, f.aliases, []string{f.name})), i.parent, i.origin, i.episode, at, into},
		{"UPDATE subjects SET parent_uid = ?, updated_at = ? WHERE parent_uid = ?", into, at, from},
		{"UPDATE points SET subject_uid = ?, updated_at = ? WHERE subject_uid = ?", into, at, from},
		{"UPDATE episodes SET updated_at = ? WHERE uid IN (SELECT episode_uid FROM episode_subjects WHERE subject_uid = ?)", at, from},
		{"UPDATE repo_cache SET subject_uid = ? WHERE subject_uid = ?", into, from},
	}
	for _, table := range []string{"episode_subjects", "session_repos", "injected"} {
		steps = append(steps,
			[]any{"UPDATE OR IGNORE " + table + " SET subject_uid = ? WHERE subject_uid = ?", into, from},
			[]any{"DELETE FROM " + table + " WHERE subject_uid = ?", from})
	}
	steps = append(steps,
		[]any{"UPDATE OR IGNORE syntheses SET subject_uid = ?, updated_at = ? WHERE subject_uid = ?", into, at, from},
		[]any{"UPDATE syntheses SET stale = 1, updated_at = ? WHERE subject_uid = ? AND EXISTS (SELECT 1 FROM syntheses WHERE subject_uid = ?)", at, into, from},
		[]any{"DELETE FROM syntheses WHERE subject_uid = ?", from})
	for _, step := range steps {
		if _, err := q.ExecContext(bg, step[0].(string), step[1:]...); err != nil {
			return err
		}
	}
	if err := reindex(q, into); err != nil {
		return err
	}
	if drop != "" {
		if _, err := q.ExecContext(bg, "UPDATE subjects SET parent_uid = NULL, parent_episode_uid = NULL, updated_at = ? WHERE uid = ?", at, drop); err != nil {
			return err
		}
	}
	if err := moveLinks(q, from, into, at); err != nil {
		return err
	}
	_, err = q.ExecContext(bg, "INSERT OR REPLACE INTO merged(from_uid, into_uid, at) VALUES (?, ?, ?)", from, into, at)
	return err
}

type linkRecord struct {
	source, typ, target, origin, updated string
	active                               bool
	episode                              sql.NullString
}

func moveLinks(q db.Querier, from, into, at string) error {
	rows, err := q.QueryContext(bg, "SELECT source_uid, type, target_uid, active, origin, episode_uid, updated_at FROM links WHERE source_uid = ?1 OR target_uid = ?1", from)
	if err != nil {
		return err
	}
	var moved []linkRecord
	for rows.Next() {
		var l linkRecord
		if err := rows.Scan(&l.source, &l.typ, &l.target, &l.active, &l.origin, &l.episode, &l.updated); err != nil {
			rows.Close()
			return err
		}
		moved = append(moved, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err := q.ExecContext(bg, "DELETE FROM links WHERE source_uid = ?1 OR target_uid = ?1", from); err != nil {
		return err
	}
	for _, l := range moved {
		if l.source == from {
			l.source = into
		}
		if l.target == from {
			l.target = into
		}
		if l.source == l.target {
			continue
		}
		var origin, updated string
		err := q.QueryRowContext(bg, "SELECT origin, updated_at FROM links WHERE source_uid = ? AND type = ? AND target_uid = ?", l.source, l.typ, l.target).Scan(&origin, &updated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Same rule as sync: a user link beats a model one, else the most recent wins.
		if err == nil && !(l.origin == "user" && origin == "model" || l.origin == origin && l.updated > updated) {
			continue
		}
		if _, err := q.ExecContext(bg, "INSERT OR REPLACE INTO links(source_uid, type, target_uid, active, origin, episode_uid, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			l.source, l.typ, l.target, l.active, l.origin, l.episode, at); err != nil {
			return err
		}
	}
	return nil
}

// Only subjects merge by name: a repo shares its name with its forks, and a
// person may share a first name with a tool.
func MergeDuplicates(q db.Querier) (int, error) {
	all, err := query(q, "WHERE kind = 'subject' ORDER BY uid")
	if err != nil {
		return 0, err
	}
	return mergeDuplicates(q, all)
}

func mergeDuplicates(q db.Querier, all []Subject) (int, error) {
	kept := map[string]string{}
	n := 0
	for _, s := range all {
		into, ok := kept[Normalize(s.Name)]
		if !ok {
			kept[Normalize(s.Name)] = s.UID
			continue
		}
		// A concurrent pass may already have merged either subject.
		err := Merge(q, s.UID, into)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
