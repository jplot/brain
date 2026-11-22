package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jplot/brain/internal/mask"
)

func ParseJournalLine(raw []byte, file string) ([]Item, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m["v"] != float64(1) {
		return nil, errors.New("journal line: v must be 1")
	}
	id, session, ts, role, text, err := requiredFields(m)
	if err != nil {
		return nil, err
	}
	when, ok := normalizeTS(ts)
	if !ok {
		return nil, fmt.Errorf("journal line: invalid ts %q", ts)
	}
	switch role {
	case RoleUser, RoleClaude, RoleSubagent:
	default:
		return nil, fmt.Errorf("journal line: unknown role %q", role)
	}
	entry := Entry{Key: fmt.Sprintf("journal:%s:%s", file, id), SessionID: session, TS: when, Role: role, Text: mask.Text(text)}
	if cwd, ok, err := optional(m, "cwd"); err != nil {
		return nil, err
	} else if ok {
		entry.Cwd = &cwd
	}
	items := []Item{entry}
	if heading, ok, err := optional(m, "title"); err != nil {
		return nil, err
	} else if ok {
		items = append(items, Title{session, mask.Text(heading)})
	}
	return items, nil
}

func requiredFields(m map[string]any) (id, session, ts, role, text string, err error) {
	fields := []struct {
		key string
		dst *string
	}{{"id", &id}, {"session", &session}, {"ts", &ts}, {"role", &role}, {"text", &text}}
	for _, f := range fields {
		value, ok := m[f.key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return "", "", "", "", "", fmt.Errorf("journal line: %s is required", f.key)
		}
		*f.dst = value
	}
	text = strings.TrimSpace(text)
	return
}

func optional(m map[string]any, key string) (string, bool, error) {
	v, present := m[key]
	if !present || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("journal line: %s must be a string", key)
	}
	s = strings.TrimSpace(s)
	return s, s != "", nil
}
