// Package summary turns the indexed sessions into episodes and points through claude -p.
package summary

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jplot/brain/internal/config"
	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/mask"
	"github.com/jplot/brain/internal/memory"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

const (
	ChunkChars       = 60000
	Quiet            = 30 * time.Minute
	HintsWait        = 24 * time.Hour
	Retry            = time.Hour
	MaxFailures      = 3
	MaxChunks        = 100
	MaxCandidates    = 60
	MaxOpenPoints    = 30
	MaxCurrentPoints = 30
	MaxShownAliases  = 8
	MaxPointChars    = 300
	MaxDataChars     = 15000
	MaxNameChars     = 80
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

var ctx = context.Background()

// Replaced by tests: none of them may reach the real claude.
var runner = RunClaude(model, effort)

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
		args = append(args, MaxFailures, db.Stamp(now.Add(-Retry)))
	}
	query += " GROUP BY m.session_id"
	// A repository still unresolved would be missing from the candidates; after a day the session goes without it.
	having := []string{"(last < ? OR NOT EXISTS (SELECT 1 FROM pending_paths p WHERE p.session_id = m.session_id))"}
	args = append(args, db.Stamp(now.Add(-HintsWait)))
	if o.SessionID == "" {
		having = append(having, "last < ?")
		args = append(args, db.Stamp(now.Add(-Quiet)))
	}
	if o.Since != "" {
		having = append(having, "min(m.ts) >= ?")
		args = append(args, o.Since)
	}
	query += " HAVING " + strings.Join(having, " AND ")
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
	valid := points && nonBlank(output["title"]) && nonBlank(output["summary"])
	for _, key := range []string{"new_subjects", "links", "aliases", "closes", "supersedes"} {
		v, present := output[key]
		_, list := v.([]any)
		valid = valid && (!present || list)
	}
	if !valid {
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
	kind, text, sourceTS, subject string
	importance                    int64
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
	subject, _ := m["subject"].(string)
	return newPoint{kind: kind, text: text, sourceTS: ts, subject: subject, importance: importance}, true
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

var newRef = regexp.MustCompile(`^new:(\d+)$`)

func rewrite(v any, shift func(any) any, keys ...string) []any {
	items, _ := v.([]any)
	out := make([]any, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			out[i] = item
			continue
		}
		m = maps.Clone(m)
		for _, key := range keys {
			if ref, ok := m[key]; ok {
				m[key] = shift(ref)
			}
		}
		out[i] = m
	}
	return out
}

// Each partial numbers its new subjects from 0: the merge call gets them in one list, references shifted to match.
func combine(batch []map[string]any) map[string]any {
	news, partials := []any{}, []any{}
	for _, output := range batch {
		own, _ := output["new_subjects"].([]any)
		offset := len(news)
		shift := func(v any) any {
			s, _ := v.(string)
			m := newRef.FindStringSubmatch(s)
			if m == nil {
				return v
			}
			n, err := strconv.Atoi(m[1])
			if err != nil || n >= len(own) {
				return ""
			}
			return fmt.Sprintf("new:%d", n+offset)
		}
		partial := maps.Clone(output)
		delete(partial, "new_subjects")
		partial["points"] = rewrite(output["points"], shift, "subject")
		partial["links"] = rewrite(output["links"], shift, "source", "target")
		partial["aliases"] = rewrite(output["aliases"], shift, "subject")
		news = append(news, rewrite(own, shift, "parent")...)
		partials = append(partials, partial)
	}
	return map[string]any{"new_subjects": news, "partials": partials}
}

// Each round merges at least two outputs into one, so n chunks cost at most n-1 merge calls.
func merge(c *calls, system string, data promptData, outputs []map[string]any) (map[string]any, error) {
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
			body, err := encode(combine(batch))
			if err != nil {
				return nil, err
			}
			merged, err := c.summary(prompt(data, "Résumés partiels à fusionner, en JSON ; new:<n> y désigne new_subjects[n],"+
				" liste commune à tous ; le new de chaque supersedes est le rang dans les points de son propre partiel,"+
				" à partir de 0 :\n"+body), system)
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
			sessionID, db.Stamp(now))
		counted = err == nil
		return err
	})
	return counted, err
}

func fail(conn *sql.DB, sessionID string, now time.Time, c *calls, err error) Outcome {
	log.Printf("résumé échoué : %s (%T)", sessionID, err)
	if _, err := conn.Exec("UPDATE summary_failures SET last_at = ? WHERE session_id = ?", db.Stamp(now), sessionID); err != nil {
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

func pairs(q db.Querier, query string, args ...any) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Words and adjacent word pairs, built once: one strings.Contains per term took 34 s on 6 M characters and
// 3 000 subjects. Pairs alone found "réseau de la maison" in "réseau de chez moi… de la panne… la maison",
// so a name whose pairs all occur is confirmed by one Contains.
func vocabulary(text string) func(term string) bool {
	words := strings.Fields(subjects.Normalize(text))
	padded := " " + strings.Join(words, " ") + " "
	seen := map[string]bool{}
	for i, w := range words {
		seen[w] = true
		if i > 0 {
			seen[words[i-1]+" "+w] = true
		}
	}
	return func(term string) bool {
		key := strings.Fields(subjects.Normalize(term))
		if len(key) == 1 {
			return seen[key[0]]
		}
		for i := 1; i < len(key); i++ {
			if !seen[key[i-1]+" "+key[i]] {
				return false
			}
		}
		return len(key) > 1 && strings.Contains(padded, " "+strings.Join(key, " ")+" ")
	}
}

func candidates(q db.Querier, sessionID, text string) ([]candidate, error) {
	all, err := subjects.List(q)
	if err != nil {
		return nil, err
	}
	repos, err := pairs(q, "SELECT subject_uid, '' FROM session_repos WHERE session_id = ?", sessionID)
	if err != nil {
		return nil, err
	}
	active, err := pairs(q, "SELECT es.subject_uid, max(e.end_ts) FROM episode_subjects es"+
		" JOIN episodes e ON e.uid = es.episode_uid GROUP BY es.subject_uid")
	if err != nil {
		return nil, err
	}
	mentioned := vocabulary(text)
	rank := map[string]int{}
	names := map[string]string{}
	for _, s := range all {
		names[s.UID] = s.Name
		if _, ok := repos[s.UID]; ok {
			rank[s.UID] = -2
		} else if slices.ContainsFunc(append([]string{s.Name}, s.Aliases...), mentioned) {
			rank[s.UID] = -1
		}
	}
	slices.SortFunc(all, func(a, b subjects.Subject) int {
		return cmp.Or(cmp.Compare(rank[a.UID], rank[b.UID]), cmp.Compare(active[b.UID], active[a.UID]),
			cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.UID, b.UID))
	})
	out := []candidate{}
	for _, s := range all[:min(len(all), MaxCandidates)] {
		_, session := repos[s.UID]
		c := candidate{uid: s.UID, kind: s.Kind, name: s.Name, aliases: s.Aliases, session: session}
		if s.ParentUID != nil {
			c.parent = names[*s.ParentUID]
		}
		out = append(out, c)
	}
	return out, nil
}

func shownPoints(q db.Querier, limit int, now time.Time, where string, args ...any) ([]shownPoint, error) {
	rows, err := q.QueryContext(ctx, "SELECT p.id, p.kind, p.text, p.importance, p.created_at, p.subject_uid,"+
		" u.last_used_at FROM points p LEFT JOIN point_usage u ON u.uid = p.uid WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []memory.Point
	for rows.Next() {
		var p memory.Point
		if err := rows.Scan(&p.ID, &p.Kind, &p.Text, &p.Importance, &p.CreatedAt, &p.SubjectUID, &p.LastUsedAt); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	scored := memory.Score(points, nil, now)
	var out []shownPoint
	for _, s := range scored[:min(len(scored), limit)] {
		p := shownPoint{id: s.ID, kind: s.Kind, text: s.Text}
		if s.SubjectUID != nil {
			p.subject = *s.SubjectUID
		}
		out = append(out, p)
	}
	return out, nil
}

const (
	ofCandidates = "p.subject_uid IN (SELECT value FROM json_each(?))"
	openPending  = " AND p.kind = 'pending' AND p.closed_at IS NULL"
)

func gather(q db.Querier, sessionID, text string, now time.Time) (promptData, error) {
	var d promptData
	var err error
	if d.candidates, err = candidates(q, sessionID, text); err != nil {
		return d, err
	}
	uids := make([]string, len(d.candidates))
	for i, c := range d.candidates {
		uids[i] = c.uid
	}
	raw, err := json.Marshal(uids)
	if err != nil {
		return d, err
	}
	if d.open, err = shownPoints(q, MaxOpenPoints, now, ofCandidates+openPending, string(raw)); err != nil {
		return d, err
	}
	// A pending point with no subject would otherwise never be closed.
	loose, err := shownPoints(q, MaxOpenPoints-len(d.open), now, "p.subject_uid IS NULL"+openPending)
	if err != nil {
		return d, err
	}
	d.open = append(d.open, loose...)
	if d.current, err = shownPoints(q, MaxCurrentPoints, now,
		ofCandidates+" AND p.kind IN ('decision', 'fact') AND p.superseded_by_uid IS NULL", string(raw)); err != nil {
		return d, err
	}
	d.fit()
	return d, nil
}

type newSubject struct {
	name, parent string
	aliases      []string
}

type proposal struct{ source, typ, target string }

type aliasAdd struct {
	subject string
	add     []string
}

type replacement struct {
	old  int64
	rank int
}

type episode struct {
	start, end     string
	title, summary *string
	points         []newPoint
	closes         []int64
	newSubjects    []newSubject
	links          []proposal
	aliases        []aliasAdd
	supersedes     []replacement
	candidates     map[string]bool
	rejected       int
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
	var lines, texts []string
	timestamps := map[string]bool{}
	for _, m := range rows {
		if m.role == "tool" {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s : %s", m.ts, m.role, m.text))
		texts = append(texts, m.text)
		timestamps[m.ts] = true
	}
	status := Empty
	if slices.ContainsFunc(rows, func(m message) bool { return m.role == "user" }) {
		parts := chunks(lines)
		if len(parts) > MaxChunks {
			log.Printf("épisode trop long : %s (%d morceaux)", sessionID, len(parts))
			status = TooLong
		} else {
			data, err := gather(conn, sessionID, strings.Join(texts, "\n"), now)
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
			if err := summarizeParts(c, user, sessionID, parts, data, timestamps, &e); err != nil {
				return Outcome{}, err
			}
			status = Summarized
		}
	}
	written, err := store(conn, sessionID, e, db.Stamp(now))
	if err != nil {
		return Outcome{}, err
	}
	if !written {
		return Outcome{Status: Already, CostUSD: c.cost}, nil
	}
	return Outcome{Status: status, Points: len(e.points), CostUSD: c.cost}, nil
}

func strs(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, mask.Text(strings.TrimSpace(s)))
		}
	}
	return out
}

func field(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// A new subject keeps its index even when invalid: new:<n> references count on it.
func readGraph(output map[string]any, e *episode) {
	news, _ := output["new_subjects"].([]any)
	for _, item := range news {
		m, _ := item.(map[string]any)
		s := newSubject{parent: field(m, "parent"), aliases: strs(m["aliases"])}
		if name := mask.Text(strings.TrimSpace(field(m, "name"))); subjects.Normalize(name) != "" {
			s.name = name
		}
		e.newSubjects = append(e.newSubjects, s)
	}
	links, _ := output["links"].([]any)
	for _, item := range links {
		m, _ := item.(map[string]any)
		e.links = append(e.links, proposal{field(m, "source"), field(m, "type"), field(m, "target")})
	}
	aliases, _ := output["aliases"].([]any)
	for _, item := range aliases {
		m, _ := item.(map[string]any)
		e.aliases = append(e.aliases, aliasAdd{field(m, "subject"), strs(m["add"])})
	}
}

func summarizeParts(c *calls, user, sessionID string, parts []string, data promptData, timestamps map[string]bool, e *episode) error {
	sys := system(user)
	var outputs []map[string]any
	for _, part := range parts {
		output, err := c.summary(prompt(data, "Conversation :\n"+part), sys)
		if err != nil {
			return err
		}
		outputs = append(outputs, output)
	}
	output, err := merge(c, sys, data, outputs)
	if err != nil {
		return err
	}
	title, summary := mask.Text(output["title"].(string)), mask.Text(output["summary"].(string))
	e.title, e.summary = &title, &summary
	raw := output["points"].([]any)
	ranks := map[int64]int{}
	for i, r := range raw {
		if p, ok := cleanPoint(r, timestamps); ok {
			ranks[int64(i)] = len(e.points)
			e.points = append(e.points, p)
		}
	}
	if len(e.points) < len(raw) {
		log.Printf("points rejetés : %s (%d sur %d)", sessionID, len(raw)-len(e.points), len(raw))
	}
	closes, _ := output["closes"].([]any)
	for _, v := range closes {
		id, ok := integer(v)
		if ok && slices.ContainsFunc(data.open, func(p shownPoint) bool { return p.id == id }) {
			e.closes = append(e.closes, id)
		} else {
			e.rejected++
		}
	}
	e.candidates = map[string]bool{}
	for _, s := range data.candidates {
		e.candidates[s.uid] = true
	}
	readGraph(output, e)
	supersedes, _ := output["supersedes"].([]any)
	for _, item := range supersedes {
		m, _ := item.(map[string]any)
		old, oldOK := integer(m["old"])
		rank, rankOK := integer(m["new"])
		i, kept := ranks[rank]
		if oldOK && rankOK && kept && (e.points[i].kind == "decision" || e.points[i].kind == "fact") &&
			slices.ContainsFunc(data.current, func(p shownPoint) bool { return p.id == old }) {
			e.supersedes = append(e.supersedes, replacement{old, i})
		} else {
			e.rejected++
		}
	}
	return nil
}

type refs struct {
	q          db.Querier
	origin     subjects.Origin
	candidates map[string]bool
	created    []string
	ignored    int
}

func live(q db.Querier, uid string) (string, error) {
	for seen := map[string]bool{}; !seen[uid]; {
		seen[uid] = true
		var found bool
		if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM subjects WHERE uid = ?)", uid).Scan(&found); err != nil || found {
			return uid, err
		}
		err := q.QueryRowContext(ctx, "SELECT into_uid FROM merged WHERE from_uid = ?", uid).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

func (r *refs) resolve(ref string) (string, error) {
	if m := newRef.FindStringSubmatch(ref); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n >= len(r.created) {
			return "", nil
		}
		return r.created[n], nil
	}
	if !r.candidates[ref] {
		return "", nil
	}
	return live(r.q, ref)
}

var refused = []error{subjects.ErrCycle, subjects.ErrSecondParent, subjects.ErrUserSet, subjects.ErrLinkType, subjects.ErrSelfLink}

func (r *refs) propose(err error) error {
	if slices.ContainsFunc(refused, func(e error) bool { return errors.Is(err, e) }) {
		r.ignored++
		return nil
	}
	return err
}

func (r *refs) graph(e episode) error {
	var edges []proposal
	for i, s := range e.newSubjects {
		uid := ""
		if s.name != "" {
			var err error
			if uid, err = subjects.Create(r.q, s.name, s.aliases, nil); err != nil {
				return err
			}
			if err := subjects.AddAliases(r.q, uid, s.aliases); err != nil {
				return err
			}
		}
		r.created = append(r.created, uid)
		if s.parent != "" {
			edges = append(edges, proposal{fmt.Sprintf("new:%d", i), "part_of", s.parent})
		}
	}
	for _, l := range append(edges, e.links...) {
		source, err := r.resolve(l.source)
		if err != nil {
			return err
		}
		target, err := r.resolve(l.target)
		if err != nil {
			return err
		}
		switch {
		case source == "" || target == "":
			r.ignored++
		case l.typ == "part_of":
			err = r.propose(subjects.SetParent(r.q, source, target, r.origin))
		default:
			err = r.propose(subjects.Link(r.q, source, l.typ, target, r.origin))
		}
		if err != nil {
			return err
		}
	}
	for _, a := range e.aliases {
		uid, err := r.resolve(a.subject)
		if err != nil {
			return err
		}
		if uid == "" {
			r.ignored++
			continue
		}
		if err := subjects.AddAliases(r.q, uid, a.add); err != nil {
			return err
		}
	}
	return nil
}

func store(conn *sql.DB, sessionID string, e episode, created string) (written bool, err error) {
	ignored := 0
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
		uid := db.NewUID()
		res, err := c.ExecContext(ctx, "INSERT INTO episodes(uid, session_id, start_ts, end_ts, title, summary,"+
			" created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", uid, sessionID, e.start, e.end, e.title,
			e.summary, created, created)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r := &refs{q: c, origin: subjects.Origin{EpisodeUID: &uid}, candidates: e.candidates}
		if err := r.graph(e); err != nil {
			return err
		}
		about := map[string]bool{}
		pointUIDs, pointSubjects := make([]string, len(e.points)), make([]string, len(e.points))
		for i, p := range e.points {
			subject, err := r.resolve(p.subject)
			if err != nil {
				return err
			}
			var value any
			switch {
			case subject != "":
				value, about[subject] = subject, true
			case p.subject != "":
				r.ignored++
			}
			pointUIDs[i], pointSubjects[i] = db.NewUID(), subject
			if _, err := c.ExecContext(ctx, "INSERT INTO points(uid, episode_id, kind, text, importance, source_ts,"+
				" created_at, updated_at, subject_uid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", pointUIDs[i], id, p.kind,
				mask.Text(p.text), p.importance, p.sourceTS, created, created, value); err != nil {
				return err
			}
		}
		for _, closed := range e.closes {
			if _, err := c.ExecContext(ctx, "UPDATE points SET closed_at = ?, updated_at = ? WHERE id = ?"+
				" AND kind = 'pending' AND closed_at IS NULL AND corrected = 0", created, created, closed); err != nil {
				return err
			}
		}
		for _, s := range e.supersedes {
			res, err := c.ExecContext(ctx, "UPDATE points SET superseded_by_uid = ?, superseded_at = ?, updated_at = ?"+
				" WHERE id = ? AND subject_uid = ? AND kind IN ('decision', 'fact') AND superseded_by_uid IS NULL",
				pointUIDs[s.rank], created, created, s.old, pointSubjects[s.rank])
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				r.ignored++
			}
		}
		for subject := range about {
			if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO episode_subjects(episode_uid, subject_uid) VALUES (?, ?)",
				uid, subject); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO episode_subjects(episode_uid, subject_uid)"+
			" SELECT ?, subject_uid FROM session_repos WHERE session_id = ?", uid, sessionID); err != nil {
			return err
		}
		if _, err := subjects.MergeDuplicates(c); err != nil {
			return err
		}
		written, ignored = true, r.ignored+e.rejected
		return nil
	})
	if ignored > 0 {
		log.Printf("propositions ignorées : %s (%d)", sessionID, ignored)
	}
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
		" WHERE id = ?", db.Stamp(time.Time{}), e.id); err != nil {
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
			db.Stamp(time.Time{}), e.id)
		done = err == nil
		return err
	})
	return done, err
}

func since(conn *sql.DB, o Options, now time.Time) (string, error) {
	if o.All || o.Since != "" {
		return o.Since, nil
	}
	if _, err := conn.Exec("INSERT OR IGNORE INTO meta(key, value) VALUES ('summaries_since', ?)", db.Stamp(now)); err != nil {
		return "", err
	}
	var first string
	err := conn.QueryRow("SELECT value FROM meta WHERE key = 'summaries_since'").Scan(&first)
	return first, err
}

// RunCatchup returns false when another summarizer holds the lock. Only the sessions started after
// the first run are summarized, unless o.All; o.Since replaces that first run.
func RunCatchup(p paths.Paths, o Options) ([]Outcome, bool, error) {
	if err := p.EnsureHome(); err != nil {
		return nil, false, err
	}
	release, acquired, err := lock.Writer(p.SummaryLock(), false)
	if err != nil || !acquired {
		return nil, false, err
	}
	defer release()
	outcomes, err := catchup(p, o)
	return outcomes, true, err
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
