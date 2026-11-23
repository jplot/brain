package subjects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/jplot/brain/internal/db"
)

var (
	bg          = context.Background()
	ErrNotFound = errors.New("sujet inconnu")
)

type Subject struct {
	UID, Kind, Name string
	Aliases         []string
	Repo, ParentUID *string
	UpdatedAt       string
}

func now() string { return db.Stamp(time.Time{}) }

func Normalize(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(name) {
		switch {
		case unicode.Is(unicode.Mn, r):
		// NFKD leaves œ and æ whole, and "cœur" must find "coeur".
		case r == 'œ' || r == 'Œ':
			b.WriteString("oe")
		case r == 'æ' || r == 'Æ':
			b.WriteString("ae")
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func union(name string, lists ...[]string) []string {
	seen := map[string]bool{Normalize(name): true}
	out := []string{}
	for _, list := range lists {
		for _, alias := range list {
			if key := Normalize(alias); !seen[key] {
				seen[key] = true
				out = append(out, alias)
			}
		}
	}
	return out
}

func encode(aliases []string) string {
	b, _ := json.Marshal(aliases)
	return string(b)
}

const columns = "uid, kind, name, aliases, repo, parent_uid, updated_at"

func scan(row interface{ Scan(...any) error }) (Subject, error) {
	var s Subject
	var aliases string
	var repo, parent sql.NullString
	if err := row.Scan(&s.UID, &s.Kind, &s.Name, &aliases, &repo, &parent, &s.UpdatedAt); err != nil {
		return s, err
	}
	if repo.Valid {
		s.Repo = &repo.String
	}
	if parent.Valid {
		s.ParentUID = &parent.String
	}
	return s, json.Unmarshal([]byte(aliases), &s.Aliases)
}

func query(q db.Querier, where string, args ...any) ([]Subject, error) {
	rows, err := q.QueryContext(bg, "SELECT "+columns+" FROM subjects "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subject
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func Get(q db.Querier, uid string) (*Subject, error) {
	s, err := scan(q.QueryRowContext(bg, "SELECT "+columns+" FROM subjects WHERE uid = ?", uid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func List(q db.Querier) ([]Subject, error) { return query(q, "ORDER BY name, uid") }

// FindByTerm also takes a uid, so a caller can pass what the user typed.
func FindByTerm(q db.Querier, term string) ([]Subject, error) {
	return query(q, "WHERE uid = ?1 OR uid IN (SELECT subject_uid FROM subject_terms WHERE term = ?2) ORDER BY uid", term, Normalize(term))
}

// ByTerms maps each normalized term to the subjects named or aliased by it, in one indexed query.
func ByTerms(q db.Querier, terms []string) (map[string][]Subject, error) {
	raw, err := json.Marshal(terms)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(bg, "SELECT t.term, s.uid, s.kind, s.name, s.aliases, s.repo, s.parent_uid, s.updated_at"+
		" FROM subject_terms t JOIN subjects s ON s.uid = t.subject_uid"+
		" WHERE t.term IN (SELECT value FROM json_each(?)) ORDER BY t.term, s.uid", string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Subject{}
	for rows.Next() {
		var term string
		var s Subject
		var aliases string
		var repo, parent sql.NullString
		if err := rows.Scan(&term, &s.UID, &s.Kind, &s.Name, &aliases, &repo, &parent, &s.UpdatedAt); err != nil {
			return nil, err
		}
		if repo.Valid {
			s.Repo = &repo.String
		}
		if parent.Valid {
			s.ParentUID = &parent.String
		}
		if err := json.Unmarshal([]byte(aliases), &s.Aliases); err != nil {
			return nil, err
		}
		out[term] = append(out[term], s)
	}
	return out, rows.Err()
}

// reindex runs after every write of a name or an alias: the hook looks terms up here, never in Go.
func reindex(q db.Querier, uid string) error {
	s, err := Get(q, uid)
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(bg, "DELETE FROM subject_terms WHERE subject_uid = ?", uid); err != nil {
		return err
	}
	for _, name := range append([]string{s.Name}, s.Aliases...) {
		if term := Normalize(name); term != "" {
			if _, err := q.ExecContext(bg, "INSERT OR IGNORE INTO subject_terms(term, subject_uid) VALUES (?, ?)", term, uid); err != nil {
				return err
			}
		}
	}
	return nil
}

// RebuildTerms fills subject_terms for subjects written before the table existed or by a sync.
func RebuildTerms(q db.Querier) error {
	if _, err := q.ExecContext(bg, "DELETE FROM subject_terms"); err != nil {
		return err
	}
	all, err := List(q)
	if err != nil {
		return err
	}
	for _, s := range all {
		if err := reindex(q, s.UID); err != nil {
			return err
		}
	}
	return nil
}

// EnsureRepo relies on UNIQUE(repo): two indexers racing on the same new
// repository both insert, one is ignored, both read the same uid.
func EnsureRepo(q db.Querier, identity, name string) (string, error) {
	at := now()
	res, err := q.ExecContext(bg, "INSERT OR IGNORE INTO subjects(uid, kind, name, aliases, repo, created_at, updated_at) VALUES (?, 'repo', ?, '[]', ?, ?, ?)",
		db.NewUID(), name, identity, at, at)
	if err != nil {
		return "", err
	}
	var uid string
	if err := q.QueryRowContext(bg, "SELECT uid FROM subjects WHERE repo = ?", identity).Scan(&uid); err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		err = reindex(q, uid)
	}
	return uid, err
}

func Create(q db.Querier, name string, aliases []string, parent *string) (string, error) {
	all, err := query(q, "WHERE kind = 'subject' ORDER BY uid")
	if err != nil {
		return "", err
	}
	for _, s := range all {
		if Normalize(s.Name) == Normalize(name) {
			return s.UID, nil
		}
	}
	var origin any
	if parent != nil {
		if _, err := Get(q, *parent); err != nil {
			return "", err
		}
		origin = "model"
	}
	uid, at := db.NewUID(), now()
	_, err = q.ExecContext(bg, "INSERT INTO subjects(uid, kind, name, aliases, parent_uid, parent_origin, created_at, updated_at) VALUES (?, 'subject', ?, ?, ?, ?, ?, ?)",
		uid, name, encode(union(name, aliases)), parent, origin, at, at)
	if err != nil {
		return "", err
	}
	return uid, reindex(q, uid)
}

func Rename(q db.Querier, uid, name string) error {
	res, err := q.ExecContext(bg, "UPDATE subjects SET name = ?, updated_at = ? WHERE uid = ?", name, now(), uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return reindex(q, uid)
}

func AddAliases(q db.Querier, uid string, aliases []string) error {
	s, err := Get(q, uid)
	if err != nil {
		return err
	}
	merged := union(s.Name, s.Aliases, aliases)
	if slices.Equal(merged, s.Aliases) {
		return nil
	}
	_, err = q.ExecContext(bg, "UPDATE subjects SET aliases = ?, updated_at = ? WHERE uid = ?", encode(merged), now(), uid)
	if err != nil {
		return err
	}
	return reindex(q, uid)
}
