// Package search answers the read side of the memory: full-text search and the session views.
package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/subjects"
)

const (
	markOn  = "\x01"
	markOff = "\x02"

	from = " FROM messages m"
)

var (
	ErrEmptyQuery = errors.New("empty query")
	background    = context.Background()
)

type ErrAmbiguousSession struct {
	Ref string
	IDs []string
}

func (e *ErrAmbiguousSession) Error() string {
	return fmt.Sprintf("ambiguous id prefix: %s (%s)", e.Ref, strings.Join(e.IDs, ", "))
}

type Segment struct {
	Text  string
	Match bool
}

type Hit struct {
	TS, SessionID, Role string
	Segments            []Segment
	Subjects            []string
}

// Subject is a name, keyword or uid; Since and Until are calendar days in their own location, both included.
type Filter struct {
	Subject      string
	Since, Until *time.Time
	Limit        int
}

type ListFilter struct {
	Subject string
	Before  string
	Limit   int
}

type SessionRow struct {
	ID                 string
	Title              *string
	StartedAt, EndedAt string
	Messages           int
	Subjects           []string
}

type Message struct {
	ID                        int64
	SessionID, TS, Role, Text string
	GitBranch                 *string
}

// Terms splits like Python's str.split, which also breaks on the four ASCII separators \x1c-\x1f.
func Terms(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

func quote(term string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(term, `"`, `""`), "\x00", "�") + `"`
}

func ToFTSQuery(text string) (string, error) {
	tokens := Terms(text)
	if len(tokens) == 0 {
		return "", ErrEmptyQuery
	}
	for i, t := range tokens {
		tokens[i] = quote(t)
	}
	return strings.Join(tokens, " "), nil
}

// MatchAny matches a row holding at least one term; ToFTSQuery wants all of them.
func MatchAny(terms []string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = quote(t)
	}
	return strings.Join(quoted, " OR ")
}

func about(q db.Querier, term string) (clause string, args []any, err error) {
	if term == "" {
		return "", nil, nil
	}
	tree, err := subjects.TreeByTerm(q, term)
	if err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(append([]string{}, tree...))
	return "m.session_id IN (SELECT e.session_id FROM episodes e JOIN episode_subjects es ON es.episode_uid = e.uid" +
		" WHERE es.subject_uid IN (SELECT value FROM json_each(?)))", []any{string(raw)}, err
}

func dayStart(t time.Time, plus int) string {
	y, m, d := t.Date()
	return time.Date(y, m, d+plus, 0, 0, 0, 0, t.Location()).UTC().Format(db.UTCFormat)
}

func visible(since, until *time.Time) (clauses []string, args []any) {
	if since != nil {
		clauses = append(clauses, "m.ts >= ?")
		args = append(args, dayStart(*since, 0))
	}
	if until != nil {
		clauses = append(clauses, "m.ts < ?")
		args = append(args, dayStart(*until, 1))
	}
	return clauses, args
}

func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func and(clauses []string) string {
	if len(clauses) == 0 {
		return "1"
	}
	return strings.Join(clauses, " AND ")
}

func segments(snippet string) []Segment {
	var out []Segment
	for i, chunk := range strings.Split(snippet, markOn) {
		highlighted, plain, closed := strings.Cut(chunk, markOff)
		if i > 0 && closed {
			out = append(out, Segment{highlighted, true})
			chunk = plain
		}
		if chunk != "" {
			out = append(out, Segment{chunk, false})
		}
	}
	return out
}

func collect[T any](q db.Querier, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := q.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func Search(q db.Querier, text string, f Filter) ([]Hit, error) {
	match, err := ToFTSQuery(text)
	if err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	clauses, args := visible(f.Since, f.Until)
	subject, subjectArgs, err := about(q, f.Subject)
	if err != nil {
		return nil, err
	}
	if subject != "" {
		clauses, args = append(clauses, subject), append(args, subjectArgs...)
	}
	where := ""
	for _, c := range clauses {
		where += " AND " + c
	}
	args = append([]any{markOn, markOff, match}, args...)
	hits, err := collect(q, "SELECT m.ts, m.session_id, m.role,"+
		" snippet(messages_fts, 0, ?, ?, '…', 16)"+
		" FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid"+
		" WHERE messages_fts MATCH ?"+where+
		" ORDER BY bm25(messages_fts), m.ts DESC LIMIT ?", append(args, limit),
		func(r *sql.Rows) (Hit, error) {
			var h Hit
			var excerpt string
			err := r.Scan(&h.TS, &h.SessionID, &h.Role, &excerpt)
			h.Segments = segments(excerpt)
			return h, err
		})
	if err != nil || len(hits) == 0 {
		return nil, err
	}
	sessions := make([]string, len(hits))
	for i, h := range hits {
		sessions[i] = h.SessionID
	}
	names, err := sessionSubjects(q, sessions)
	for i := range hits {
		hits[i].Subjects = names[hits[i].SessionID]
	}
	return hits, err
}

func sessionSubjects(q db.Querier, sessions []string) (map[string][]string, error) {
	raw, err := json.Marshal(sessions)
	if err != nil {
		return nil, err
	}
	type pair struct{ session, name string }
	pairs, err := collect(q, "SELECT DISTINCT e.session_id, s.name FROM episodes e"+
		" JOIN episode_subjects es ON es.episode_uid = e.uid JOIN subjects s ON s.uid = es.subject_uid"+
		" WHERE e.session_id IN (SELECT value FROM json_each(?)) ORDER BY s.name, s.uid", []any{string(raw)},
		func(r *sql.Rows) (p pair, err error) { return p, r.Scan(&p.session, &p.name) })
	names := map[string][]string{}
	for _, p := range pairs {
		names[p.session] = append(names[p.session], p.name)
	}
	return names, err
}

func sessionRows(q db.Querier, ids []string) (map[string]*SessionRow, error) {
	clauses := []string{"m.session_id IN (" + placeholders(len(ids)) + ")"}
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}
	type group struct {
		session, first, last string
		title                sql.NullString
		n                    int
	}
	groups, err := collect(q, "SELECT m.session_id, s.title, MIN(m.ts), MAX(m.ts), COUNT(*)"+
		from+" LEFT JOIN sessions s ON s.id = m.session_id WHERE "+and(clauses)+
		" GROUP BY m.session_id", idArgs,
		func(r *sql.Rows) (g group, err error) {
			err = r.Scan(&g.session, &g.title, &g.first, &g.last, &g.n)
			return g, err
		})
	if err != nil {
		return nil, err
	}
	found := map[string]*SessionRow{}
	for _, g := range groups {
		row := &SessionRow{ID: g.session, StartedAt: g.first, EndedAt: g.last, Messages: g.n}
		if g.title.Valid {
			row.Title = &g.title.String
		}
		found[g.session] = row
	}
	names, err := sessionSubjects(q, ids)
	for id, row := range found {
		row.Subjects = names[id]
	}
	return found, err
}

func rowsOf(found map[string]*SessionRow) []SessionRow {
	out := make([]SessionRow, 0, len(found))
	for _, r := range found {
		out = append(out, *r)
	}
	return out
}

func DayView(q db.Querier, day time.Time) ([]SessionRow, error) {
	clauses, args := visible(&day, &day)
	ids, err := collect(q, "SELECT DISTINCT m.session_id"+from+" WHERE "+and(clauses), args,
		func(r *sql.Rows) (id string, err error) { return id, r.Scan(&id) })
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	found, err := sessionRows(q, ids)
	if err != nil {
		return nil, err
	}
	rows := rowsOf(found)
	slices.SortFunc(rows, func(a, b SessionRow) int {
		return strings.Compare(a.StartedAt+"\x00"+a.ID, b.StartedAt+"\x00"+b.ID)
	})
	return rows, nil
}

// ListSessions pages by the last visible message; sessions tied with the last
// one of the page are kept so the next page, cut strictly before, loses none.
func ListSessions(q db.Querier, f ListFilter) ([]SessionRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	var having []string
	subject, args, err := about(q, f.Subject)
	if err != nil {
		return nil, err
	}
	if f.Before != "" {
		having = append(having, "MAX(m.ts) < ?")
		args = append(args, f.Before)
	}
	where := ""
	if subject != "" {
		where = " WHERE " + subject
	}
	base := "SELECT m.session_id, MAX(m.ts)" + from + where + " GROUP BY m.session_id"
	tail := ""
	if len(having) > 0 {
		tail = " HAVING " + strings.Join(having, " AND ")
	}
	type page struct{ id, last string }
	scan := func(r *sql.Rows) (p page, err error) { return p, r.Scan(&p.id, &p.last) }
	rows, err := collect(q, base+tail+" ORDER BY MAX(m.ts) DESC, m.session_id LIMIT ?", append(slices.Clone(args), limit), scan)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	glue := " HAVING "
	if tail != "" {
		glue = " AND "
	}
	tied, err := collect(q, base+tail+glue+"MAX(m.ts) = ?", append(args, rows[len(rows)-1].last), scan)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range append(rows, tied...) {
		if !slices.Contains(ids, p.id) {
			ids = append(ids, p.id)
		}
	}
	found, err := sessionRows(q, ids)
	if err != nil {
		return nil, err
	}
	out := rowsOf(found)
	slices.SortFunc(out, func(a, b SessionRow) int {
		if c := strings.Compare(b.EndedAt, a.EndedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func escapeLike(text string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(text)
}

// ResolveSession returns "" when no visible session matches; an exact id wins over longer ids it prefixes.
func ResolveSession(q db.Querier, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	// A rebuild without its transcript leaves a session with episodes and no message.
	ids, err := collect(q, "SELECT id FROM (SELECT session_id AS id FROM messages UNION SELECT session_id FROM episodes)"+
		` WHERE (id = ? OR id LIKE ? ESCAPE '\')`+" ORDER BY id",
		[]any{ref, escapeLike(ref) + "%"},
		func(r *sql.Rows) (id string, err error) { return id, r.Scan(&id) })
	if err != nil {
		return "", err
	}
	if slices.Contains(ids, ref) {
		return ref, nil
	}
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	}
	return "", &ErrAmbiguousSession{Ref: ref, IDs: ids}
}

// SessionView returns a nil row when the session has neither a message nor an episode.
func SessionView(q db.Querier, ref string) (*SessionRow, []Message, error) {
	id, err := ResolveSession(q, ref)
	if err != nil || id == "" {
		return nil, nil, err
	}
	found, err := sessionRows(q, []string{id})
	if err != nil {
		return nil, nil, err
	}
	if found[id] == nil {
		row := &SessionRow{ID: id}
		if err := q.QueryRowContext(background, "SELECT MIN(start_ts), MAX(end_ts), (SELECT title FROM sessions WHERE id = ?1)"+
			" FROM episodes WHERE session_id = ?1", id).Scan(&row.StartedAt, &row.EndedAt, &row.Title); err != nil {
			return nil, nil, err
		}
		names, err := sessionSubjects(q, []string{id})
		if err != nil {
			return nil, nil, err
		}
		row.Subjects = names[id]
		found[id] = row
	}
	messages, err := collect(q, "SELECT m.id, m.session_id, m.ts, m.role, m.git_branch, m.text"+
		from+" WHERE m.session_id = ? ORDER BY m.ts, m.id", []any{id},
		func(r *sql.Rows) (m Message, err error) {
			var branch sql.NullString
			err = r.Scan(&m.ID, &m.SessionID, &m.TS, &m.Role, &branch, &m.Text)
			if branch.Valid {
				m.GitBranch = &branch.String
			}
			return m, err
		})
	return found[id], messages, err
}
