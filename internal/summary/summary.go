// Package summary turns the indexed sessions into episodes and points through claude -p.
package summary

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/mask"
	"github.com/jplot/brain/internal/paths"
)

const (
	ChunkChars    = 60000
	Quiet         = 30 * time.Minute
	Retry         = time.Hour
	MaxFailures   = 3
	MaxChunks     = 100
	MaxOpenPoints = 30
)

const (
	Summarized = "summarized"
	Empty      = "empty"
	TooLong    = "too_long"
	Failed     = "failed"
	Already    = "already"
	Busy       = "busy"
	Refreshed  = "refreshed"
)

const utc = "2006-01-02T15:04:05.000Z"

var ctx = context.Background()

// Replaced by tests: none of them may reach the real claude.
var runner Runner = RunClaude

// Replaced by tests: 50 ms makes a held write lock fail fast.
var busyTimeout = 5 * time.Second

type Outcome struct {
	Status    string
	Points    int
	CostUSD   float64
	SessionID string
}

type Options struct {
	SessionID, Since string
	Limit            int
	All, Force       bool
}

func stamp(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC().Format(utc)
}

func newUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func PendingSessions(q db.Querier, now time.Time, o Options) ([]string, error) {
	query := "SELECT m.session_id, max(m.ts) AS last FROM messages m" +
		" LEFT JOIN (SELECT session_id, max(end_ts) AS done FROM episodes GROUP BY session_id) e" +
		"   ON e.session_id = m.session_id" +
		" LEFT JOIN summary_failures f ON f.session_id = m.session_id" +
		" WHERE m.ts > COALESCE(e.done, '')" +
		" AND m.session_id NOT IN (SELECT session_id FROM forgotten_sessions)"
	var args []any
	if o.SessionID != "" {
		query += " AND m.session_id = ?"
		args = append(args, o.SessionID)
	}
	if !o.Force {
		query += " AND (f.attempts IS NULL OR (f.attempts < ? AND f.last_at < ?))"
		args = append(args, MaxFailures, stamp(now.Add(-Retry)))
	}
	query += " GROUP BY m.session_id"
	var having []string
	if o.SessionID == "" {
		having = append(having, "last < ?")
		args = append(args, stamp(now.Add(-Quiet)))
	}
	if o.Since != "" {
		having = append(having, "min(m.ts) >= ?")
		args = append(args, o.Since)
	}
	if len(having) > 0 {
		query += " HAVING " + strings.Join(having, " AND ")
	}
	query += " ORDER BY last DESC"
	if o.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, o.Limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var id, last string
		if err := rows.Scan(&id, &last); err != nil {
			return nil, err
		}
		sessions = append(sessions, id)
	}
	return sessions, rows.Err()
}

type calls struct {
	run  Runner
	cost float64
}

func (c *calls) call(prompt, system string, schema any) (map[string]any, error) {
	result, err := c.run(prompt, system, schema)
	cost := result.CostUSD
	var e *Error
	if errors.As(err, &e) {
		cost = e.Cost
	}
	if cost == nil {
		log.Print("coût d'un appel à claude : inconnu")
	} else {
		log.Printf("coût d'un appel à claude : $%.4f", *cost)
		c.cost += *cost
	}
	return result.Data, err
}

func (c *calls) summary(prompt, system string) (map[string]any, error) {
	output, err := c.call(prompt, system, outputSchema)
	if err != nil {
		return nil, err
	}
	return output, checkShape(output)
}

func truncate(s string, n int) string {
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}

// ChunkChars counts code points: counted in bytes, accented text would be cut into more chunks, so more calls.
func chunks(lines []string) []string {
	var out, current []string
	size := 0
	for _, line := range lines {
		line = truncate(line, ChunkChars)
		n := utf8.RuneCountInString(line)
		if len(current) > 0 && size+n+1 > ChunkChars {
			out = append(out, strings.Join(current, "\n"))
			current, size = nil, 0
		}
		current = append(current, line)
		size += n + 1
	}
	return append(out, strings.Join(current, "\n"))
}

func nonBlank(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

func checkShape(output map[string]any) error {
	_, points := output["points"].([]any)
	closes, present := output["closes"]
	_, list := closes.([]any)
	if !points || !nonBlank(output["title"]) || !nonBlank(output["summary"]) || present && !list {
		return &Error{msg: "sortie du modèle invalide"}
	}
	return nil
}

// JSON numbers decode as float64; a fraction or a bool is not an integer.
func integer(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

type newPoint struct {
	kind, text, sourceTS string
	importance           int64
}

func cleanPoint(raw any, timestamps map[string]bool) (newPoint, bool) {
	m, _ := raw.(map[string]any)
	kind, _ := m["kind"].(string)
	text, _ := m["text"].(string)
	importance, isInt := integer(m["importance"])
	ts, isString := m["source_ts"].(string)
	if !slices.Contains(kinds, kind) || strings.TrimSpace(text) == "" || !isInt || importance < 1 || importance > 5 || !isString {
		return newPoint{}, false
	}
	// Haiku returned "[ts]" in 2 answers out of 14 of a real session; 2 sessions out of 20 ended with no point.
	ts = strings.Trim(strings.TrimSpace(ts), "[]")
	if !timestamps[ts] {
		// Replaying 3 sessions with no point, 1 answer out of 12 gave the time without the date: 10 points out of 10 rejected.
		var same []string
		for owned := range timestamps {
			if strings.HasSuffix(owned, "T"+ts) {
				same = append(same, owned)
			}
		}
		if len(same) != 1 {
			return newPoint{}, false
		}
		ts = same[0]
	}
	return newPoint{kind: kind, text: text, sourceTS: ts, importance: importance}, true
}

func encode(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// Each round merges at least two outputs into one, so n chunks cost at most n-1 merge calls.
func merge(c *calls, system string, open []openPoint, outputs []map[string]any) (map[string]any, error) {
	for len(outputs) > 1 {
		var batches [][]map[string]any
		var current []map[string]any
		size := 0
		for _, output := range outputs {
			text, err := encode(output)
			if err != nil {
				return nil, err
			}
			n := utf8.RuneCountInString(text)
			if len(current) > 0 && size+n > ChunkChars {
				batches = append(batches, current)
				current, size = nil, 0
			}
			current = append(current, output)
			size += n
		}
		batches = append(batches, current)
		if len(batches) == len(outputs) {
			return nil, &Error{msg: "fusion impossible : résumés partiels trop volumineux"}
		}
		outputs = nil
		for _, batch := range batches {
			if len(batch) == 1 {
				outputs = append(outputs, batch[0])
				continue
			}
			body, err := encode(batch)
			if err != nil {
				return nil, err
			}
			merged, err := c.summary(prompt(open, "Résumés partiels à fusionner, en JSON :\n"+body), system)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, merged)
		}
	}
	return outputs[0], nil
}

func immediate(conn *sql.DB, steps func(*sql.Conn) error) error {
	c, err := conn.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := steps(c); err != nil {
		_, rollback := c.ExecContext(ctx, "ROLLBACK")
		return errors.Join(err, rollback)
	}
	_, err = c.ExecContext(ctx, "COMMIT")
	return err
}

func forgotten(q db.Querier, sessionID string) (bool, error) {
	var found bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM forgotten_sessions WHERE session_id = ?)", sessionID).Scan(&found)
	return found, err
}

// The attempt is counted before claude runs: a process killed during the call still spends one of the MaxFailures.
func countAttempt(conn *sql.DB, sessionID string, now time.Time) (counted bool, err error) {
	err = immediate(conn, func(c *sql.Conn) error {
		gone, err := forgotten(c, sessionID)
		if err != nil || gone {
			return err
		}
		_, err = c.ExecContext(ctx, "INSERT INTO summary_failures(session_id, attempts, last_at) VALUES (?, 1, ?)"+
			" ON CONFLICT(session_id) DO UPDATE SET attempts = attempts + 1, last_at = excluded.last_at",
			sessionID, stamp(now))
		counted = err == nil
		return err
	})
	return counted, err
}

func fail(conn *sql.DB, sessionID string, now time.Time, c *calls, err error) Outcome {
	log.Printf("résumé échoué : %s (%T)", sessionID, err)
	if _, err := conn.Exec("UPDATE summary_failures SET last_at = ? WHERE session_id = ?", stamp(now), sessionID); err != nil {
		log.Printf("date d'échec non mise à jour : %s (%T)", sessionID, err)
	}
	return Outcome{Status: Failed, CostUSD: c.cost, SessionID: sessionID}
}

// A zero now stamps each write with the time it happens.
func SummarizeSession(conn *sql.DB, user, sessionID string, run Runner, now time.Time) Outcome {
	c := &calls{run: run}
	out, err := summarize(conn, user, sessionID, c, now)
	if err != nil {
		return fail(conn, sessionID, now, c, err)
	}
	out.SessionID = sessionID
	return out
}

type message struct{ ts, role, text string }

func newMessages(conn *sql.DB, sessionID string) ([]message, error) {
	var done sql.NullString
	if err := conn.QueryRow("SELECT max(end_ts) FROM episodes WHERE session_id = ?", sessionID).Scan(&done); err != nil {
		return nil, err
	}
	rows, err := conn.Query("SELECT ts, role, text FROM messages WHERE session_id = ? AND ts > ?"+
		" ORDER BY ts, id", sessionID, done.String)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []message
	for rows.Next() {
		var m message
		if err := rows.Scan(&m.ts, &m.role, &m.text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func openPoints(conn *sql.DB) ([]openPoint, error) {
	rows, err := conn.Query("SELECT id, text FROM points WHERE kind = 'pending' AND closed_at IS NULL"+
		" ORDER BY importance DESC, id LIMIT ?", MaxOpenPoints)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openPoint
	for rows.Next() {
		var p openPoint
		if err := rows.Scan(&p.id, &p.text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type episode struct {
	start, end     string
	title, summary *string
	points         []newPoint
	closes         []int64
}

func summarize(conn *sql.DB, user, sessionID string, c *calls, now time.Time) (Outcome, error) {
	if gone, err := forgotten(conn, sessionID); err != nil || gone {
		return Outcome{Status: Already}, err
	}
	rows, err := newMessages(conn, sessionID)
	if err != nil || len(rows) == 0 {
		return Outcome{Status: Already}, err
	}
	e := episode{start: rows[0].ts, end: rows[len(rows)-1].ts}
	var lines []string
	timestamps := map[string]bool{}
	for _, m := range rows {
		if m.role == "tool" {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s : %s", m.ts, m.role, m.text))
		timestamps[m.ts] = true
	}
	status := Empty
	if slices.ContainsFunc(rows, func(m message) bool { return m.role == "user" }) {
		parts := chunks(lines)
		if len(parts) > MaxChunks {
			log.Printf("épisode trop long : %s (%d morceaux)", sessionID, len(parts))
			status = TooLong
		} else {
			open, err := openPoints(conn)
			if err != nil {
				return Outcome{}, err
			}
			counted, err := countAttempt(conn, sessionID, now)
			if err != nil {
				log.Printf("résumé différé : %s (%T)", sessionID, err)
				return Outcome{Status: Busy}, nil
			}
			if !counted {
				return Outcome{Status: Already}, nil
			}
			if err := summarizeParts(c, user, sessionID, parts, open, timestamps, &e); err != nil {
				return Outcome{}, err
			}
			status = Summarized
		}
	}
	written, err := store(conn, sessionID, e, stamp(now))
	if err != nil {
		return Outcome{}, err
	}
	if !written {
		return Outcome{Status: Already, CostUSD: c.cost}, nil
	}
	return Outcome{Status: status, Points: len(e.points), CostUSD: c.cost}, nil
}

func summarizeParts(c *calls, user, sessionID string, parts []string, open []openPoint, timestamps map[string]bool, e *episode) error {
	sys := system(user)
	var outputs []map[string]any
	for _, part := range parts {
		output, err := c.summary(prompt(open, "Conversation :\n"+part), sys)
		if err != nil {
			return err
		}
		outputs = append(outputs, output)
	}
	output, err := merge(c, sys, open, outputs)
	if err != nil {
		return err
	}
	title, summary := mask.Text(output["title"].(string)), mask.Text(output["summary"].(string))
	e.title, e.summary = &title, &summary
	raw := output["points"].([]any)
	for _, r := range raw {
		if p, ok := cleanPoint(r, timestamps); ok {
			e.points = append(e.points, p)
		}
	}
	if len(e.points) < len(raw) {
		log.Printf("points rejetés : %s (%d sur %d)", sessionID, len(raw)-len(e.points), len(raw))
	}
	closes, _ := output["closes"].([]any)
	for _, v := range closes {
		id, ok := integer(v)
		if ok && slices.ContainsFunc(open, func(p openPoint) bool { return p.id == id }) {
			e.closes = append(e.closes, id)
		}
	}
	return nil
}

func store(conn *sql.DB, sessionID string, e episode, created string) (written bool, err error) {
	err = immediate(conn, func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "DELETE FROM summary_failures WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		var exists bool
		if err := c.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM episodes WHERE session_id = ? AND start_ts = ?)",
			sessionID, e.start).Scan(&exists); err != nil {
			return err
		}
		gone, err := forgotten(c, sessionID)
		if err != nil || exists || gone {
			return err
		}
		res, err := c.ExecContext(ctx, "INSERT INTO episodes(uid, session_id, start_ts, end_ts, title, summary,"+
			" created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", newUID(), sessionID, e.start, e.end, e.title,
			e.summary, created, created)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, p := range e.points {
			if _, err := c.ExecContext(ctx, "INSERT INTO points(uid, episode_id, kind, text, importance, source_ts,"+
				" created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", newUID(), id, p.kind,
				mask.Text(p.text), p.importance, p.sourceTS, created, created); err != nil {
				return err
			}
		}
		for _, closed := range e.closes {
			if _, err := c.ExecContext(ctx, "UPDATE points SET closed_at = ?, updated_at = ? WHERE id = ?"+
				" AND kind = 'pending' AND closed_at IS NULL AND corrected = 0", created, created, closed); err != nil {
				return err
			}
		}
		written = true
		return nil
	})
	return written, err
}

type staleEpisode struct {
	id      int64
	title   sql.NullString
	summary string
}

func remaining(q db.Querier, episode int64) (ids []int64, texts []string, err error) {
	rows, err := q.QueryContext(ctx, "SELECT id, text FROM points WHERE episode_id = ? ORDER BY id", episode)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, nil, err
		}
		ids, texts = append(ids, id), append(texts, text)
	}
	return ids, texts, rows.Err()
}

func staleEpisodes(conn *sql.DB) ([]staleEpisode, error) {
	rows, err := conn.Query("SELECT id, title, summary FROM episodes WHERE summary_stale = 1 AND summary IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []staleEpisode
	for rows.Next() {
		var e staleEpisode
		if err := rows.Scan(&e.id, &e.title, &e.summary); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RefreshStale clears a stale summary before the call: a failure leaves none rather than one quoting a forgotten point.
func RefreshStale(conn *sql.DB, run Runner) (int, float64) {
	c := &calls{run: run}
	stale, err := staleEpisodes(conn)
	if err != nil {
		log.Printf("résumés périmés illisibles (%T)", err)
		return 0, 0
	}
	refreshed := 0
	for _, e := range stale {
		done, err := refresh(conn, c, e)
		if err != nil {
			log.Printf("résumé périmé non refait : %T", err)
		}
		if done {
			refreshed++
		}
	}
	return refreshed, c.cost
}

func refresh(conn *sql.DB, c *calls, e staleEpisode) (done bool, err error) {
	if _, err := conn.Exec("UPDATE episodes SET title = NULL, summary = NULL, summary_stale = 0, updated_at = ?"+
		" WHERE id = ?", stamp(time.Time{}), e.id); err != nil {
		return false, err
	}
	ids, texts, err := remaining(conn, e.id)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	body := fmt.Sprintf("Titre actuel : %s\nRésumé actuel : %s\nPoints restants :\n- %s", e.title.String, e.summary,
		strings.Join(texts, "\n- "))
	output, err := c.call(wrap(body), refreshSystem, refreshSchema)
	if err != nil {
		return false, err
	}
	if !nonBlank(output["title"]) || !nonBlank(output["summary"]) {
		return false, &Error{msg: "sortie du modèle invalide"}
	}
	err = immediate(conn, func(tx *sql.Conn) error {
		current, _, err := remaining(tx, e.id)
		if err != nil || !slices.Equal(current, ids) {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE episodes SET title = ?, summary = ?, summary_stale = 0, updated_at = ?"+
			" WHERE id = ?", mask.Text(output["title"].(string)), mask.Text(output["summary"].(string)),
			stamp(time.Time{}), e.id)
		done = err == nil
		return err
	})
	return done, err
}

func since(conn *sql.DB, o Options, now time.Time) (string, error) {
	if o.All || o.Since != "" {
		return o.Since, nil
	}
	if _, err := conn.Exec("INSERT OR IGNORE INTO meta(key, value) VALUES ('summaries_since', ?)", stamp(now)); err != nil {
		return "", err
	}
	var first string
	err := conn.QueryRow("SELECT value FROM meta WHERE key = 'summaries_since'").Scan(&first)
	return first, err
}

// RunCatchup returns false when another summarizer holds the lock. Only the sessions started after
// the first run are summarized, unless o.All; o.Since replaces that first run.
func RunCatchup(p paths.Paths, o Options) ([]Outcome, bool) {
	if err := p.EnsureHome(); err != nil {
		log.Printf("résumés impossibles (%T)", err)
		return nil, false
	}
	release, acquired, err := lock.Writer(p.SummaryLock(), false)
	if err != nil {
		log.Printf("résumés impossibles (%T)", err)
	}
	if !acquired {
		return nil, false
	}
	defer release()
	outcomes, err := catchup(p, o)
	if err != nil {
		log.Printf("résumés impossibles (%T)", err)
	}
	return outcomes, true
}

func catchup(p paths.Paths, o Options) ([]Outcome, error) {
	conn, err := db.Open(p, busyTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	now := time.Now()
	if o.Since, err = since(conn, o, now); err != nil {
		return nil, err
	}
	sessions, err := PendingSessions(conn, now, o)
	if err != nil {
		return nil, err
	}
	user := config.UserName()
	var outcomes []Outcome
	// Oldest first: newest first left 48 pending points open for 3 closed over 20 real sessions.
	for _, session := range slices.Backward(sessions) {
		outcomes = append(outcomes, SummarizeSession(conn, user, session, runner, time.Time{}))
		if outcomes[len(outcomes)-1].Status == Busy {
			break
		}
	}
	if refreshed, cost := RefreshStale(conn, runner); refreshed > 0 || cost > 0 {
		outcomes = append(outcomes, Outcome{Status: Refreshed, Points: refreshed, CostUSD: cost})
	}
	return outcomes, nil
}
