package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/mask"
	"github.com/jplot/brain/internal/memo"
	"github.com/jplot/brain/internal/memory"
	"github.com/jplot/brain/internal/search"
	"github.com/jplot/brain/internal/subjects"
)

const (
	maxPointText = 2000
	lockWait     = 2 * time.Second
	allPoints    = 1 << 30
)

var (
	background = context.Background()
	digits     = regexp.MustCompile(`^[0-9]{1,6}$`)
	kinds      = []string{"topic", "decision", "pending", "fact"}
	user       = subjects.Origin{User: true}
)

type request struct {
	query url.Values
	body  map[string]json.RawMessage
	ref   string
}

type handler func(*server, *request) (any, error)

var routes = map[string]handler{
	"GET /api/refresh":          refresh,
	"GET /api/sessions":         sessions,
	"GET /api/search":           searchMessages,
	"GET /api/points":           points,
	"GET /api/memo":             memoText,
	"GET /api/counts":           counts,
	"GET /api/subjects":         tree,
	"POST /api/points/correct":  correct,
	"POST /api/points/close":    closePoint,
	"POST /api/points/forget":   forgetPoint,
	"POST /api/sessions/forget": forgetSession,
	"POST /api/subjects/rename": rename,
	"POST /api/subjects/merge":  merge,
	"POST /api/subjects/parent": move,
	"POST /api/subjects/link":   link,
	"POST /api/subjects/unlink": unlink,
}

const sessionPrefix = "/api/sessions/"

func (s *server) route(method, path string, query url.Values) (string, handler, *request) {
	req := &request{query: query}
	if method == http.MethodGet && strings.HasPrefix(path, sessionPrefix) {
		req.ref = path[len(sessionPrefix):]
		if req.ref == "" || strings.Contains(req.ref, "/") {
			return "session", nil, req
		}
		return "session", sessionView, req
	}
	return strings.TrimPrefix(path, "/api/"), routes[method+" "+path], req
}

func classify(err error) (int, string) {
	var r *reply
	var missingPoint *memory.ErrPointNotFound
	var ambiguousSession *search.ErrAmbiguousSession
	var unknownSubject *subjects.ErrUnknownSubject
	var ambiguousSubject *subjects.ErrAmbiguousSubject
	switch {
	case errors.As(err, &r):
		return r.status, r.message
	case errors.As(err, &missingPoint), errors.As(err, &unknownSubject), errors.Is(err, subjects.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.As(err, &ambiguousSession), errors.As(err, &ambiguousSubject), errors.Is(err, search.ErrEmptyQuery),
		errors.Is(err, subjects.ErrCycle), errors.Is(err, subjects.ErrSelfLink), errors.Is(err, subjects.ErrSelfMerge),
		errors.Is(err, subjects.ErrLinkType):
		return http.StatusBadRequest, err.Error()
	}
	return http.StatusInternalServerError, "internal error"
}

func (r *request) param(key string) (string, bool) {
	values, ok := r.query[key]
	if !ok {
		return "", false
	}
	return values[0], true
}

func (r *request) bounded(key string, fallback, low, high int) (int, error) {
	value, ok := r.param(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if !digits.MatchString(value) || err != nil || n < low || n > high {
		return 0, fail(http.StatusBadRequest, key+" invalid")
	}
	return n, nil
}

func (r *request) limit(fallback int) (int, error) {
	return r.bounded("limit", fallback, 1, 200)
}

func (r *request) day(key string) (*time.Time, error) {
	value, ok := r.param(key)
	if !ok {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02", value, time.Local)
	if err != nil {
		return nil, fail(http.StatusBadRequest, key+" invalid")
	}
	return &t, nil
}

func (r *request) text(key string) (string, error) {
	var value *string
	if raw, ok := r.body[key]; !ok || json.Unmarshal(raw, &value) != nil || value == nil {
		return "", fail(http.StatusBadRequest, key+" missing or invalid")
	}
	return *value, nil
}

func (r *request) id() (int64, error) {
	var id int64
	if raw, ok := r.body["id"]; !ok || json.Unmarshal(raw, &id) != nil || id < 1 {
		return 0, fail(http.StatusBadRequest, "id missing or invalid")
	}
	return id, nil
}

// A zero-width space or a BOM alone would make a text that looks empty on the page.
func flat(text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		words[i] = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, w)
	}
	return strings.Join(slices.DeleteFunc(words, func(w string) bool { return w == "" }), " ")
}

func list[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

var done = map[string]bool{"ok": true}

func refresh(s *server, _ *request) (any, error) {
	if err := index.Refresh(s.conn, s.p); err != nil {
		return nil, err
	}
	return done, nil
}

type sessionJSON struct {
	ID           string   `json:"id"`
	Title        *string  `json:"title"`
	StartedAt    string   `json:"started_at"`
	EndedAt      string   `json:"ended_at"`
	Messages     int      `json:"n_messages"`
	Subjects     []string `json:"subjects"`
	EpisodeTitle *string  `json:"episode_title"`
	Summary      *string  `json:"summary"`
	Decisions    int      `json:"decisions"`
	Pending      int      `json:"pending"`
}

type episodeJSON struct {
	ID       int64    `json:"id"`
	StartTS  string   `json:"start_ts"`
	EndTS    string   `json:"end_ts"`
	Title    *string  `json:"title"`
	Summary  *string  `json:"summary"`
	Subjects []string `json:"subjects"`
}

func (s *server) shown(sessionID string) ([]episodeJSON, error) {
	all, err := memory.VisibleEpisodes(s.conn, sessionID)
	if err != nil {
		return nil, err
	}
	out := []episodeJSON{}
	for _, e := range all {
		if e.Title != nil && *e.Title != "" {
			out = append(out, episodeJSON{e.ID, e.StartTS, e.EndTS, e.Title, e.Summary, list(e.Subjects)})
		}
	}
	return out, nil
}

func (s *server) decorate(rows []search.SessionRow) ([]sessionJSON, error) {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	found, err := s.conn.QueryContext(background, "SELECT e.session_id,"+
		" COALESCE(SUM(p.kind = 'decision' AND p.superseded_by_uid IS NULL), 0), COALESCE(SUM(p.kind = 'pending' AND p.closed_at IS NULL), 0)"+
		" FROM points p JOIN episodes e ON e.id = p.episode_id"+
		" WHERE e.session_id IN (SELECT value FROM json_each(?)) GROUP BY e.session_id", string(raw))
	if err != nil {
		return nil, err
	}
	defer found.Close()
	tallies := map[string][2]int{}
	for found.Next() {
		var id string
		var t [2]int
		if err := found.Scan(&id, &t[0], &t[1]); err != nil {
			return nil, err
		}
		tallies[id] = t
	}
	if err := found.Err(); err != nil {
		return nil, err
	}
	out := make([]sessionJSON, len(rows))
	for i, r := range rows {
		episodes, err := s.shown(r.ID)
		if err != nil {
			return nil, err
		}
		out[i] = sessionJSON{ID: r.ID, Title: r.Title, StartedAt: r.StartedAt, EndedAt: r.EndedAt, Messages: r.Messages,
			Subjects: list(r.Subjects), Decisions: tallies[r.ID][0], Pending: tallies[r.ID][1]}
		if len(episodes) > 0 {
			last := episodes[len(episodes)-1]
			out[i].EpisodeTitle, out[i].Summary = last.Title, last.Summary
		}
	}
	return out, nil
}

func sessions(s *server, r *request) (any, error) {
	limit, err := r.limit(50)
	if err != nil {
		return nil, err
	}
	before, _ := r.param("before")
	subject, _ := r.param("subject")
	rows, err := search.ListSessions(s.conn, search.ListFilter{Subject: subject, Before: before, Limit: limit})
	if err != nil {
		return nil, err
	}
	return s.decorate(rows)
}

type messageJSON struct {
	TS        string  `json:"ts"`
	Role      string  `json:"role"`
	GitBranch *string `json:"git_branch"`
	Text      string  `json:"text"`
}

func sessionView(s *server, r *request) (any, error) {
	row, messages, err := search.SessionView(s.conn, r.ref)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fail(http.StatusNotFound, "session not found")
	}
	decorated, err := s.decorate([]search.SessionRow{*row})
	if err != nil {
		return nil, err
	}
	episodes, err := s.shown(row.ID)
	if err != nil {
		return nil, err
	}
	shown := make([]messageJSON, len(messages))
	for i, m := range messages {
		shown[i] = messageJSON{m.TS, m.Role, m.GitBranch, m.Text}
	}
	return struct {
		sessionJSON
		Episodes []episodeJSON `json:"episodes"`
		Messages []messageJSON `json:"messages"`
	}{decorated[0], episodes, shown}, nil
}

type hitJSON struct {
	TS        string   `json:"ts"`
	SessionID string   `json:"session_id"`
	Role      string   `json:"role"`
	Segments  [][2]any `json:"segments"`
	Subjects  []string `json:"subjects"`
}

func searchMessages(s *server, r *request) (any, error) {
	limit, err := r.limit(20)
	if err != nil {
		return nil, err
	}
	since, err := r.day("since")
	if err != nil {
		return nil, err
	}
	until, err := r.day("until")
	if err != nil {
		return nil, err
	}
	text, _ := r.param("q")
	subject, _ := r.param("subject")
	hits, err := search.Search(s.conn, text, search.Filter{Subject: subject, Since: since, Until: until, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]hitJSON, len(hits))
	for i, h := range hits {
		segments := make([][2]any, len(h.Segments))
		for j, seg := range h.Segments {
			segments[j] = [2]any{seg.Text, seg.Match}
		}
		out[i] = hitJSON{h.TS, h.SessionID, h.Role, segments, list(h.Subjects)}
	}
	return out, nil
}

type pointJSON struct {
	ID          int64   `json:"id"`
	EpisodeID   int64   `json:"episode_id"`
	SessionID   string  `json:"session_id"`
	Kind        string  `json:"kind"`
	Text        string  `json:"text"`
	Importance  int     `json:"importance"`
	SourceTS    string  `json:"source_ts"`
	CreatedAt   string  `json:"created_at"`
	ClosedAt    *string `json:"closed_at"`
	Corrected   bool    `json:"corrected"`
	SubjectUID  *string `json:"subject_uid"`
	SubjectName *string `json:"subject_name"`
}

func points(s *server, r *request) (any, error) {
	filter := memory.PointFilter{Limit: allPoints}
	kind, given := r.param("kind")
	if given && !slices.Contains(kinds, kind) {
		return nil, fail(http.StatusBadRequest, "invalid kind")
	}
	open, given := r.param("open")
	if given && open != "1" {
		return nil, fail(http.StatusBadRequest, "open doit valoir 1")
	}
	filter.Kind, filter.OpenOnly = kind, given
	limit, err := r.limit(50)
	if err != nil {
		return nil, err
	}
	offset, err := r.bounded("offset", 0, 0, 999999)
	if err != nil {
		return nil, err
	}
	filter.Subject, _ = r.param("subject")
	filter.Text, _ = r.param("q")
	every, err := memory.VisiblePoints(s.conn, filter)
	if err != nil {
		return nil, err
	}
	page := every[min(offset, len(every)):min(offset+limit, len(every))]
	ids := make([]int64, len(page))
	for i, p := range page {
		ids[i] = p.EpisodeID
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(background, "SELECT id, session_id FROM episodes WHERE id IN (SELECT value FROM json_each(?))", string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessionOf := map[int64]string{}
	for rows.Next() {
		var id int64
		var session string
		if err := rows.Scan(&id, &session); err != nil {
			return nil, err
		}
		sessionOf[id] = session
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]pointJSON, len(page))
	for i, p := range page {
		out[i] = pointJSON{p.ID, p.EpisodeID, sessionOf[p.EpisodeID], p.Kind, p.Text, p.Importance, p.SourceTS,
			p.CreatedAt, p.ClosedAt, p.Corrected, p.SubjectUID, p.SubjectName}
	}
	return map[string]any{"points": out, "total": len(every)}, nil
}

func counts(s *server, _ *request) (any, error) {
	var n struct {
		Sessions int `json:"sessions"`
		Points   int `json:"points"`
		Subjects int `json:"subjects"`
	}
	err := s.conn.QueryRowContext(background, "SELECT (SELECT COUNT(DISTINCT session_id) FROM messages),"+
		" (SELECT COUNT(*) FROM points WHERE superseded_by_uid IS NULL), (SELECT COUNT(*) FROM subjects)").Scan(&n.Sessions, &n.Points, &n.Subjects)
	return n, err
}

func memoText(s *server, r *request) (any, error) {
	term, given := r.param("subject")
	if !given {
		text, err := memo.Preview(s.conn, s.cwd, time.Now())
		return map[string]string{"text": text}, err
	}
	found, err := subjects.FindByTerm(s.conn, term)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, &subjects.ErrUnknownSubject{Term: term}
	}
	if len(found) > 1 {
		return nil, &subjects.ErrAmbiguousSubject{Term: term, Candidates: found}
	}
	text, err := memo.Index(s.conn, found[0].UID, nil)
	return map[string]string{"text": text}, err
}

type linkJSON struct {
	Type       string `json:"type"`
	Target     string `json:"target"`
	TargetName string `json:"target_name"`
}

type subjectJSON struct {
	UID       string        `json:"uid"`
	Kind      string        `json:"kind"`
	Name      string        `json:"name"`
	Aliases   []string      `json:"aliases"`
	Repo      *string       `json:"repo"`
	ParentUID *string       `json:"parent_uid"`
	Synthesis string        `json:"synthesis"`
	Points    int           `json:"points"`
	Links     []linkJSON    `json:"links"`
	Children  []subjectJSON `json:"children"`
}

func tree(s *server, _ *request) (any, error) {
	nodes, err := subjects.Tree(s.conn)
	if err != nil {
		return nil, err
	}
	all, err := subjects.List(s.conn)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, n := range all {
		names[n.UID] = n.Name
	}
	rows, err := s.conn.QueryContext(background, "SELECT subject_uid, COUNT(*) FROM points"+
		" WHERE subject_uid IS NOT NULL AND superseded_by_uid IS NULL GROUP BY subject_uid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tally := map[string]int{}
	for rows.Next() {
		var uid string
		var n int
		if err := rows.Scan(&uid, &n); err != nil {
			return nil, err
		}
		tally[uid] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var build func([]subjects.Node) ([]subjectJSON, error)
	build = func(nodes []subjects.Node) ([]subjectJSON, error) {
		out := []subjectJSON{}
		for _, n := range nodes {
			synthesis, err := memory.Synthesis(s.conn, n.UID)
			if err != nil {
				return nil, err
			}
			children, err := build(n.Children)
			if err != nil {
				return nil, err
			}
			links := []linkJSON{}
			for _, l := range n.Links {
				links = append(links, linkJSON{l.Type, l.Target, names[l.Target]})
			}
			out = append(out, subjectJSON{n.UID, n.Kind, n.Name, list(n.Aliases), n.Repo, n.ParentUID,
				synthesis, tally[n.UID], links, children})
		}
		return out, nil
	}
	return build(nodes)
}

func correct(s *server, r *request) (any, error) {
	id, err := r.id()
	if err != nil {
		return nil, err
	}
	var text *string
	var importance *int
	if raw, given := r.body["text"]; given {
		if json.Unmarshal(raw, &text) != nil || text == nil || flat(*text) == "" || utf8.RuneCountInString(*text) > maxPointText {
			return nil, fail(http.StatusBadRequest, "text must be a string of 1 to 2000 characters")
		}
	}
	if raw, given := r.body["importance"]; given {
		if json.Unmarshal(raw, &importance) != nil || importance == nil || *importance < 1 || *importance > 5 {
			return nil, fail(http.StatusBadRequest, "importance must be an integer from 1 to 5")
		}
	}
	if text == nil && importance == nil {
		return nil, fail(http.StatusBadRequest, "nothing to correct")
	}
	if err := memory.CorrectPoint(s.conn, id, text, importance); err != nil {
		return nil, err
	}
	return done, nil
}

func closePoint(s *server, r *request) (any, error) {
	id, err := r.id()
	if err != nil {
		return nil, err
	}
	if err := memory.ClosePoint(s.conn, id); err != nil {
		return nil, err
	}
	return done, nil
}

func forgetPoint(s *server, r *request) (any, error) {
	id, err := r.id()
	if err != nil {
		return nil, err
	}
	if err := memory.ForgetPoint(s.conn, id); err != nil {
		return nil, err
	}
	return done, nil
}

// Same lock as paul forget: an index pass started before the forgetting would index the session again.
func forgetSession(s *server, r *request) (any, error) {
	ref, err := r.text("id")
	if err != nil {
		return nil, err
	}
	for deadline := time.Now().Add(lockWait); ; time.Sleep(50 * time.Millisecond) {
		release, acquired, err := lock.Writer(s.p.IndexLock(), false)
		if err != nil {
			return nil, err
		}
		if acquired {
			defer release()
			break
		}
		if time.Now().After(deadline) {
			return nil, fail(http.StatusServiceUnavailable, "memory is busy, try again in a moment")
		}
	}
	id, err := search.ResolveSession(s.conn, ref)
	if err != nil {
		return nil, err
	}
	if id == "" || id != ref {
		return nil, fail(http.StatusNotFound, "session not found")
	}
	if err := memory.ForgetSession(s.conn, id); err != nil {
		return nil, err
	}
	return done, nil
}

// The summarizer may set a parent between SetParent's cycle check and its write, or create
// a subject between rename's name check and its write.
func (s *server) atomically(steps func(db.Querier) error) error {
	conn, err := s.conn.Conn(background)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(background, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := steps(conn); err != nil {
		_, undo := conn.ExecContext(background, "ROLLBACK")
		return errors.Join(err, undo)
	}
	_, err = conn.ExecContext(background, "COMMIT")
	return err
}

func (r *request) texts(keys ...string) ([]string, error) {
	out := make([]string, len(keys))
	for i, k := range keys {
		v, err := r.text(k)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func rename(s *server, r *request) (any, error) {
	v, err := r.texts("uid", "name")
	if err != nil {
		return nil, err
	}
	name := flat(mask.Text(v[1]))
	if name == "" {
		return nil, fail(http.StatusBadRequest, "name missing or invalid")
	}
	if err := s.atomically(func(q db.Querier) error { return renameUnique(q, v[0], name) }); err != nil {
		return nil, err
	}
	return done, nil
}

// Two subjects with one name merge at the next summarize: the page asks for an explicit merge instead.
// Repos never merge by name (forks share theirs), so they stay out of the check, on both sides.
func renameUnique(q db.Querier, uid, name string) error {
	renamed, err := subjects.Get(q, uid)
	if err != nil {
		return err
	}
	all, err := subjects.List(q)
	if err != nil {
		return err
	}
	for _, other := range all {
		if renamed.Kind == "subject" && other.Kind == "subject" && other.UID != uid &&
			subjects.Normalize(other.Name) == subjects.Normalize(name) {
			return fail(http.StatusConflict, "another subject is already named \""+other.Name+"\": to combine them, use \"Merge\"")
		}
	}
	return subjects.Rename(q, uid, name)
}

func merge(s *server, r *request) (any, error) {
	v, err := r.texts("from", "into")
	if err != nil {
		return nil, err
	}
	if err := subjects.Merge(s.conn, v[0], v[1]); err != nil {
		return nil, err
	}
	return done, nil
}

func move(s *server, r *request) (any, error) {
	v, err := r.texts("uid", "parent")
	if err != nil {
		return nil, err
	}
	if err := s.atomically(func(q db.Querier) error { return subjects.SetParent(q, v[0], v[1], user) }); err != nil {
		return nil, err
	}
	return done, nil
}

func writeLink(s *server, r *request, write func(db.Querier, string, string, string, subjects.Origin) error) (any, error) {
	v, err := r.texts("source", "type", "target")
	if err != nil {
		return nil, err
	}
	if err := write(s.conn, v[0], v[1], v[2], user); err != nil {
		return nil, err
	}
	return done, nil
}

func link(s *server, r *request) (any, error)   { return writeLink(s, r, subjects.Link) }
func unlink(s *server, r *request) (any, error) { return writeLink(s, r, subjects.Unlink) }
