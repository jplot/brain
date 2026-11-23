package subjects

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jplot/brain/internal/db"
)

var (
	ErrCycle        = errors.New("cycle dans l'arbre des sujets")
	ErrSecondParent = errors.New("le sujet a déjà un parent")
	ErrUserSet      = errors.New("posé par l'utilisateur, le résumeur n'y touche pas")
	ErrLinkType     = errors.New("type de lien inconnu")
	ErrSelfMerge    = errors.New("un sujet ne se fusionne pas avec lui-même")
)

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
	if err := exists(q, source, target); err != nil {
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
	underFrom, err := Descendants(q, from)
	if err != nil {
		return err
	}
	underInto, err := Descendants(q, into)
	if err != nil {
		return err
	}
	// Sync replays merges, so a merge never refuses a cycle: into leaves from's
	// subtree, and never takes a parent from its own.
	if !slices.Contains(underInto, from) && (slices.Contains(underFrom, into) || !i.parent.Valid && i.origin.String != "user") {
		i.parent, i.origin, i.episode = f.parent, f.origin, f.episode
	}
	if !i.repo.Valid && f.repo.Valid {
		i.repo, i.kind = f.repo, "repo"
	}
	at := now()
	steps := [][]any{
		{"DELETE FROM subjects WHERE uid = ?", from},
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
	for _, step := range steps {
		if _, err := q.ExecContext(bg, step[0].(string), step[1:]...); err != nil {
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

// Two repos never merge with each other: forks share a name (exemple/outil,
// autre/outil).
func MergeDuplicates(q db.Querier) (int, error) {
	all, err := query(q, "ORDER BY uid")
	if err != nil {
		return 0, err
	}
	var kept []Subject
	n := 0
	for _, s := range all {
		i := slices.IndexFunc(kept, func(k Subject) bool {
			return Normalize(k.Name) == Normalize(s.Name) && (k.Repo == nil || s.Repo == nil)
		})
		if i < 0 {
			kept = append(kept, s)
			continue
		}
		if err := Merge(q, s.UID, kept[i].UID); err != nil {
			return n, err
		}
		if kept[i].Repo == nil {
			kept[i].Repo = s.Repo
		}
		n++
	}
	return n, nil
}
