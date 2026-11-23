// Package search answers the read side of the memory: full-text search and the session views, all bounded by a scope.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/projects"
)

const (
	markOn  = "\x01"
	markOff = "\x02"

	from  = " FROM messages m LEFT JOIN projects p ON p.id = m.project"
	label = "COALESCE(p.label, 'unsorted')"
	utc   = "2006-01-02T15:04:05.000Z"
)

var (
	ErrEmptyQuery = errors.New("requête vide")
	background    = context.Background()
)

type ErrAmbiguousSession struct {
	Ref string
	IDs []string
}

func (e *ErrAmbiguousSession) Error() string {
	return fmt.Sprintf("début d'identifiant ambigu : %s (%s)", e.Ref, strings.Join(e.IDs, ", "))
}

type Segment struct {
	Text  string
	Match bool
}

type Hit struct {
	TS, Project, Label, SessionID, Role string
	Segments                            []Segment
}

// Since and Until are calendar days in their own location, both included.
type Filter struct {
	Project, Label string
	Since, Until   *time.Time
	Limit          int
}

type ListFilter struct {
	Project, Label, Before string
	Limit                  int
}

type SessionRow struct {
	ID                 string
	Title              *string
	StartedAt, EndedAt string
	Projects, Labels   []string
	Messages           int
}

type Message struct {
	ID                                        int64
	SessionID, TS, Role, Project, Label, Text string
	GitBranch                                 *string
}

func ToFTSQuery(text string) (string, error) {
	// Python's str.split also breaks on the four ASCII separators \x1c-\x1f.
	tokens := strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
	if len(tokens) == 0 {
		return "", ErrEmptyQuery
	}
	for i, t := range tokens {
		t = strings.ReplaceAll(t, `"`, `""`)
		tokens[i] = `"` + strings.ReplaceAll(t, "\x00", "�") + `"`
	}
	return strings.Join(tokens, " "), nil
}

func dayStart(t time.Time, plus int) string {
	y, m, d := t.Date()
	return time.Date(y, m, d+plus, 0, 0, 0, 0, t.Location()).UTC().Format(utc)
}

func visible(s projects.Scope, project, lbl string, since, until *time.Time) (clauses []string, args []any) {
	if s.Projects != nil {
		ids := make([]string, 0, len(s.Projects))
		for id := range s.Projects {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		clauses = append(clauses, "m.project IN ("+placeholders(len(ids))+")")
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if project != "" {
		clauses = append(clauses, "m.project = ?")
		args = append(args, project)
	}
	if lbl != "" {
		clauses = append(clauses, label+" = ?")
		args = append(args, lbl)
	}
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

func Search(q db.Querier, text string, s projects.Scope, f Filter) ([]Hit, error) {
	match, err := ToFTSQuery(text)
	if err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	clauses, args := visible(s, f.Project, f.Label, f.Since, f.Until)
	where := ""
	for _, c := range clauses {
		where += " AND " + c
	}
	args = append([]any{markOn, markOff, match}, args...)
	return collect(q, "SELECT m.ts, m.project, "+label+", m.session_id, m.role,"+
		" snippet(messages_fts, 0, ?, ?, '…', 16)"+
		" FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid"+
		" LEFT JOIN projects p ON p.id = m.project"+
		" WHERE messages_fts MATCH ?"+where+
		" ORDER BY bm25(messages_fts), m.ts DESC LIMIT ?", append(args, limit),
		func(r *sql.Rows) (Hit, error) {
			var h Hit
			var excerpt string
			err := r.Scan(&h.TS, &h.Project, &h.Label, &h.SessionID, &h.Role, &excerpt)
			h.Segments = segments(excerpt)
			return h, err
		})
}

func sessionRows(q db.Querier, s projects.Scope, ids []string) (map[string]*SessionRow, error) {
	clauses, args := visible(s, "", "", nil, nil)
	clauses = append([]string{"m.session_id IN (" + placeholders(len(ids)) + ")"}, clauses...)
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}
	type group struct {
		session, project, label, first, last string
		title                                sql.NullString
		n                                    int
	}
	groups, err := collect(q, "SELECT m.session_id, s.title, m.project, "+label+", MIN(m.ts), MAX(m.ts), COUNT(*)"+
		from+" LEFT JOIN sessions s ON s.id = m.session_id WHERE "+and(clauses)+
		" GROUP BY m.session_id, m.project", append(idArgs, args...),
		func(r *sql.Rows) (g group, err error) {
			err = r.Scan(&g.session, &g.title, &g.project, &g.label, &g.first, &g.last, &g.n)
			return g, err
		})
	if err != nil {
		return nil, err
	}
	found := map[string]*SessionRow{}
	for _, g := range groups {
		row := found[g.session]
		if row == nil {
			row = &SessionRow{ID: g.session, StartedAt: g.first, EndedAt: g.last}
			if g.title.Valid {
				row.Title = &g.title.String
			}
			found[g.session] = row
		}
		row.StartedAt, row.EndedAt = min(row.StartedAt, g.first), max(row.EndedAt, g.last)
		row.Projects = append(row.Projects, g.project)
		if !slices.Contains(row.Labels, g.label) {
			row.Labels = append(row.Labels, g.label)
		}
		row.Messages += g.n
	}
	for _, row := range found {
		slices.Sort(row.Projects)
		slices.Sort(row.Labels)
	}
	if s.Projects != nil {
		return found, hideTitles(q, s, found)
	}
	return found, nil
}

// A title written from a message outside the scope would leak what that message said.
func hideTitles(q db.Querier, s projects.Scope, found map[string]*SessionRow) error {
	ids := make([]any, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	inside := make([]any, 0, len(s.Projects))
	for id := range s.Projects {
		inside = append(inside, id)
	}
	partly, err := collect(q, "SELECT DISTINCT session_id FROM messages WHERE session_id IN ("+placeholders(len(ids))+
		") AND project NOT IN ("+placeholders(len(inside))+")", append(ids, inside...),
		func(r *sql.Rows) (id string, err error) { return id, r.Scan(&id) })
	for _, id := range partly {
		found[id].Title = nil
	}
	return err
}

func rowsOf(found map[string]*SessionRow) []SessionRow {
	out := make([]SessionRow, 0, len(found))
	for _, r := range found {
		out = append(out, *r)
	}
	return out
}

func DayView(q db.Querier, day time.Time, s projects.Scope) ([]SessionRow, error) {
	clauses, args := visible(s, "", "", &day, &day)
	ids, err := collect(q, "SELECT DISTINCT m.session_id"+from+" WHERE "+and(clauses), args,
		func(r *sql.Rows) (id string, err error) { return id, r.Scan(&id) })
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	found, err := sessionRows(q, s, ids)
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
func ListSessions(q db.Querier, s projects.Scope, f ListFilter) ([]SessionRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	clauses, args := visible(s, "", "", nil, nil)
	picked, pickedArgs := visible(projects.Everything, f.Project, f.Label, nil, nil)
	var having []string
	if len(picked) > 0 {
		having = append(having, "SUM("+and(picked)+") > 0")
	}
	havingArgs := pickedArgs
	if f.Before != "" {
		having = append(having, "MAX(m.ts) < ?")
		havingArgs = append(havingArgs, f.Before)
	}
	base := "SELECT m.session_id, MAX(m.ts)" + from + " WHERE " + and(clauses) + " GROUP BY m.session_id"
	tail := ""
	if len(having) > 0 {
		tail = " HAVING " + strings.Join(having, " AND ")
	}
	type page struct{ id, last string }
	scan := func(r *sql.Rows) (p page, err error) { return p, r.Scan(&p.id, &p.last) }
	args = append(args, havingArgs...)
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
	found, err := sessionRows(q, s, ids)
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
func ResolveSession(q db.Querier, ref string, s projects.Scope) (string, error) {
	if ref == "" {
		return "", nil
	}
	clauses, args := visible(s, "", "", nil, nil)
	where := ""
	for _, c := range clauses {
		where += " AND " + c
	}
	ids, err := collect(q, "SELECT DISTINCT m.session_id"+from+
		` WHERE (m.session_id = ? OR m.session_id LIKE ? ESCAPE '\')`+where+" ORDER BY m.session_id",
		append([]any{ref, escapeLike(ref) + "%"}, args...),
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

// SessionView returns a nil row when the session is absent or has no visible message.
func SessionView(q db.Querier, ref string, s projects.Scope) (*SessionRow, []Message, error) {
	id, err := ResolveSession(q, ref, s)
	if err != nil || id == "" {
		return nil, nil, err
	}
	found, err := sessionRows(q, s, []string{id})
	if err != nil {
		return nil, nil, err
	}
	clauses, args := visible(s, "", "", nil, nil)
	where := ""
	for _, c := range clauses {
		where += " AND " + c
	}
	messages, err := collect(q, "SELECT m.id, m.session_id, m.ts, m.role, m.project, "+label+", m.git_branch, m.text"+
		from+" WHERE m.session_id = ?"+where+" ORDER BY m.ts, m.id", append([]any{id}, args...),
		func(r *sql.Rows) (m Message, err error) {
			var branch sql.NullString
			err = r.Scan(&m.ID, &m.SessionID, &m.TS, &m.Role, &m.Project, &m.Label, &branch, &m.Text)
			if branch.Valid {
				m.GitBranch = &branch.String
			}
			return m, err
		})
	return found[id], messages, err
}

// Names maps project identities to display names; an identity without a
// projects row (degraded localization) shows its last segment.
func Names(q db.Querier, ids []string) (map[string]string, error) {
	names := map[string]string{}
	for _, id := range ids {
		names[id] = id[strings.LastIndexAny(id, "/:")+1:]
	}
	if len(ids) == 0 {
		return names, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := collect(q, "SELECT id, name FROM projects WHERE id IN ("+placeholders(len(ids))+")", args,
		func(r *sql.Rows) (pair [2]string, err error) { return pair, r.Scan(&pair[0], &pair[1]) })
	for _, p := range rows {
		names[p[0]] = p[1]
	}
	return names, err
}
