// Package memory reads and edits the points and episodes the summarizer wrote.
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/mask"
	"github.com/jplot/brain/internal/search"
)

const (
	utc       = "2006-01-02T15:04:05.000Z"
	day       = "2006-01-02"
	pointCols = "p.id, p.episode_id, p.uid, p.kind, p.text, p.importance, p.source_ts, p.created_at," +
		" p.closed_at, p.corrected, p.updated_at"
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
}

type Episode struct {
	ID                             int64
	UID, SessionID, StartTS, EndTS string
	Title, Summary                 *string
	CreatedAt                      string
	SummaryStale                   bool
	UpdatedAt                      string
}

// Since and Until are calendar days (YYYY-MM-DD) in the local zone, both included.
type PointFilter struct {
	Kind, Text         string
	OpenOnly           bool
	PointID, EpisodeID int64
	Since, Until       string
	Limit              int
}

func now() string { return time.Now().UTC().Format(utc) }

func dayStart(s string, plus int) (string, error) {
	t, err := time.ParseInLocation(day, s, time.Local)
	if err != nil {
		return "", fmt.Errorf("date invalide %q : %w", s, err)
	}
	y, m, d := t.Date()
	return time.Date(y, m, d+plus, 0, 0, 0, 0, time.Local).UTC().Format(utc), nil
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
	rows, err := q.QueryContext(background, "SELECT "+pointCols+" FROM points p"+
		where+" ORDER BY p.importance DESC, p.created_at DESC, p.id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		var closed sql.NullString
		if err := rows.Scan(&p.ID, &p.EpisodeID, &p.UID, &p.Kind, &p.Text, &p.Importance, &p.SourceTS,
			&p.CreatedAt, &closed, &p.Corrected, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if closed.Valid {
			p.ClosedAt = &closed.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func VisibleEpisodes(q db.Querier, sessionID string) ([]Episode, error) {
	rows, err := q.QueryContext(background, "SELECT id, uid, session_id, start_ts, end_ts, title, summary,"+
		" created_at, summary_stale, updated_at FROM episodes WHERE session_id = ? ORDER BY start_ts", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		var title, summary sql.NullString
		if err := rows.Scan(&e.ID, &e.UID, &e.SessionID, &e.StartTS, &e.EndTS, &title, &summary,
			&e.CreatedAt, &e.SummaryStale, &e.UpdatedAt); err != nil {
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
	var busy, log, done int
	err := conn.QueryRowContext(background, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done)
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
			{"INSERT OR REPLACE INTO forgotten_points(uid, at) VALUES (?, ?)", []any{uid, at}},
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

func ForgetSession(q db.Querier, sessionID string) error {
	d, ok := q.(*sql.DB)
	if !ok {
		return ErrInTransaction
	}
	return write(d, func(c *sql.Conn) error {
		if _, err := c.ExecContext(background, "INSERT OR IGNORE INTO forgotten_sessions(session_id, at) VALUES (?, ?)",
			sessionID, now()); err != nil {
			return err
		}
		for _, table := range [][2]string{{"messages", "session_id"}, {"sessions", "id"},
			{"episodes", "session_id"}, {"summary_failures", "session_id"}} {
			if _, err := c.ExecContext(background, "DELETE FROM "+table[0]+" WHERE "+table[1]+" = ?", sessionID); err != nil {
				return err
			}
		}
		for _, fts := range []string{"messages_fts", "points_fts"} {
			if _, err := c.ExecContext(background, "INSERT INTO "+fts+"("+fts+") VALUES ('optimize')"); err != nil {
				return err
			}
		}
		return nil
	})
}
