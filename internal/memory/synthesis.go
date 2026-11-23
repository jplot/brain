package memory

import "github.com/jplot/brain/internal/db"

// Every reader of syntheses goes through these: a stale one may still quote a forgotten point.
func Synthesis(q db.Querier, subjectUID string) (string, error) {
	var text string
	rows, err := q.QueryContext(background, "SELECT text FROM syntheses WHERE subject_uid = ? AND stale = 0", subjectUID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if rows.Next() {
		err = rows.Scan(&text)
	}
	if err != nil {
		return "", err
	}
	return text, rows.Err()
}

type ChildSynthesis struct{ UID, Name, Text string }

func ChildSyntheses(q db.Querier, parentUID string, limit int) ([]ChildSynthesis, error) {
	rows, err := q.QueryContext(background, "SELECT s.uid, s.name, y.text FROM subjects s JOIN syntheses y ON y.subject_uid = s.uid"+
		" WHERE s.parent_uid = ? AND y.stale = 0 ORDER BY s.name, s.uid LIMIT ?", parentUID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChildSynthesis
	for rows.Next() {
		var c ChildSynthesis
		if err := rows.Scan(&c.UID, &c.Name, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
