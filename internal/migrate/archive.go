package migrate

import (
	"database/sql"
	"path/filepath"
)

var roles = map[string]string{"jonathan": "user", "claude": "claude", "sous_agent": "subagent"}

// The old index kept the messages of transcripts Claude Code has deleted since (after 30 days
// by default): those of a file the new index will not read go to a journal. Format v1 has no
// tool role, so tool lines are only counted.
func archive(from *sql.Tx, kept map[string]bool, dir string) (archived, tools int, err error) {
	rows, err := from.Query(`SELECT m.key, m.session_id, m.file, m.ts, m.role, m.text, s.title, s.launch_cwd
		FROM messages m LEFT JOIN sessions s ON s.id = m.session_id ORDER BY m.id`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	j := &journal{path: filepath.Join(dir, "archive.jsonl")}
	defer j.close()
	seen := map[string]bool{}
	for rows.Next() {
		var key, session, file, ts, role, text string
		var title, cwd sql.NullString
		if err := rows.Scan(&key, &session, &file, &ts, &role, &text, &title, &cwd); err != nil {
			return 0, 0, err
		}
		if kept[file] {
			continue
		}
		if role == "outil" {
			tools++
			continue
		}
		l := journalLine{V: 1, ID: key, Session: session, TS: ts, Role: roles[role], Text: text}
		if !seen[session] {
			seen[session] = true
			l.Title, l.Cwd = title.String, cwd.String
		}
		if err := j.write(l); err != nil {
			return 0, 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	return j.n, tools, j.close()
}
