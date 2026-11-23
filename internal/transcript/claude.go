// Package transcript turns Claude Code transcript lines and external journal lines into indexable items.
package transcript

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/mask"
)

const (
	RoleUser     = "user"
	RoleClaude   = "claude"
	RoleSubagent = "subagent"
	RoleTool     = "tool"
)

type Entry struct {
	Key, SessionID, TS, Role string
	Cwd, GitBranch           *string
	Text                     string
	Paths                    []string
}

type Title struct{ SessionID, Title string }

type AgentUse struct{ ToolUseID string }

// Item is an Entry, a Title or an AgentUse.
type Item interface{}

var agentTools = []string{"Agent", "Task"}

var notUserPrefixes = []string{
	"<teammate-message", "Another Claude session sent a message:", "<bash-stdout>", "<bash-stderr>",
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
}

var (
	bashInput          = regexp.MustCompile(`(?s)\A<bash-input>(.*?)</bash-input>\z`)
	commandName        = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs        = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	notificationResult = regexp.MustCompile(`(?s)<result>(.*?)</result>`)
	handBackReport     = regexp.MustCompile(`(?s)The report follows:(.*?)</agent-message>`)
	shellDir           = regexp.MustCompile(`\A\s*(?:git\s+-C|cd)\s+(?:"([^"]+)"|'([^']+)'|([^\s;&|]+))`)
	pastedContent      = regexp2.MustCompile(`<pasted_content id="([^"]*)">\n?(.*?)\n?</pasted_content id="\1">`, regexp2.Singleline)
)

var interruptions = []string{"[Request interrupted by user]", "[Request interrupted by user for tool use]"}

// Transcripts carry many line types this reader ignores, so an unknown shape is skipped, not an error.
func ParseClaudeLine(raw []byte, isAgentUse func(string) bool) ([]Item, error) {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, nil
	}
	switch m["type"] {
	case "ai-title":
		return title(m), nil
	case "user":
		return user(m, isAgentUse), nil
	case "assistant":
		return assistant(m), nil
	case "attachment":
		return queued(m), nil
	}
	return nil, nil
}

func str(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func title(m map[string]any) []Item {
	session, _ := str(m, "sessionId")
	text, _ := str(m, "aiTitle")
	text = strings.TrimSpace(text)
	if session == "" || text == "" {
		return nil
	}
	return []Item{Title{session, mask.Text(text)}}
}

type line struct {
	uuid, session, ts string
	cwd, gitBranch    *string
}

func newLine(m map[string]any) (line, bool) {
	l := line{}
	l.uuid, _ = str(m, "uuid")
	l.session, _ = str(m, "sessionId")
	ts, _ := str(m, "timestamp")
	var ok bool
	l.ts, ok = normalizeTS(ts)
	if cwd, isStr := str(m, "cwd"); isStr {
		l.cwd = &cwd
	}
	if branch, isStr := str(m, "gitBranch"); isStr {
		l.gitBranch = &branch
	}
	return l, ok && l.uuid != "" && l.session != ""
}

func (l line) entry(role, text, key string) Entry {
	return Entry{Key: key, SessionID: l.session, TS: l.ts, Role: role, Cwd: l.cwd, GitBranch: l.gitBranch, Text: mask.Text(text)}
}

func (l line) report(text string) Entry {
	text = mask.Text(text)
	sum := sha1.Sum([]byte(text))
	return l.entry(RoleSubagent, text, fmt.Sprintf("report:%s:%s", l.session, hex.EncodeToString(sum[:])))
}

func normalizeTS(value string) (string, bool) {
	moment, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", false
	}
	return moment.UTC().Format(db.UTCFormat), true
}

func content(m map[string]any) any {
	message, _ := m["message"].(map[string]any)
	return message["content"]
}

func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			block, _ := b.(map[string]any)
			if block["type"] != "text" {
				continue
			}
			if text, ok := str(block, "text"); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func origin(container map[string]any) any {
	o, _ := container["origin"].(map[string]any)
	return o["kind"]
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func notificationReport(text string) string {
	found := notificationResult.FindStringSubmatch(text)
	if found == nil {
		return ""
	}
	report := strings.TrimSpace(html.UnescapeString(found[1]))
	if report == "" || hasAnyPrefix(report, "This agent's report was delivered", "This agent has not reported yet") ||
		strings.Contains(report, "report was delivered to you as a message") {
		return ""
	}
	return report
}

func handBackText(text string) string {
	if !strings.Contains(text, "[Subagent hand-back]") {
		return ""
	}
	found := handBackReport.FindStringSubmatch(text)
	if found == nil {
		return ""
	}
	lines := withoutLeadingBlanks(strings.Split(strings.ReplaceAll(found[1], "\r\n", "\n"), "\n"))
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "[harness:") {
		lines = withoutLeadingBlanks(lines[1:])
	}
	return strings.TrimSpace(dedent(lines))
}

func withoutLeadingBlanks(lines []string) []string {
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			return lines[i:]
		}
	}
	return nil
}

// Hand-back reports arrive indented; dedenting keeps their key equal to the notification path's.
func dedent(lines []string) string {
	margin, set := "", false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		indent := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if !set {
			margin, set = indent, true
			continue
		}
		n := 0
		for n < len(margin) && n < len(indent) && margin[n] == indent[n] {
			n++
		}
		margin = margin[:n]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			out[i] = strings.TrimPrefix(l, margin)
		}
	}
	return strings.Join(out, "\n")
}

func humanText(text string) string {
	if found := bashInput.FindStringSubmatch(strings.TrimSpace(text)); found != nil {
		return "! " + strings.TrimSpace(found[1])
	}
	if hasAnyPrefix(strings.TrimLeft(text, " \t\r\n"), "<command-name>", "<command-message>") {
		var parts []string
		for _, re := range []*regexp.Regexp{commandName, commandArgs} {
			if found := re.FindStringSubmatch(text); found != nil {
				if p := strings.TrimSpace(found[1]); p != "" {
					parts = append(parts, p)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	text = strings.TrimSpace(text)
	for _, i := range interruptions {
		if text == i {
			return ""
		}
	}
	unwrapped, _ := pastedContent.Replace(text, "$2", -1, -1)
	return unwrapped
}

func user(m map[string]any, isAgentUse func(string) bool) []Item {
	l, ok := newLine(m)
	if !ok {
		return nil
	}
	c := content(m)
	if _, has := m["toolUseResult"]; has {
		return toolResults(l, c, isAgentUse)
	}
	if truthy(m["isCompactSummary"]) {
		return nil
	}
	text := textOf(c)
	if truthy(m["isMeta"]) {
		return reportItems(l, handBackText(text))
	}
	return userText(l, text, origin(m))
}

func reportItems(l line, report string) []Item {
	if report == "" {
		return nil
	}
	return []Item{l.report(report)}
}

func userText(l line, text string, origin any) []Item {
	if origin == "task-notification" || (origin == nil && strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<task-notification")) {
		return reportItems(l, notificationReport(text))
	}
	if origin == nil && hasAnyPrefix(strings.TrimLeft(text, " \t\r\n"), notUserPrefixes...) {
		return nil
	}
	if origin != nil && origin != "human" {
		return nil
	}
	return spoken(l, text)
}

func spoken(l line, text string) []Item {
	said := humanText(text)
	if said == "" {
		return nil
	}
	return []Item{l.entry(RoleUser, said, l.uuid+":0")}
}

func toolResults(l line, c any, isAgentUse func(string) bool) []Item {
	blocks, _ := c.([]any)
	var found []Item
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_result" || truthy(block["is_error"]) {
			continue
		}
		id, ok := str(block, "tool_use_id")
		if !ok || !isAgentUse(id) {
			continue
		}
		report := strings.TrimSpace(textOf(block["content"]))
		if report != "" && !hasAnyPrefix(report, "Async agent launched", "Spawned successfully.") {
			found = append(found, l.report(report))
		}
	}
	return found
}

func queued(m map[string]any) []Item {
	attachment, ok := m["attachment"].(map[string]any)
	if !ok || attachment["type"] != "queued_command" {
		return nil
	}
	l, ok := newLine(m)
	if !ok {
		return nil
	}
	text := textOf(attachment["prompt"])
	if strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<task-notification") {
		return reportItems(l, notificationReport(text))
	}
	if attachment["commandMode"] != "prompt" || origin(attachment) != "human" {
		return nil
	}
	return spoken(l, text)
}

func assistant(m map[string]any) []Item {
	l, ok := newLine(m)
	blocks, isList := content(m).([]any)
	if !ok || truthy(m["isApiErrorMessage"]) || !isList {
		return nil
	}
	var found []Item
	for i, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s:%d", l.uuid, i)
		switch block["type"] {
		case "text":
			if text, _ := str(block, "text"); strings.TrimSpace(text) != "" {
				found = append(found, l.entry(RoleClaude, strings.TrimSpace(text), key))
			}
		case "tool_use":
			found = append(found, toolUse(l, block, key)...)
		}
	}
	return found
}

func toolUse(l line, block map[string]any, key string) []Item {
	name, _ := str(block, "name")
	if name == "" {
		return nil
	}
	detail := ""
	if input, ok := block["input"].(map[string]any); ok {
		field := "file_path"
		if name == "Bash" {
			field = "description"
		}
		value, _ := str(input, field)
		detail = strings.TrimSpace(value)
	}
	text := name
	if detail != "" {
		text = name + " " + detail
	}
	entry := l.entry(RoleTool, text, key)
	entry.Paths = toolPaths(name, block["input"])
	found := []Item{entry}
	if id, _ := str(block, "id"); id != "" {
		for _, t := range agentTools {
			if name == t {
				found = append(found, AgentUse{id})
			}
		}
	}
	return found
}

func toolPaths(name string, input any) []string {
	in, _ := input.(map[string]any)
	var found []string
	for _, field := range []string{"file_path", "path", "notebook_path"} {
		if p, _ := str(in, field); usable(p) {
			found = append(found, p)
		}
	}
	if command, _ := str(in, "command"); name == "Bash" {
		if m := shellDir.FindStringSubmatch(command); m != nil {
			if p := m[1] + m[2] + m[3]; usable(p) {
				found = append(found, p)
			}
		}
	}
	return found
}

func usable(p string) bool { return filepath.IsAbs(p) && !strings.ContainsRune(p, 0) }
