package transcript

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jplot/brain/internal/mask"
)

type obj = map[string]any

const ts = "2026-10-26T23:45:57Z"

func base(kind string, extra obj) obj {
	m := obj{"type": kind, "uuid": "u1", "sessionId": "s", "timestamp": ts, "cwd": "/p", "gitBranch": "main"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func userLine(content any, origin string, extra obj) obj {
	m := base("user", obj{"message": obj{"role": "user", "content": content}})
	if origin != "" {
		m["origin"] = obj{"kind": origin}
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func assistantLine(blocks []any, extra obj) obj {
	return base("assistant", obj{"message": obj{"role": "assistant", "content": blocks}, "isApiErrorMessage": extra["isApiErrorMessage"]})
}

func queuedLine(prompt any, mode, origin string) obj {
	attachment := obj{"type": "queued_command", "prompt": prompt, "commandMode": mode}
	if origin != "" {
		attachment["origin"] = obj{"kind": origin}
	}
	return base("attachment", obj{"attachment": attachment})
}

func text(s string) obj { return obj{"type": "text", "text": s} }

func toolUseBlock(name string, input any) obj {
	return obj{"type": "tool_use", "id": "t", "name": name, "input": input}
}

func parse(t *testing.T, o any, known func(string) bool) []Item {
	t.Helper()
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	items, err := ParseClaudeLine(raw, known)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func never(string) bool { return false }

func entries(t *testing.T, o any, known func(string) bool) []Entry {
	t.Helper()
	var out []Entry
	for _, it := range parse(t, o, known) {
		if e, ok := it.(Entry); ok {
			out = append(out, e)
		}
	}
	return out
}

func roleText(es []Entry) [][2]string {
	var out [][2]string
	for _, e := range es {
		out = append(out, [2]string{e.Role, e.Text})
	}
	return out
}

func TestRetained(t *testing.T) {
	notif := "<task-notification><status>completed</status><result>Found</result></task-notification>"
	cases := []struct {
		name       string
		obj        obj
		role, text string
	}{
		{"human origin", userLine("hello", "human", nil), "user", "hello"},
		{"no origin", userLine("hello", "", nil), "user", "hello"},
		{"text blocks", userLine([]any{text("look"), obj{"type": "image"}}, "human", nil), "user", "look"},
		{"slash command", userLine("<command-name>/voice</command-name><command-message>voice</command-message><command-args>on</command-args>", "", nil), "user", "/voice on"},
		{"bash input", userLine("<bash-input>ls -la</bash-input>", "", nil), "user", "! ls -la"},
		{"bash tag quoted", userLine("see the <bash-input> tag in the code", "", nil), "user", "see the <bash-input> tag in the code"},
		{"command-message quoted", userLine("why is <command-message> handled", "", nil), "user", "why is <command-message> handled"},
		{"bash input inside sentence", userLine("the format is <bash-input>ls</bash-input> and I want to discuss it", "", nil), "user", "the format is <bash-input>ls</bash-input> and I want to discuss it"},
		{"bash input in pasted", userLine("<pasted_content id=\"1\">\n<bash-input>ls</bash-input>\n</pasted_content id=\"1\"> there", "", nil), "user", "<bash-input>ls</bash-input> there"},
		{"bash input padded", userLine("  <bash-input>ls</bash-input>\n", "", nil), "user", "! ls"},
		{"queued human", queuedLine("on the side", "prompt", "human"), "user", "on the side"},
		{"queued with image", queuedLine([]any{text("with image"), obj{"type": "image"}}, "prompt", "human"), "user", "with image"},
		{"assistant text", assistantLine([]any{text("here")}, nil), "claude", "here"},
		{"bash tool", assistantLine([]any{toolUseBlock("Bash", obj{"command": "rm x", "description": "List files"})}, nil), "tool", "Bash List files"},
		{"edit tool", assistantLine([]any{toolUseBlock("Edit", obj{"file_path": "/a/b.py"})}, nil), "tool", "Edit /a/b.py"},
		{"notification origin", userLine(notif, "task-notification", nil), "subagent", "Found"},
		{"notification no origin", userLine("<task-notification><result>Found</result></task-notification>", "", nil), "subagent", "Found"},
		{"notification queued", queuedLine("<task-notification><result>Found</result></task-notification>", "task-notification", ""), "subagent", "Found"},
		{"hand-back", userLine("Another Claude session sent a message:\n<agent-message from=\"a\">\n[Subagent hand-back] x The report follows:\n  [harness: y]\n  \n  The report\n</agent-message>\nMore", "", obj{"isMeta": true}), "subagent", "The report"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := roleText(entries(t, c.obj, never))
			if want := [][2]string{{c.role, c.text}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestDiscarded(t *testing.T) {
	cases := map[string]any{
		"system reminder":      userLine("<system-reminder>x</system-reminder>", "", obj{"isMeta": true}),
		"meta agent message":   userLine("<agent-message from=\"a\">hi</agent-message>", "", obj{"isMeta": true}),
		"compact summary":      userLine("summary", "human", obj{"isCompactSummary": true}),
		"teammate message":     userLine("<teammate-message>x</teammate-message>", "", nil),
		"bash stdout":          userLine("<bash-stdout>ok</bash-stdout><bash-stderr></bash-stderr>", "", nil),
		"local stdout":         userLine("<local-command-stdout>ok</local-command-stdout>", "", nil),
		"local caveat":         userLine("<local-command-caveat>x</local-command-caveat>", "", nil),
		"notification no body": userLine("<task-notification><status>killed</status></task-notification>", "task-notification", nil),
		"notification delivered": userLine("<task-notification><result>This agent's report was delivered to you</result></task-notification>",
			"task-notification", nil),
		"foreign origin":        userLine("hello", "peer", nil),
		"queued foreign origin": queuedLine("hello", "prompt", "peer"),
		"api error":             assistantLine([]any{text("API Error")}, obj{"isApiErrorMessage": true}),
		"thinking":              assistantLine([]any{obj{"type": "thinking", "thinking": "hmm"}}, nil),
		"blob":                  obj{"type": "api-request-blob"},
		"hook attachment":       obj{"type": "attachment", "attachment": obj{"type": "hook_success"}},
		"empty":                 obj{},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parse(t, o, never); len(got) != 0 {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func resultLine(content any, id string, block obj) obj {
	b := obj{"type": "tool_result", "tool_use_id": id, "content": content}
	for k, v := range block {
		b[k] = v
	}
	return userLine([]any{b}, "", obj{"toolUseResult": obj{"x": 1}})
}

func isT1(id string) bool { return id == "t1" }

func always(string) bool { return true }

func TestAgentResultIsReport(t *testing.T) {
	if got := roleText(entries(t, resultLine("The report", "t1", nil), isT1)); !reflect.DeepEqual(got, [][2]string{{"subagent", "The report"}}) {
		t.Fatalf("got %q", got)
	}
	two := []any{text("One"), text("Two")}
	if got := entries(t, resultLine(two, "t1", nil), isT1); len(got) != 1 || got[0].Text != "One\nTwo" {
		t.Fatalf("got %v", got)
	}
	for name, o := range map[string]obj{
		"other id":  resultLine("The report", "other", nil),
		"launched":  resultLine("Async agent launched in background", "t1", nil),
		"is_error":  resultLine("The report", "t1", obj{"is_error": true}),
		"spawned":   resultLine("Spawned successfully.", "t1", nil),
		"spawned 2": resultLine("Spawned successfully. Agent id: x", "t1", nil),
	} {
		if got := entries(t, o, isT1); len(got) != 0 {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestUnknownToolResultIsNotUser(t *testing.T) {
	if got := entries(t, resultLine("command output", "t1", nil), never); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestAgentUseRemembered(t *testing.T) {
	for _, name := range []string{"Agent", "Task"} {
		items := parse(t, assistantLine([]any{obj{"type": "tool_use", "id": "t9", "name": name, "input": obj{}}}, nil), never)
		if len(items) != 2 || items[1] != (AgentUse{"t9"}) {
			t.Fatalf("%s: got %v", name, items)
		}
		if e := items[0].(Entry); e.Role != "tool" {
			t.Fatalf("role %q", e.Role)
		}
	}
	items := parse(t, assistantLine([]any{obj{"type": "tool_use", "id": "t9", "name": "Bash", "input": obj{}}}, nil), never)
	if len(items) != 1 {
		t.Fatalf("got %v", items)
	}
}

func TestToolWithoutDescriptionNorPath(t *testing.T) {
	for name, input := range map[string]obj{"Bash": {"command": "ls"}, "Grep": {"pattern": "x"}} {
		got := entries(t, assistantLine([]any{toolUseBlock(name, input)}, nil), never)
		if len(got) != 1 || got[0].Text != name {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestTitle(t *testing.T) {
	got := parse(t, obj{"type": "ai-title", "sessionId": "s", "aiTitle": "Plan the release"}, never)
	if !reflect.DeepEqual(got, []Item{Title{"s", "Plan the release"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestReportSameKeyOnEveryPath(t *testing.T) {
	notification := entries(t, userLine("<task-notification><result>The report</result></task-notification>", "task-notification", nil), never)
	handBack := entries(t, userLine("<agent-message from=\"a\">\n[Subagent hand-back] The report follows:\n  The report\n</agent-message>", "", obj{"isMeta": true}), never)
	result := entries(t, resultLine("The report", "t1", nil), always)
	if notification[0].Key != handBack[0].Key || handBack[0].Key != result[0].Key || !strings.HasPrefix(notification[0].Key, "report:s:") {
		t.Fatalf("keys differ: %q %q %q", notification[0].Key, handBack[0].Key, result[0].Key)
	}
}

func TestKeyAndFields(t *testing.T) {
	got := entries(t, userLine("hello", "human", nil), never)
	if len(got) != 1 {
		t.Fatal(got)
	}
	e := got[0]
	if e.Key != "u1:0" || e.SessionID != "s" || *e.Cwd != "/p" || *e.GitBranch != "main" {
		t.Fatalf("got %+v", e)
	}
}

func TestKeyPerBlock(t *testing.T) {
	got := entries(t, assistantLine([]any{text("a"), text("b")}, nil), never)
	if len(got) != 2 || got[0].Key != "u1:0" || got[1].Key != "u1:1" {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizedDate(t *testing.T) {
	cases := map[string]string{
		"2026-10-26T23:45:57Z":          "2026-10-26T23:45:57.000Z",
		"2026-10-26T23:45:57.123+02:00": "2026-10-26T21:45:57.123Z",
	}
	for in, want := range cases {
		got := entries(t, userLine("hello", "", obj{"timestamp": in}), never)
		if len(got) != 1 || got[0].TS != want {
			t.Errorf("%s: got %v", in, got)
		}
	}
	for _, bad := range []any{"yesterday", nil, "2026-10-26T23:45:57"} {
		if got := entries(t, userLine("hello", "", obj{"timestamp": bad}), never); len(got) != 0 {
			t.Errorf("%v: got %v", bad, got)
		}
	}
}

func TestUnexpectedShapeIsSkipped(t *testing.T) {
	cases := []string{
		`null`, `[]`, `"x"`, `3`,
		`{"type":"user","message":"x"}`,
		`{"type":"user","message":{"content":3}}`,
		`{"type":"user","uuid":"u","sessionId":"s","timestamp":"` + ts + `","message":{"content":["bare",null,3]}}`,
		`{"type":"user","uuid":"u","sessionId":"s","timestamp":"` + ts + `","message":{"content":{"type":"text"}}}`,
		`{"type":"user","uuid":"u","sessionId":3,"timestamp":"` + ts + `","message":{"content":"x"}}`,
		`{"type":"user","uuid":null,"sessionId":"s","timestamp":"` + ts + `","message":{"content":"x"}}`,
		`{"type":"assistant","uuid":"u","sessionId":"s","timestamp":"` + ts + `","message":{"content":[null,"x",{"type":"tool_use","id":3,"name":[]},{"type":"text","text":3}]}}`,
		`{"type":"ai-title","sessionId":"s","aiTitle":3}`,
		`{"type":"attachment","attachment":"x"}`,
		`{"type":"attachment","uuid":"u","sessionId":"s","timestamp":"` + ts + `","attachment":{"type":"queued_command","prompt":3,"commandMode":"prompt","origin":"human"}}`,
		`{"type":"user","uuid":"u","sessionId":"s","timestamp":"` + ts + `","origin":{"kind":"human"},"isMeta":"yes","toolUseResult":3,"message":{"content":"x"}}`,
	}
	for _, raw := range cases {
		items, err := ParseClaudeLine([]byte(raw), always)
		if err != nil || len(items) != 0 {
			t.Errorf("%s: got %v, %v", raw, items, err)
		}
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	if _, err := ParseClaudeLine([]byte(`{"type":`), never); err == nil {
		t.Fatal("want an error")
	}
}

func TestNonTextFieldsAreIgnored(t *testing.T) {
	if got := entries(t, userLine("x", "", obj{"cwd": 3}), never); got[0].Cwd != nil {
		t.Errorf("cwd %v", *got[0].Cwd)
	}
	if got := entries(t, assistantLine([]any{toolUseBlock("Bash", "x")}, nil), never); len(got) != 1 || got[0].Text != "Bash" {
		t.Errorf("got %v", got)
	}
}

func TestLoneSurrogateStaysEncodable(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u","sessionId":"s","timestamp":"` + ts + `","origin":{"kind":"human"},"message":{"content":"a\ud800"}}`,
		`{"type":"user","uuid":"u","sessionId":"s","timestamp":"` + ts + `","isMeta":true,"message":{"content":"<agent-message from=\"a\">\n[Subagent hand-back] The report follows:\n  a\ud800\n</agent-message>"}}`,
		`{"type":"assistant","uuid":"u","sessionId":"s","timestamp":"` + ts + `","message":{"content":[{"type":"text","text":"a\ud800"}]}}`,
	}
	for _, raw := range lines {
		items, err := ParseClaudeLine([]byte(raw), never)
		if err != nil || len(items) != 1 {
			t.Fatalf("%s: %v %v", raw, items, err)
		}
		if got := items[0].(Entry).Text; !utf8.ValidString(got) || !strings.HasPrefix(got, "a") {
			t.Errorf("got %q", got)
		}
	}
}

func TestParagraphReportSameKeyOnTwoPaths(t *testing.T) {
	handBack := entries(t, userLine("<agent-message from=\"a\">\n[Subagent hand-back] The report follows:\n  [harness: y]\n\n  One\n\n  Two\n</agent-message>", "", obj{"isMeta": true}), never)
	notification := entries(t, userLine("<task-notification><result>One\n\nTwo</result></task-notification>", "task-notification", nil), never)
	if handBack[0].Text != "One\n\nTwo" || handBack[0].Key != notification[0].Key {
		t.Fatalf("got %q %q", handBack[0].Text, notification[0].Text)
	}
}

func TestOtherAgentMessageWithoutOriginIsDiscarded(t *testing.T) {
	for _, s := range []string{
		"Another Claude session sent a message:\n<teammate-message teammate_id=\"a\" color=\"blue\">hi</teammate-message>",
		"  Another Claude session sent a message:\nhi",
	} {
		if got := entries(t, userLine(s, "", nil), never); len(got) != 0 {
			t.Errorf("%q: got %v", s, got)
		}
	}
}

func TestHarnessInterruptionIsDiscarded(t *testing.T) {
	for _, origin := range []string{"", "human"} {
		for _, s := range []string{"[Request interrupted by user]", "[Request interrupted by user for tool use]"} {
			if got := entries(t, userLine(s, origin, nil), never); len(got) != 0 {
				t.Errorf("%q %q: got %v", origin, s, got)
			}
			if got := entries(t, queuedLine(s, "prompt", "human"), never); len(got) != 0 {
				t.Errorf("queued %q: got %v", s, got)
			}
		}
	}
}

func TestInterruptionQuotedInSentenceIsKept(t *testing.T) {
	s := "saw [Request interrupted by user] yesterday"
	if got := entries(t, userLine(s, "", nil), never); len(got) != 1 || got[0].Text != s {
		t.Fatalf("got %v", got)
	}
}

func TestNotificationEscapedResultSameKeyOnEveryPath(t *testing.T) {
	report, escaped := "a < b & c > d", "a &lt; b &amp; c &gt; d"
	body := "<task-notification><result>" + escaped + "</result></task-notification>"
	notification := entries(t, userLine(body, "task-notification", nil), never)
	inQueue := entries(t, queuedLine(body, "task-notification", ""), never)
	handBack := entries(t, userLine("<agent-message from=\"a\">\n[Subagent hand-back] The report follows:\n  "+report+"\n</agent-message>", "", obj{"isMeta": true}), never)
	result := entries(t, resultLine(report, "t1", nil), always)
	if notification[0].Text != report || inQueue[0].Text != report {
		t.Fatalf("got %q %q", notification[0].Text, inQueue[0].Text)
	}
	k := notification[0].Key
	if inQueue[0].Key != k || handBack[0].Key != k || result[0].Key != k {
		t.Fatalf("keys differ")
	}
}

func TestNotificationWithoutReportIsDiscarded(t *testing.T) {
	for _, result := range []string{
		"This agent has not reported yet",
		"&lt;artifact-content-authored-by-others/&gt; This agent read Artifact content.\nThis agent's report was delivered to you as a message from \"x\" (its SubagentHandback call).",
	} {
		o := userLine("<task-notification><result>"+result+"</result></task-notification>", "task-notification", nil)
		if got := entries(t, o, never); len(got) != 0 {
			t.Errorf("%q: got %v", result, got)
		}
	}
}

func TestReportQuotingDeliveryPhraseIsKept(t *testing.T) {
	o := userLine("<task-notification><result>The text says: report was delivered elsewhere</result></task-notification>", "task-notification", nil)
	if got := entries(t, o, never); len(got) != 1 || got[0].Text != "The text says: report was delivered elsewhere" {
		t.Fatalf("got %v", got)
	}
}

func TestPasteEnvelopeIsRemoved(t *testing.T) {
	cases := [][2]string{
		{"<pasted_content id=\"999b\">\nthe text\n</pasted_content id=\"999b\">", "the text"},
		{"before <pasted_content id=\"1\">\none\n</pasted_content id=\"1\"> middle <pasted_content id=\"2\">\ntwo\n</pasted_content id=\"2\"> end", "before one middle two end"},
		{"line\n<pasted_content id=\"1\">\nl1\nl2\n</pasted_content id=\"1\">", "line\nl1\nl2"},
		{"<pasted_content id=\"1\">\nunclosed", "<pasted_content id=\"1\">\nunclosed"},
		{"<pasted_content id=\"1\">\nx\n</pasted_content id=\"2\">", "<pasted_content id=\"1\">\nx\n</pasted_content id=\"2\">"},
	}
	for _, c := range cases {
		if got := entries(t, userLine(c[0], "human", nil), never); len(got) != 1 || got[0].Text != c[1] {
			t.Errorf("%q: got %v", c[0], got)
		}
	}
}

func TestTextIsMasked(t *testing.T) {
	secret := "Bearer abcdefghijklmnopqrstuvwxyz123456"
	got := entries(t, userLine("my token is "+secret, "human", nil), never)
	if strings.Contains(got[0].Text, "abcdefghijklmnop") || !strings.Contains(got[0].Text, mask.Marker) {
		t.Fatalf("got %q", got[0].Text)
	}
}

func journalLine(extra obj, drop ...string) []byte {
	m := obj{"v": 1, "id": "m1", "session": "sess", "ts": ts, "role": "user", "text": "hello"}
	for k, v := range extra {
		m[k] = v
	}
	for _, k := range drop {
		delete(m, k)
	}
	raw, _ := json.Marshal(m)
	return raw
}

func TestJournalV1(t *testing.T) {
	for _, role := range []string{"user", "claude", "subagent"} {
		items, err := ParseJournalLine(journalLine(obj{"role": role}), "voice.jsonl")
		if err != nil || len(items) != 1 {
			t.Fatalf("%s: %v %v", role, items, err)
		}
		want := Entry{Key: "journal:voice.jsonl:m1", SessionID: "sess", TS: "2026-10-26T23:45:57.000Z", Role: role, Text: "hello"}
		if !reflect.DeepEqual(items[0], want) {
			t.Errorf("%s: got %+v", role, items[0])
		}
	}
}

func TestJournalTitleAndCwd(t *testing.T) {
	items, err := ParseJournalLine(journalLine(obj{"title": "Morning chat", "cwd": "/work/demo"}), "j")
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %v", items, err)
	}
	if e := items[0].(Entry); e.Cwd == nil || *e.Cwd != "/work/demo" || e.GitBranch != nil {
		t.Errorf("entry %+v", e)
	}
	if items[1] != (Title{"sess", "Morning chat"}) {
		t.Errorf("title %v", items[1])
	}
}

func TestJournalMasksAndNormalizes(t *testing.T) {
	items, err := ParseJournalLine(journalLine(obj{"text": "tok Bearer abcdefghijklmnopqrstuvwxyz123456", "ts": "2026-10-26T23:45:57.123+02:00", "title": "Bearer abcdefghijklmnopqrstuvwxyz123456"}), "j")
	if err != nil {
		t.Fatal(err)
	}
	e := items[0].(Entry)
	if e.TS != "2026-10-26T21:45:57.123Z" || strings.Contains(e.Text, "abcdefghijklmnop") || strings.Contains(items[1].(Title).Title, "abcdefghijklmnop") {
		t.Errorf("got %+v %+v", e, items[1])
	}
}

func TestJournalRejectsUnknownRole(t *testing.T) {
	for _, role := range []string{"jonathan", "tool", ""} {
		if _, err := ParseJournalLine(journalLine(obj{"role": role}), "j"); err == nil {
			t.Errorf("role %q accepted", role)
		}
	}
}

func TestJournalRejectsMissingV(t *testing.T) {
	if _, err := ParseJournalLine(journalLine(nil, "v"), "j"); err == nil {
		t.Fatal("missing v accepted")
	}
	for _, v := range []any{2, "1", nil} {
		if _, err := ParseJournalLine(journalLine(obj{"v": v}), "j"); err == nil {
			t.Errorf("v %v accepted", v)
		}
	}
}

func TestJournalRejectsInvalidLines(t *testing.T) {
	cases := map[string][]byte{"bad json": []byte(`{"v":1,`), "not an object": []byte(`[1]`), "bad ts": journalLine(obj{"ts": "yesterday"}),
		"empty text": journalLine(obj{"text": "  "}), "bad cwd": journalLine(obj{"cwd": 3})}
	for _, field := range []string{"id", "session", "ts", "role", "text"} {
		cases["missing "+field] = journalLine(nil, field)
	}
	for name, raw := range cases {
		if items, err := ParseJournalLine(raw, "j"); err == nil {
			t.Errorf("%s accepted: %v", name, items)
		}
	}
}

func TestEntryPathsFromTools(t *testing.T) {
	cases := []struct {
		name  string
		input obj
		want  []string
	}{
		{"Read", obj{"file_path": "/x/a.go"}, []string{"/x/a.go"}},
		{"Edit", obj{"file_path": "/x/b.go"}, []string{"/x/b.go"}},
		{"Write", obj{"file_path": "/x/c.go"}, []string{"/x/c.go"}},
		{"NotebookEdit", obj{"notebook_path": "/x/n.ipynb"}, []string{"/x/n.ipynb"}},
		{"Grep", obj{"pattern": "p", "path": "/x/src"}, []string{"/x/src"}},
		{"Bash", obj{"command": "git -C /x status"}, []string{"/x"}},
		{"Bash", obj{"command": "cd /x && ls"}, []string{"/x"}},
		{"Bash", obj{"command": `cd "/x y"; ls`}, []string{"/x y"}},
		{"Bash", obj{"command": "ls /x"}, nil},
		{"Read", obj{"file_path": "rel/a.go"}, nil},
		{"Bash", obj{"command": "cd rel && ls"}, nil},
		{"Read", obj{"file_path": "/x/a\x00b"}, nil},
	}
	for _, c := range cases {
		got := entries(t, assistantLine([]any{toolUseBlock(c.name, c.input)}, nil), never)
		if len(got) != 1 || !reflect.DeepEqual(got[0].Paths, c.want) {
			t.Errorf("%s %v: got %v, want %v", c.name, c.input, got, c.want)
		}
	}
}
