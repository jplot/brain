// Package memory reads and edits the points and episodes the summarizer wrote.
package memory

import (
	"bytes"
	stdcmp "cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/mask"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/search"
	"github.com/jplot/brain/internal/subjects"
)

const (
	day       = "2006-01-02"
	pointCols = "p.id, p.episode_id, p.uid, p.kind, p.text, p.importance, p.source_ts, p.created_at," +
		" p.closed_at, p.corrected, p.updated_at, p.subject_uid, s.name, u.last_used_at"
)

var (
	background = context.Background()

	ErrNothingToCorrect = errors.New("rien à corriger")
	ErrInTransaction    = errors.New("ForgetSession exige une connexion sans transaction ouverte")
)

type ErrPointNotFound struct{ ID int64 }

func (e *ErrPointNotFound) Error() string { return fmt.Sprintf("point introuvable : %d", e.ID) }

type Point struct {
	ID, EpisodeID       int64
	UID, Kind, Text     string
	Importance          int
	SourceTS, CreatedAt string
	ClosedAt            *string
	Corrected           bool
	UpdatedAt           string
	SubjectUID          *string
	SubjectName         *string
	LastUsedAt          *string
}

type Episode struct {
	ID                             int64
	UID, SessionID, StartTS, EndTS string
	Title, Summary                 *string
	CreatedAt                      string
	SummaryStale                   bool
	UpdatedAt                      string
	Subjects                       []string
}

// Since and Until are calendar days (YYYY-MM-DD) in the local zone, both included.
// Subject is a name, keyword or uid and covers its descendants; replaced points show only with History.
// OwnOnly takes Subject as a uid and leaves its descendants out.
// Order "score" ranks by Score with the terms of Text; the default is importance then recency.
type PointFilter struct {
	Kind, Subject, Text string
	Order               string
	OpenOnly, History   bool
	OwnOnly             bool
	PointID, EpisodeID  int64
	Since, Until        string
	Limit               int
}

func now() string { return db.Stamp(time.Time{}) }

func dayStart(s string, plus int) (string, error) {
	t, err := time.ParseInLocation(day, s, time.Local)
	if err != nil {
		return "", fmt.Errorf("date invalide %q : %w", s, err)
	}
	y, m, d := t.Date()
	return time.Date(y, m, d+plus, 0, 0, 0, 0, time.Local).UTC().Format(db.UTCFormat), nil
}

func VisiblePoints(q db.Querier, f PointFilter) ([]Point, error) {
	var clauses []string
	var args []any
	add := func(clause string, arg any) {
		clauses = append(clauses, clause)
		args = append(args, arg)
	}
	if f.Kind != "" {
		add("p.kind = ?", f.Kind)
	}
	if f.PointID != 0 {
		add("p.id = ?", f.PointID)
	}
	if f.EpisodeID != 0 {
		add("p.episode_id = ?", f.EpisodeID)
	}
	if f.Since != "" {
		since, err := dayStart(f.Since, 0)
		if err != nil {
			return nil, err
		}
		add("p.source_ts >= ?", since)
	}
	if f.Until != "" {
		until, err := dayStart(f.Until, 1)
		if err != nil {
			return nil, err
		}
		add("p.source_ts < ?", until)
	}
	if f.OpenOnly {
		clauses = append(clauses, "p.kind = 'pending' AND p.closed_at IS NULL")
	}
	if !f.History {
		clauses = append(clauses, "p.superseded_by_uid IS NULL")
	}
	if f.Subject != "" && f.OwnOnly {
		add("p.subject_uid = ?", f.Subject)
	} else if f.Subject != "" {
		tree, err := subjects.TreeByTerm(q, f.Subject)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(append([]string{}, tree...))
		if err != nil {
			return nil, err
		}
		add("p.subject_uid IN (SELECT value FROM json_each(?))", string(raw))
	}
	if strings.TrimSpace(f.Text) != "" {
		match, err := search.ToFTSQuery(f.Text)
		if err != nil {
			return nil, err
		}
		add("p.id IN (SELECT rowid FROM points_fts WHERE points_fts MATCH ?)", match)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	order, cut := " ORDER BY p.importance DESC, p.created_at DESC, p.id DESC LIMIT ?", []any{limit}
	if f.Order == "score" {
		order, cut = "", nil
	}
	points, err := queryPoints(q, "SELECT "+pointCols+" FROM points p"+
		" LEFT JOIN subjects s ON s.uid = p.subject_uid LEFT JOIN point_usage u ON u.uid = p.uid"+
		where+order, append(args, cut...))
	if err != nil || f.Order != "score" {
		return points, err
	}
	ids := make([]int64, len(points))
	for i, p := range points {
		ids[i] = p.ID
	}
	relevance, err := Relevance(q, search.Terms(f.Text), ids)
	if err != nil {
		return nil, err
	}
	scored := Score(points, relevance, time.Now())
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]Point, len(scored))
	for i, s := range scored {
		out[i] = s.Point
	}
	return out, nil
}

func queryPoints(q db.Querier, query string, args []any) ([]Point, error) {
	rows, err := q.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		var closed, subjectUID, subjectName, lastUsed sql.NullString
		if err := rows.Scan(&p.ID, &p.EpisodeID, &p.UID, &p.Kind, &p.Text, &p.Importance, &p.SourceTS,
			&p.CreatedAt, &closed, &p.Corrected, &p.UpdatedAt, &subjectUID, &subjectName, &lastUsed); err != nil {
			return nil, err
		}
		for _, v := range []struct {
			from sql.NullString
			to   **string
		}{{closed, &p.ClosedAt}, {subjectUID, &p.SubjectUID}, {subjectName, &p.SubjectName}, {lastUsed, &p.LastUsedAt}} {
			if v.from.Valid {
				*v.to = &v.from.String
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// A summary waiting to be redone may still quote a forgotten point.
func VisibleEpisodes(q db.Querier, sessionID string) ([]Episode, error) {
	rows, err := q.QueryContext(background, "SELECT id, uid, session_id, start_ts, end_ts,"+
		" CASE WHEN summary_stale = 0 THEN title END, CASE WHEN summary_stale = 0 THEN summary END,"+
		" created_at, summary_stale, updated_at,"+
		" (SELECT json_group_array(name) FROM (SELECT s.name FROM episode_subjects es JOIN subjects s ON s.uid = es.subject_uid"+
		" WHERE es.episode_uid = episodes.uid ORDER BY s.name, s.uid))"+
		" FROM episodes WHERE session_id = ? ORDER BY start_ts", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		var title, summary sql.NullString
		var names string
		if err := rows.Scan(&e.ID, &e.UID, &e.SessionID, &e.StartTS, &e.EndTS, &title, &summary,
			&e.CreatedAt, &e.SummaryStale, &e.UpdatedAt, &names); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(names), &e.Subjects); err != nil {
			return nil, err
		}
		if title.Valid {
			e.Title = &title.String
		}
		if summary.Valid {
			e.Summary = &summary.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type Scored struct {
	Point
	Score float64
}

func Score(points []Point, relevance map[int64]float64, now time.Time) []Scored {
	out := make([]Scored, len(points))
	for i, p := range points {
		hours := math.Inf(1)
		at := p.CreatedAt
		if p.LastUsedAt != nil {
			at = *p.LastUsedAt
		}
		if t, err := time.Parse(db.UTCFormat, at); err == nil {
			hours = max(now.Sub(t).Hours(), 0)
		}
		out[i] = Scored{p, math.Pow(0.995, hours) + float64(p.Importance-1)/4 + relevance[p.ID]}
	}
	slices.SortFunc(out, func(a, b Scored) int {
		if a.Score != b.Score {
			return stdcmp.Compare(b.Score, a.Score)
		}
		return stdcmp.Compare(b.ID, a.ID)
	})
	return out
}

// Relevance is the BM25 of points_fts for any of the terms, min-max normalized over ids.
// FTS5 ranks the best match lowest (negative), so the raw value is its opposite; a
// candidate without a match counts 0. All candidates tied above 0 get 1, tied at 0 get 0.
func Relevance(q db.Querier, terms []string, ids []int64) (map[int64]float64, error) {
	out := make(map[int64]float64, len(ids))
	for _, id := range ids {
		out[id] = 0
	}
	if len(terms) == 0 || len(ids) == 0 {
		return out, nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(background, "SELECT rowid, -bm25(points_fts) FROM points_fts"+
		" WHERE points_fts MATCH ? AND rowid IN (SELECT value FROM json_each(?))", search.MatchAny(terms), string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		out[id] = score
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range out {
		lo, hi = min(lo, v), max(hi, v)
	}
	for id, v := range out {
		switch {
		case hi > lo:
			out[id] = (v - lo) / (hi - lo)
		case hi > 0:
			out[id] = 1
		default:
			out[id] = 0
		}
	}
	return out, nil
}

func MarkUsed(q db.Querier, uids []string, at time.Time) error {
	for _, uid := range uids {
		if _, err := q.ExecContext(background, "INSERT INTO point_usage(uid, last_used_at) VALUES (?, ?)"+
			" ON CONFLICT(uid) DO UPDATE SET last_used_at = excluded.last_used_at", uid, db.Stamp(at)); err != nil {
			return err
		}
	}
	return nil
}

func optimizePoints(q db.Querier) error {
	_, err := q.ExecContext(background, "INSERT INTO points_fts(points_fts) VALUES ('optimize')")
	return err
}

func CorrectPoint(q db.Querier, id int64, text *string, importance *int) error {
	if text == nil && importance == nil {
		return ErrNothingToCorrect
	}
	if importance != nil && (*importance < 1 || *importance > 5) {
		return fmt.Errorf("importance hors de 1 à 5 : %d", *importance)
	}
	var masked, imp any
	if text != nil {
		masked = mask.Text(*text)
	}
	if importance != nil {
		imp = *importance
	}
	res, err := q.ExecContext(background, "UPDATE points SET text = COALESCE(?, text), importance = COALESCE(?, importance),"+
		" corrected = 1, updated_at = ? WHERE id = ?", masked, imp, now(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return notFound(id, err)
	}
	if text != nil {
		return optimizePoints(q)
	}
	return nil
}

func notFound(id int64, err error) error {
	if err != nil {
		return err
	}
	return &ErrPointNotFound{id}
}

func ClosePoint(q db.Querier, id int64) error {
	at := now()
	res, err := q.ExecContext(background, "UPDATE points SET closed_at = ?, updated_at = ? WHERE id = ?", at, at, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return notFound(id, err)
	}
	return nil
}

// flush runs on the connection that wrote: a reader holding the WAL blocked the
// checkpoint for the whole busy_timeout (5 s measured), so it runs with none.
func flush(conn *sql.Conn) error {
	var timeout int
	if err := conn.QueryRowContext(background, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		return err
	}
	if _, err := conn.ExecContext(background, "PRAGMA busy_timeout=0"); err != nil {
		return err
	}
	var busy, pages, moved int
	err := conn.QueryRowContext(background, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &moved)
	if err == nil && busy == 1 {
		log.Printf("point de contrôle occupé : %d pages sur %d restent dans le WAL", pages-moved, pages)
	}
	_, restore := conn.ExecContext(background, fmt.Sprintf("PRAGMA busy_timeout=%d", timeout))
	return errors.Join(err, restore)
}

func write(d *sql.DB, steps func(*sql.Conn) error) (err error) {
	conn, err := d.Conn(background)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(background, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := steps(conn); err != nil {
		_, rollback := conn.ExecContext(background, "ROLLBACK")
		return errors.Join(err, rollback)
	}
	if _, err := conn.ExecContext(background, "COMMIT"); err != nil {
		return err
	}
	return flush(conn)
}

// A synthesis may say what a point said without citing it, and a parent's is written from its children's.
const staleTree = "UPDATE syntheses SET stale = 1, updated_at = ? WHERE subject_uid IN (WITH RECURSIVE up(uid) AS" +
	" (SELECT subject_uid FROM points WHERE %s UNION SELECT s.parent_uid FROM subjects s JOIN up ON s.uid = up.uid" +
	" WHERE s.parent_uid IS NOT NULL) SELECT uid FROM up)"

func ForgetPoint(d *sql.DB, id int64) error {
	return write(d, func(c *sql.Conn) error {
		var uid string
		var episode int64
		err := c.QueryRowContext(background, "SELECT uid, episode_id FROM points WHERE id = ?", id).Scan(&uid, &episode)
		if errors.Is(err, sql.ErrNoRows) {
			return &ErrPointNotFound{id}
		}
		if err != nil {
			return err
		}
		at := now()
		for _, step := range []struct {
			query string
			args  []any
		}{
			{"UPDATE episodes SET summary_stale = 1, updated_at = ? WHERE id = ?", []any{at, episode}},
			{"UPDATE syntheses SET stale = 1, updated_at = ? WHERE EXISTS (SELECT 1 FROM json_each(cited) WHERE value = ?)", []any{at, uid}},
			{fmt.Sprintf(staleTree, "id = ?"), []any{at, id}},
			{"UPDATE points SET superseded_by_uid = NULL, superseded_at = NULL, updated_at = ? WHERE superseded_by_uid = ?", []any{at, uid}},
			{"INSERT OR REPLACE INTO forgotten_points(uid, at) VALUES (?, ?)", []any{uid, at}},
			{"DELETE FROM point_usage WHERE uid = ?", []any{uid}},
			{"DELETE FROM points WHERE id = ?", []any{id}},
			{"INSERT INTO points_fts(points_fts) VALUES ('optimize')", nil},
		} {
			if _, err := c.ExecContext(background, step.query, step.args...); err != nil {
				return err
			}
		}
		return nil
	})
}

func ForgetSession(q db.Querier, p paths.Paths, sessionID string) error {
	d, ok := q.(*sql.DB)
	if !ok {
		return ErrInTransaction
	}
	return write(d, func(c *sql.Conn) error {
		if _, err := c.ExecContext(background, "UPDATE syntheses SET stale = 1, updated_at = ? WHERE EXISTS"+
			" (SELECT 1 FROM json_each(cited) j WHERE j.value IN (SELECT p.uid FROM points p"+
			" JOIN episodes e ON e.id = p.episode_id WHERE e.session_id = ?))", now(), sessionID); err != nil {
			return err
		}
		if _, err := c.ExecContext(background, fmt.Sprintf(staleTree, "episode_id IN (SELECT id FROM episodes WHERE session_id = ?)"),
			now(), sessionID); err != nil {
			return err
		}
		if _, err := c.ExecContext(background, "INSERT OR IGNORE INTO forgotten_sessions(session_id, at) VALUES (?, ?)",
			sessionID, now()); err != nil {
			return err
		}
		if _, err := c.ExecContext(background, "DELETE FROM point_usage WHERE uid IN (SELECT p.uid FROM points p"+
			" JOIN episodes e ON e.id = p.episode_id WHERE e.session_id = ?)", sessionID); err != nil {
			return err
		}
		if _, err := c.ExecContext(background, "UPDATE points SET superseded_by_uid = NULL, superseded_at = NULL, updated_at = ?"+
			" WHERE superseded_by_uid IN (SELECT p.uid FROM points p JOIN episodes e ON e.id = p.episode_id WHERE e.session_id = ?)",
			now(), sessionID); err != nil {
			return err
		}
		touched, err := dropGraph(c, sessionID)
		if err != nil {
			return err
		}
		for _, table := range [][2]string{{"messages", "session_id"}, {"sessions", "id"},
			{"episodes", "session_id"}, {"summary_failures", "session_id"},
			{"pending_paths", "session_id"}, {"session_repos", "session_id"}, {"injected", "session_id"}} {
			if _, err := c.ExecContext(background, "DELETE FROM "+table[0]+" WHERE "+table[1]+" = ?", sessionID); err != nil {
				return err
			}
		}
		if err := dropOrphans(c, touched); err != nil {
			return err
		}
		for _, fts := range []string{"messages_fts", "points_fts"} {
			if _, err := c.ExecContext(background, "INSERT INTO "+fts+"("+fts+") VALUES ('optimize')"); err != nil {
				return err
			}
		}
		rewritten, err := withoutSession(p.Archive(), sessionID)
		if err != nil || !rewritten {
			return err
		}
		// Re-read from the start: the old offset no longer points at a line boundary.
		_, err = c.ExecContext(background, "DELETE FROM files WHERE path = ?", p.Archive())
		return err
	})
}

const sessionEpisodes = "(SELECT uid FROM episodes WHERE session_id = ?1)"

func dropGraph(c *sql.Conn, sessionID string) (touchedJSON string, err error) {
	if err := c.QueryRowContext(background, "SELECT json_group_array(uid) FROM ("+
		"SELECT p.subject_uid AS uid FROM points p JOIN episodes e ON e.id = p.episode_id WHERE e.session_id = ?1"+
		" AND p.subject_uid IS NOT NULL"+
		" UNION SELECT subject_uid FROM episode_subjects WHERE episode_uid IN "+sessionEpisodes+
		" UNION SELECT source_uid FROM links WHERE origin = 'model' AND episode_uid IN "+sessionEpisodes+
		" UNION SELECT target_uid FROM links WHERE origin = 'model' AND episode_uid IN "+sessionEpisodes+
		" UNION SELECT parent_uid FROM subjects WHERE parent_origin = 'model' AND parent_episode_uid IN "+sessionEpisodes+")",
		sessionID).Scan(&touchedJSON); err != nil {
		return "", err
	}
	for _, step := range []string{
		"DELETE FROM links WHERE origin = 'model' AND episode_uid IN " + sessionEpisodes,
		"UPDATE subjects SET parent_uid = NULL, parent_origin = NULL, parent_episode_uid = NULL, updated_at = ?2" +
			" WHERE parent_origin = 'model' AND parent_episode_uid IN " + sessionEpisodes,
		"DELETE FROM episode_subjects WHERE episode_uid IN " + sessionEpisodes,
	} {
		if _, err := c.ExecContext(background, step, sessionID, now()); err != nil {
			return "", err
		}
	}
	return touchedJSON, nil
}

// A subject the forgotten session alone created would keep its name and aliases matching prompts.
// A touched parent empties only once its touched child is gone, hence the passes.
func dropOrphans(c *sql.Conn, touchedJSON string) error {
	for {
		res, err := c.ExecContext(background, "DELETE FROM subjects WHERE kind = 'subject'"+
			" AND uid IN (SELECT value FROM json_each(?))"+
			" AND NOT EXISTS (SELECT 1 FROM points p WHERE p.subject_uid = subjects.uid)"+
			" AND NOT EXISTS (SELECT 1 FROM episode_subjects es WHERE es.subject_uid = subjects.uid)"+
			" AND NOT EXISTS (SELECT 1 FROM subjects c WHERE c.parent_uid = subjects.uid)"+
			" AND NOT EXISTS (SELECT 1 FROM links l WHERE subjects.uid IN (l.source_uid, l.target_uid))"+
			" AND NOT EXISTS (SELECT 1 FROM syntheses y WHERE y.subject_uid = subjects.uid AND y.stale = 0)", touchedJSON)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	for _, step := range []string{
		"DELETE FROM subject_terms WHERE subject_uid IN (SELECT value FROM json_each(?1)) AND subject_uid NOT IN (SELECT uid FROM subjects)",
		"DELETE FROM syntheses WHERE subject_uid IN (SELECT value FROM json_each(?1)) AND subject_uid NOT IN (SELECT uid FROM subjects)",
		"DELETE FROM summary_failures WHERE session_id IN (SELECT ?2 || value FROM json_each(?1)" +
			" WHERE value NOT IN (SELECT uid FROM subjects))",
	} {
		if _, err := c.ExecContext(background, step, touchedJSON, SynthesisFailureKey); err != nil {
			return err
		}
	}
	return nil
}

// Only the archive is rewritten: paul migrate wrote it, while the other journals belong to
// the programs that write them.
func withoutSession(path, sessionID string) (bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var kept bytes.Buffer
	removed := false
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		var l struct{ Session string }
		if json.Unmarshal(line, &l) == nil && l.Session == sessionID {
			removed = true
			continue
		}
		kept.Write(line)
	}
	if !removed {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".archive-")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(kept.Bytes())
	if err := errors.Join(err, tmp.Sync(), tmp.Close()); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	return true, errors.Join(dir.Sync(), dir.Close())
}
