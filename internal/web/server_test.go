package web

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/index"
	"github.com/jplot/brain/internal/lock"
	"github.com/jplot/brain/internal/memo"
	"github.com/jplot/brain/internal/paths"
	"github.com/jplot/brain/internal/subjects"
)

const ts = "2026-10-20T10:00:00.000Z"

var claudeMarker string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "web-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	claudeMarker = filepath.Join(dir, "claude-called")
	script := "#!/bin/sh\ntouch " + claudeMarker + "\nexit 97\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	closeBrowser()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fault struct {
	status int
	abort  bool
	held   chan struct{}
}

type fixture struct {
	t    *testing.T
	p    paths.Paths
	s    *server
	conn *sql.DB
	n    int

	mu     sync.Mutex
	faults map[string]fault
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: filepath.Join(dir, "transcripts")}
	s, err := listen(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, p: p, s: s, faults: map[string]fault{}}
	h := &http.Server{Handler: http.HandlerFunc(f.serve), ErrorLog: log.New(io.Discard, "", 0)}
	go h.Serve(s.ln)
	conn, err := db.Open(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.conn = conn
	t.Cleanup(func() {
		h.Close()
		s.conn.Close()
		conn.Close()
	})
	return f
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	broken, ok := f.faults[r.URL.Path]
	f.mu.Unlock()
	switch {
	case ok && broken.held != nil:
		f.mu.Lock()
		delete(f.faults, r.URL.Path)
		f.mu.Unlock()
		<-broken.held
		f.s.ServeHTTP(w, r)
	case ok && broken.abort:
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	case ok:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(broken.status)
		fmt.Fprintf(w, `{"error": "panne %d"}`, broken.status)
	default:
		f.s.ServeHTTP(w, r)
	}
}

func (f *fixture) hold(path string) (release func()) {
	held := make(chan struct{})
	release = sync.OnceFunc(func() { close(held) })
	f.t.Cleanup(release)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[path] = fault{held: held}
	return release
}

func (f *fixture) fail(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[path] = fault{status: status}
}

func (f *fixture) abort(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[path] = fault{abort: true}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.conn.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(query string, args ...any) (n int) {
	f.t.Helper()
	if err := f.conn.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

type obj = map[string]any

var counter atomic.Int64

func userLine(session, at, text string) obj {
	return obj{"type": "user", "uuid": fmt.Sprintf("u%d", counter.Add(1)), "sessionId": session,
		"timestamp": at, "cwd": "/p", "gitBranch": "main", "origin": obj{"kind": "human"},
		"message": obj{"role": "user", "content": text}}
}

func claudeLine(session, at string, content []obj) obj {
	return obj{"type": "assistant", "uuid": fmt.Sprintf("u%d", counter.Add(1)), "sessionId": session,
		"timestamp": at, "cwd": "/p", "gitBranch": "main",
		"message": obj{"role": "assistant", "content": content}}
}

func (f *fixture) transcript(session string, lines ...obj) {
	f.t.Helper()
	path := filepath.Join(f.p.Transcripts, "-work", fmt.Sprintf("%s-%d.jsonl", session, counter.Add(1)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			f.t.Fatal(err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) index() {
	f.t.Helper()
	if _, err := index.Run(f.p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) say(session, at, text string, extra ...obj) {
	f.t.Helper()
	f.transcript(session, append([]obj{userLine(session, at, text)}, extra...)...)
	f.index()
}

func (f *fixture) subject(name string, aliases ...string) string {
	f.t.Helper()
	uid, err := subjects.Create(f.conn, name, aliases, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return uid
}

type episodeRow struct {
	session, title, summary, start string
	stale                          bool
	subjects                       []string
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *fixture) episode(e episodeRow) int64 {
	f.t.Helper()
	f.n++
	uid := fmt.Sprint("e", f.n)
	if e.start == "" {
		e.start = ts
	}
	res, err := f.conn.Exec("INSERT INTO episodes(uid, session_id, start_ts, end_ts, title, summary, created_at, updated_at, summary_stale)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", uid, e.session, e.start, e.start, nullable(e.title), nullable(e.summary), e.start, e.start, e.stale)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, s := range e.subjects {
		f.exec("INSERT INTO episode_subjects VALUES (?, ?)", uid, s)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fixture) point(episode int64, subject, kind, text string, importance int) int64 {
	f.t.Helper()
	f.n++
	res, err := f.conn.Exec("INSERT INTO points(uid, episode_id, kind, subject_uid, text, importance, source_ts, created_at, updated_at)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", fmt.Sprint("p", f.n), episode, kind, nullable(subject), text, importance, ts, ts, ts)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

type world struct {
	a, b                 string
	ea, eb               int64
	open, decision, fact int64
}

func (f *fixture) indexed() world {
	f.t.Helper()
	f.say("sess-A", ts, "mot de A")
	f.say("sess-B", ts, "mot de B")
	return world{a: f.subject("Alpha"), b: f.subject("Beta")}
}

func (f *fixture) memories() world {
	f.t.Helper()
	w := f.indexed()
	w.ea = f.episode(episodeRow{session: "sess-A", title: "Titre A", summary: "Résumé A", subjects: []string{w.a}})
	w.eb = f.episode(episodeRow{session: "sess-B", title: "Titre B", summary: "Résumé B", subjects: []string{w.b}})
	w.open = f.point(w.ea, w.a, "pending", "Écrire la clé d'unicité", 4)
	w.decision = f.point(w.ea, w.a, "decision", "Une facture est unique", 5)
	w.fact = f.point(w.eb, w.b, "fact", "Équipe de six", 2)
	return w
}

type call struct {
	method, path, host string
	token              *string
	headers            map[string]string
	body               []byte
}

type answer struct {
	status  int
	headers http.Header
	body    []byte
}

func (a answer) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(a.body, v); err != nil {
		t.Fatalf("%v : %s", err, a.body)
	}
}

func (f *fixture) do(c call) answer {
	f.t.Helper()
	token := f.s.token
	if c.token != nil {
		token = *c.token
	}
	path := c.path
	headers := map[string]string{}
	for k, v := range c.headers {
		headers[k] = v
	}
	if token != "" && c.method != "POST" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "token=" + token
	} else if token != "" {
		headers["X-Paul-Token"] = token
	}
	req, err := http.NewRequest(c.method, "http://"+f.s.host+path, bytes.NewReader(c.body))
	if err != nil {
		f.t.Fatal(err)
	}
	if c.body == nil {
		req.Body, req.ContentLength = nil, 0
	}
	req.Host = f.s.host
	if c.host != "" {
		req.Host = strings.ReplaceAll(c.host, "{port}", f.port())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return answer{res.StatusCode, res.Header, body}
}

func (f *fixture) port() string {
	_, port, _ := net.SplitHostPort(f.s.host)
	return port
}

func ptr(s string) *string { return &s }

func (f *fixture) get(path string) answer {
	f.t.Helper()
	return f.do(call{method: "GET", path: path})
}

func (f *fixture) post(path string, payload any) answer {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.postRaw(path, raw, "application/json")
}

func (f *fixture) postRaw(path string, raw []byte, contentType string) answer {
	f.t.Helper()
	headers := map[string]string{}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	return f.do(call{method: "POST", path: path, headers: headers, body: raw})
}

func (f *fixture) status(t *testing.T, a answer, want int) {
	t.Helper()
	if a.status != want {
		t.Fatalf("statut %d, attendu %d : %s", a.status, want, a.body)
	}
}

func (f *fixture) points(query string) []map[string]any {
	f.t.Helper()
	var page struct {
		Points []map[string]any `json:"points"`
	}
	a := f.get("/api/points?" + query)
	f.status(f.t, a, 200)
	a.json(f.t, &page)
	return page.Points
}

func (f *fixture) sessions(query string) []map[string]any {
	f.t.Helper()
	var rows []map[string]any
	a := f.get("/api/sessions?" + query)
	f.status(f.t, a, 200)
	a.json(f.t, &rows)
	return rows
}

func (f *fixture) sessionRow(id string) map[string]any {
	f.t.Helper()
	for _, r := range f.sessions("") {
		if r["id"] == id {
			return r
		}
	}
	f.t.Fatalf("session %s absente", id)
	return nil
}

func field[T any](rows []map[string]any, key string) []T {
	out := make([]T, len(rows))
	for i, r := range rows {
		out[i], _ = r[key].(T)
	}
	return out
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}

func pointByID(t *testing.T, rows []map[string]any, id int64) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r["id"] == float64(id) {
			return r
		}
	}
	t.Fatalf("point %d absent", id)
	return nil
}

var securityHeaders = map[string]string{
	"Cache-Control":           "no-store",
	"Referrer-Policy":         "no-referrer",
	"X-Content-Type-Options":  "nosniff",
	"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; base-uri 'none'",
}

func TestListensOnLoopbackOnly(t *testing.T) {
	f := newFixture(t)
	if host, _, _ := net.SplitHostPort(f.s.ln.Addr().String()); host != "127.0.0.1" {
		t.Fatal(host)
	}
}

func TestMissingOrWrongTokenIs403(t *testing.T) {
	f := newFixture(t)
	for _, token := range []string{"", "mauvais", "%C3%A9"} {
		a := f.do(call{method: "GET", path: "/api/subjects", token: ptr(token)})
		f.status(t, a, 403)
		if string(a.body) != `{"error":"refusé"}` {
			t.Fatalf("%s", a.body)
		}
	}
}

func TestForeignHostIs403(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	for _, host := range []string{"evil.example", "evil.example:{port}", "localhost:{port}", "127.0.0.1:1", "127.0.0.1"} {
		f.status(t, f.do(call{method: "GET", path: "/api/subjects", host: host}), 403)
		raw, _ := json.Marshal(obj{"uid": w.a, "name": "Pirate"})
		a := f.do(call{method: "POST", path: "/api/subjects/rename", host: host, body: raw,
			headers: map[string]string{"Content-Type": "application/json"}})
		f.status(t, a, 403)
	}
	if s, _ := subjects.Get(f.conn, w.a); s.Name != "Alpha" {
		t.Fatal(s.Name)
	}
}

func TestAbsoluteFormRequestWithForeignHostIs403(t *testing.T) {
	f := newFixture(t)
	conn, err := net.Dial("tcp", f.s.host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET http://%s/api/subjects?token=%s HTTP/1.1\r\nHost: evil.example\r\nConnection: close\r\n\r\n", f.s.host, f.s.token)
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("%q %v", status, err)
	}
}

func TestPostTokenOnlyInTheHeader(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	raw, _ := json.Marshal(obj{"uid": w.a, "name": "Pirate"})
	jsonType := map[string]string{"Content-Type": "application/json"}
	f.status(t, f.do(call{method: "POST", path: "/api/subjects/rename?token=" + f.s.token, token: ptr(""), body: raw, headers: jsonType}), 403)
	f.status(t, f.do(call{method: "POST", path: "/api/subjects/rename", token: ptr("mauvais"), body: raw, headers: jsonType}), 403)
	f.status(t, f.do(call{method: "GET", path: "/api/subjects", token: ptr(""), headers: map[string]string{"X-Paul-Token": f.s.token}}), 403)
	if s, _ := subjects.Get(f.conn, w.a); s.Name != "Alpha" {
		t.Fatal(s.Name)
	}
}

func TestTokensDistinctAndLong(t *testing.T) {
	dir := t.TempDir()
	p := paths.Paths{Home: dir, Transcripts: dir}
	first, err := listen(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.ln.Close()
	defer first.conn.Close()
	second, err := listen(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.ln.Close()
	defer second.conn.Close()
	if first.token == second.token || len(first.token) != 43 || len(second.token) != 43 {
		t.Fatal(first.token, second.token)
	}
}

func TestPostWantsJSON(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	f.status(t, f.postRaw("/api/subjects/rename", []byte("uid=x"), "text/plain"), 415)
	f.status(t, f.postRaw("/api/subjects/rename", []byte("{}"), ""), 415)
	raw, _ := json.Marshal(obj{"uid": w.a, "name": "Gamma"})
	f.status(t, f.postRaw("/api/subjects/rename", raw, "application/json; charset=utf-8"), 200)
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	f := newFixture(t)
	for _, c := range []call{
		{method: "GET", path: "/api/subjects"}, {method: "GET", path: "/api/subjects", token: ptr("")},
		{method: "GET", path: "/api/inconnue"}, {method: "GET", path: "/app.js"}, {method: "GET", path: "/"},
	} {
		a := f.do(c)
		for name, value := range securityHeaders {
			if got := a.headers.Get(name); got != value {
				t.Fatalf("%s %s : %s = %q", c.path, a.body, name, got)
			}
		}
		for name := range a.headers {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Fatal(name)
			}
		}
	}
}

func TestUnhandledMethodsCheckedBefore501(t *testing.T) {
	f := newFixture(t)
	for _, method := range []string{"DELETE", "PUT", "PATCH", "OPTIONS", "HEAD", "TRACE", "get"} {
		f.status(t, f.do(call{method: method, path: "/api/subjects", token: ptr(""), host: "evil.example"}), 403)
		f.status(t, f.do(call{method: method, path: "/api/subjects", token: ptr("")}), 403)
		f.status(t, f.do(call{method: method, path: "/api/subjects", host: "evil.example"}), 403)
		a := f.do(call{method: method, path: "/api/subjects"})
		f.status(t, a, 501)
		if a.headers.Get("Content-Security-Policy") != securityHeaders["Content-Security-Policy"] {
			t.Fatal(method, a.headers)
		}
		if method == "HEAD" && len(a.body) != 0 {
			t.Fatalf("%q", a.body)
		}
	}
	if a := f.do(call{method: "DELETE", path: "/"}); string(a.body) != `{"error":"requête refusée"}` {
		t.Fatalf("%s", a.body)
	}
}

func TestBodyLimits(t *testing.T) {
	f := newFixture(t)
	f.status(t, f.postRaw("/api/subjects/rename", bytes.Repeat([]byte(" "), 64<<10+1), "application/json"), 413)
	for _, raw := range []string{"{", "[]", `"x"`, "\xff\xfe", "null", strings.Repeat("[", 60000)} {
		f.status(t, f.postRaw("/api/subjects/rename", []byte(raw), "application/json"), 400)
	}
}

func TestInternalErrorIs500WithoutLeak(t *testing.T) {
	f := newFixture(t)
	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(saved)
	if err := os.Mkdir(f.p.IndexLock(), 0o700); err != nil {
		t.Fatal(err)
	}
	a := f.get("/api/refresh?secret=graine-secrete")
	f.status(t, a, 500)
	if string(a.body) != `{"error":"erreur interne"}` {
		t.Fatalf("%s", a.body)
	}
	text := logged.String()
	if !strings.Contains(text, "refresh") || !strings.Contains(text, "PathError") {
		t.Fatalf("%q", text)
	}
	for _, leak := range []string{f.p.Home, "graine-secrete", f.s.token} {
		if strings.Contains(text, leak) {
			t.Fatalf("%q dans %q", leak, text)
		}
	}
}

func TestNoRequestIsLogged(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(saved)
	f.get("/api/search?q=mot")
	f.do(call{method: "GET", path: "/api/search", token: ptr("")})
	f.get("/api/inconnue")
	if logged.Len() != 0 {
		t.Fatalf("%q", logged.String())
	}
}

func TestConcurrentRequests(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(saved)
	calls := []func() answer{
		func() answer { return f.get("/api/refresh") },
		func() answer { return f.get("/api/search?q=mot") },
		func() answer { return f.get("/api/subjects") },
		func() answer { return f.post("/api/subjects/rename", obj{"uid": w.a, "name": "Alpha"}) },
		func() answer { return f.get("/api/sessions") },
	}
	var wg sync.WaitGroup
	var failed atomic.Int64
	for n := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				if a := calls[(n+i)%5](); a.status != 200 {
					failed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || logged.Len() != 0 {
		t.Fatalf("%d échecs, journal %q", failed.Load(), logged.String())
	}
}

func TestStaticClosedList(t *testing.T) {
	f := newFixture(t)
	f.s.static = fstest.MapFS{"app.js": {Data: []byte("ok")}, "autre.txt": {Data: []byte("autre")}}
	f.status(t, f.get("/app.js"), 200)
	for _, path := range []string{"/../paul.db", "/autre.txt", "/static/app.js", "/%2e%2e/secret", "/style.css"} {
		f.status(t, f.get(path), 404)
	}
	f.status(t, f.do(call{method: "GET", path: "/app.js", token: ptr("")}), 403)
	f.status(t, f.do(call{method: "GET", path: "/nimporte", token: ptr("")}), 403)
}

func TestPageGetsTheTokenAndItsResourcesLoad(t *testing.T) {
	f := newFixture(t)
	f.s.static = fstest.MapFS{
		"index.html": {Data: []byte(`<script src="/app.js?token=__TOKEN__"></script>`)},
		"app.js":     {Data: []byte("x")},
		"style.css":  {Data: []byte("body{}")},
	}
	page := string(f.get("/").body)
	if strings.Contains(page, "__TOKEN__") || !strings.Contains(page, "token="+f.s.token) {
		t.Fatal(page)
	}
	src := regexp.MustCompile(`src="([^"]+)"`).FindStringSubmatch(page)[1]
	f.status(t, f.do(call{method: "GET", path: src, token: ptr("")}), 200)
	a := f.get("/style.css")
	if a.status != 200 || string(a.body) != "body{}" || a.headers.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatal(a.status, string(a.body), a.headers)
	}
}

func TestEmbeddedPage(t *testing.T) {
	f := newFixture(t)
	page := f.get("/")
	f.status(t, page, 200)
	if !strings.Contains(string(page.body), "<title>paul</title>") || !strings.Contains(string(page.body), "token="+f.s.token) {
		t.Fatalf("%s", page.body)
	}
	for _, name := range []string{"/app.js", "/style.css"} {
		a := f.get(name)
		f.status(t, a, 200)
		if regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|https?://`).Match(a.body) {
			t.Fatalf("%s charge du HTML ou une ressource externe", name)
		}
	}
}

func TestSearchSeesEverySessionWithSegments(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	var hits []map[string]any
	a := f.get("/api/search?q=mot")
	f.status(t, a, 200)
	a.json(t, &hits)
	if got := sorted(field[string](hits, "session_id")...); !slices.Equal(got, []string{"sess-A", "sess-B"}) {
		t.Fatal(got)
	}
	for _, h := range hits {
		if h["session_id"] == "sess-A" && fmt.Sprint(h["segments"]) != "[[mot true] [ de A false]]" {
			t.Fatal(h["segments"])
		}
	}
	if bytes.Contains(a.body, []byte(`\u0001`)) || bytes.Contains(a.body, []byte("<")) {
		t.Fatalf("%s", a.body)
	}
}

func TestSearchFilters(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	var hits []map[string]any
	f.get("/api/search?q=mot&subject="+w.a).json(t, &hits)
	if got := field[string](hits, "session_id"); !slices.Equal(got, []string{"sess-A"}) {
		t.Fatal(got)
	}
	if fmt.Sprint(hits[0]["subjects"]) != "[Alpha]" {
		t.Fatal(hits[0])
	}
	f.get("/api/search?q=mot&limit=1").json(t, &hits)
	if len(hits) != 1 {
		t.Fatal(hits)
	}
	f.get("/api/search?q=mot&since=2026-10-20&until=2026-10-20").json(t, &hits)
	if len(hits) != 2 {
		t.Fatal(hits)
	}
	if a := f.get("/api/search?q=mot&since=2026-10-21"); string(a.body) != "[]" {
		t.Fatalf("%s", a.body)
	}
}

func TestInvalidQueries400(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	for _, path := range []string{"/api/search?q=", "/api/search?q=%20", "/api/search",
		"/api/search?q=mot&since=2026-13-01", "/api/search?q=mot&since=hier", "/api/search?q=mot&until=",
		"/api/points?kind=autre", "/api/points?open=oui", "/api/points?open=",
		"/api/points?offset=-1", "/api/points?offset=abc", "/api/points?offset=", "/api/points?offset=1.5", "/api/points?offset=100000000",
	} {
		f.status(t, f.get(path), 400)
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=abc", "limit=-1", "limit=", "limit=1.5"} {
		f.status(t, f.get("/api/sessions?"+q), 400)
		f.status(t, f.get("/api/search?q=mot&"+q), 400)
		f.status(t, f.get("/api/points?"+q), 400)
	}
	if a := f.get("/api/points?kind=x"); string(a.body) != `{"error":"kind invalide"}` {
		t.Fatalf("%s", a.body)
	}
}

func TestUnknownOrAmbiguousSubject(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	for _, path := range []string{"/api/sessions?subject=inconnu", "/api/search?q=x&subject=inconnu",
		"/api/points?subject=inconnu", "/api/memo?subject=inconnu", "/api/memo?subject="} {
		f.status(t, f.get(path), 404)
	}
	f.subject("Un", "dup")
	f.subject("Deux", "dup")
	for _, path := range []string{"/api/sessions?subject=dup", "/api/points?subject=dup", "/api/memo?subject=dup"} {
		f.status(t, f.get(path), 400)
	}
}

func TestSessionsList(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	rows := f.sessions("")
	if got := sorted(field[string](rows, "id")...); !slices.Equal(got, []string{"sess-A", "sess-B"}) {
		t.Fatal(got)
	}
	want := sorted("id", "title", "started_at", "ended_at", "n_messages", "subjects", "episode_title", "summary", "decisions", "pending")
	if got := keys(rows[0]); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	if rows[0]["title"] != nil || fmt.Sprint(rows[0]["subjects"]) != "[]" {
		t.Fatal(rows[0])
	}
}

func TestSessionsFiltersAndLimit(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	if got := field[string](f.sessions("subject="+w.a), "id"); !slices.Equal(got, []string{"sess-A"}) {
		t.Fatal(got)
	}
	if got := f.sessions("before=2026-10-20T00:00:00.000Z"); len(got) != 0 {
		t.Fatal(got)
	}
	f.say("vieille", "2026-10-19T10:00:00.000Z", "x")
	f.say("recente", "2026-10-21T10:00:00.000Z", "x")
	if got := field[string](f.sessions("limit=1"), "id"); !slices.Equal(got, []string{"recente"}) {
		t.Fatal(got)
	}
}

func TestSessionAndItsMessages(t *testing.T) {
	f := newFixture(t)
	f.indexed()
	var session struct {
		ID       string           `json:"id"`
		Title    *string          `json:"title"`
		Messages []map[string]any `json:"messages"`
		Episodes []map[string]any `json:"episodes"`
	}
	a := f.get("/api/sessions/sess-A")
	f.status(t, a, 200)
	a.json(t, &session)
	if session.ID != "sess-A" || session.Title != nil || len(session.Messages) != 1 || session.Messages[0]["text"] != "mot de A" {
		t.Fatalf("%s", a.body)
	}
	if got := keys(session.Messages[0]); !slices.Equal(got, sorted("ts", "role", "git_branch", "text")) {
		t.Fatal(got)
	}
	if string(mustJSON(t, session.Episodes)) != "[]" {
		t.Fatalf("%s", a.body)
	}
	f.status(t, f.get("/api/sessions/sess-"), 400)
	f.status(t, f.get("/api/sessions/nope"), 404)
	f.status(t, f.get("/api/sessions/a/b"), 404)
	f.status(t, f.get("/api/inconnue"), 404)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRefreshIndexesNewTranscripts(t *testing.T) {
	f := newFixture(t)
	f.transcript("neuf", userLine("neuf", ts, "nouveau"))
	if a := f.get("/api/search?q=nouveau"); string(a.body) != "[]" {
		t.Fatalf("%s", a.body)
	}
	if a := f.get("/api/refresh"); string(a.body) != `{"ok":true}` {
		t.Fatalf("%s", a.body)
	}
	var hits []any
	f.get("/api/search?q=nouveau").json(t, &hits)
	if len(hits) != 1 {
		t.Fatal(hits)
	}
}

func TestPointsFieldsAndOrder(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	rows := f.points("")
	if got := field[string](rows, "text"); !slices.Equal(got, []string{"Une facture est unique", "Écrire la clé d'unicité", "Équipe de six"}) {
		t.Fatal(got)
	}
	want := map[string]any{
		"id": float64(w.decision), "episode_id": float64(w.ea), "session_id": "sess-A", "kind": "decision",
		"text": "Une facture est unique", "importance": float64(5), "source_ts": ts, "created_at": ts,
		"closed_at": nil, "corrected": false, "subject_uid": w.a, "subject_name": "Alpha",
	}
	if fmt.Sprint(rows[0]) != fmt.Sprint(want) {
		t.Fatalf("%v\n%v", rows[0], want)
	}
}

func TestPointsFilters(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	if got := field[string](f.points("kind=fact"), "kind"); !slices.Equal(got, []string{"fact"}) {
		t.Fatal(got)
	}
	if got := field[string](f.points("subject="+w.b), "text"); !slices.Equal(got, []string{"Équipe de six"}) {
		t.Fatal(got)
	}
	if got := field[string](f.points("open=1"), "kind"); !slices.Equal(got, []string{"pending"}) {
		t.Fatal(got)
	}
	if got := field[string](f.points("q=six"), "text"); !slices.Equal(got, []string{"Équipe de six"}) {
		t.Fatal(got)
	}
	if len(f.points("limit=1")) != 1 {
		t.Fatal()
	}
	f.status(t, f.post("/api/points/close", obj{"id": w.open}), 200)
	if len(f.points("open=1")) != 0 || len(f.points("")) != 3 {
		t.Fatal(f.points(""))
	}
}

func TestPointsOffsetAndTotal(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	var first, rest struct {
		Points []map[string]any `json:"points"`
		Total  int              `json:"total"`
	}
	f.get("/api/points?limit=2").json(t, &first)
	f.get("/api/points?limit=2&offset=2").json(t, &rest)
	if first.Total != 3 || len(first.Points) != 2 || rest.Total != 3 || len(rest.Points) != 1 {
		t.Fatal(first, rest)
	}
	seen := append(field[float64](first.Points, "id"), field[float64](rest.Points, "id")...)
	slices.Sort(seen)
	want := []float64{float64(w.open), float64(w.decision), float64(w.fact)}
	slices.Sort(want)
	if !slices.Equal(seen, want) {
		t.Fatal(seen)
	}
	if a := f.get("/api/points?offset=3"); string(a.body) != `{"points":[],"total":3}` {
		t.Fatalf("%s", a.body)
	}
	f.get("/api/points?kind=fact&limit=1").json(t, &first)
	f.get("/api/points?open=1").json(t, &rest)
	if first.Total != 1 || rest.Total != 1 {
		t.Fatal(first.Total, rest.Total)
	}
}

var postRoutes = []string{"/api/points/correct", "/api/points/close", "/api/points/forget", "/api/sessions/forget",
	"/api/subjects/rename", "/api/subjects/merge", "/api/subjects/parent", "/api/subjects/link", "/api/subjects/unlink"}

func TestEveryRouteIsProtected(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	for _, path := range []string{"/api/points", "/api/memo", "/api/counts", "/api/subjects", "/api/sessions", "/api/refresh"} {
		f.status(t, f.do(call{method: "GET", path: path, token: ptr("")}), 403)
		f.status(t, f.do(call{method: "GET", path: path, token: ptr("mauvais")}), 403)
		f.status(t, f.do(call{method: "GET", path: path, host: "evil.example"}), 403)
	}
	raw := mustJSON(t, obj{"id": w.open})
	jsonType := map[string]string{"Content-Type": "application/json"}
	for _, path := range postRoutes {
		f.status(t, f.do(call{method: "POST", path: path, body: raw, headers: jsonType, token: ptr("")}), 403)
		f.status(t, f.do(call{method: "POST", path: path, body: raw, headers: jsonType, token: ptr("mauvais")}), 403)
		f.status(t, f.do(call{method: "POST", path: path, body: raw, headers: jsonType, host: "evil.example"}), 403)
		f.status(t, f.do(call{method: "POST", path: path + "?token=" + f.s.token, body: raw, headers: jsonType, token: ptr("")}), 403)
		f.status(t, f.postRaw(path, []byte("id=1"), "text/plain"), 415)
		f.status(t, f.postRaw(path, bytes.Repeat([]byte(" "), 64<<10+1), "application/json"), 413)
		f.status(t, f.postRaw(path, []byte("[]"), "application/json"), 400)
		f.status(t, f.postRaw(path, []byte("{"), "application/json"), 400)
	}
	if len(f.points("")) != 3 {
		t.Fatal()
	}
}

func TestInvalidIDs400(t *testing.T) {
	f := newFixture(t)
	f.memories()
	for _, path := range postRoutes[:3] {
		for _, bad := range []string{"true", `"1"`, "1.5", "null", "[1]", `{"a":1}`, "0", "-1", "1000000000000000000000000000000"} {
			f.status(t, f.postRaw(path, []byte(`{"id":`+bad+`,"importance":2}`), "application/json"), 400)
		}
		f.status(t, f.post(path, obj{"importance": 2}), 400)
		f.status(t, f.post(path, obj{"id": 99999, "importance": 2}), 404)
	}
	for _, bad := range []string{"true", "1", "1.5", "null", "[1]", `{"a":1}`} {
		f.status(t, f.postRaw("/api/sessions/forget", []byte(`{"id":`+bad+`}`), "application/json"), 400)
	}
	f.status(t, f.post("/api/sessions/forget", obj{}), 400)
	f.status(t, f.post("/api/sessions/forget", obj{"id": "nope"}), 404)
	if len(f.points("")) != 3 || len(f.sessions("")) != 2 {
		t.Fatal()
	}
}

func TestCorrection(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	a := f.post("/api/points/correct", obj{"id": w.open, "text": "Texte corrigé", "importance": 1})
	if a.status != 200 || string(a.body) != `{"ok":true}` {
		t.Fatalf("%d %s", a.status, a.body)
	}
	row := pointByID(t, f.points(""), w.open)
	if row["text"] != "Texte corrigé" || row["importance"] != float64(1) || row["corrected"] != true {
		t.Fatal(row)
	}
	f.status(t, f.post("/api/points/correct", obj{"id": w.open, "importance": 2}), 200)
	f.status(t, f.post("/api/points/correct", obj{"id": w.open, "text": "Autre"}), 200)
	if row := pointByID(t, f.points(""), w.open); row["text"] != "Autre" || row["importance"] != float64(2) {
		t.Fatal(row)
	}
	f.status(t, f.post("/api/points/correct", obj{"id": w.open, "text": strings.Repeat("é", 2000)}), 200)
	secret := "ghp_" + strings.Repeat("A1", 15)
	f.status(t, f.post("/api/points/correct", obj{"id": w.open, "text": "clé " + secret + " ici"}), 200)
	shown := string(f.get("/api/points").body)
	if strings.Contains(shown, secret) || !strings.Contains(shown, "[secret masqué]") {
		t.Fatal(shown)
	}
}

func TestInvalidCorrection400(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	for _, extra := range []string{``, `,"text":""`, `,"text":"   "`, `,"text":5`, `,"text":null`,
		`,"text":"` + strings.Repeat("x", 2001) + `"`, `,"importance":0`, `,"importance":6`, `,"importance":true`,
		`,"importance":3.0`, `,"importance":2.5`, `,"importance":"3"`, `,"importance":null,"text":null`,
		`,"text":"\u200b"`, `,"text":"\ufeff"`, `,"text":"\u0000"`, `,"text":" \u200b\u0000 "`} {
		raw := fmt.Sprintf(`{"id":%d%s}`, w.open, extra)
		f.status(t, f.postRaw("/api/points/correct", []byte(raw), "application/json"), 400)
	}
	row := pointByID(t, f.points(""), w.open)
	if row["text"] != "Écrire la clé d'unicité" || row["importance"] != float64(4) || row["corrected"] != false {
		t.Fatal(row)
	}
}

func TestForgetPointStalesTheSummary(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	if f.sessionRow("sess-A")["summary"] != "Résumé A" {
		t.Fatal(f.sessionRow("sess-A"))
	}
	f.status(t, f.post("/api/points/forget", obj{"id": w.open}), 200)
	if slices.Contains(field[float64](f.points(""), "id"), float64(w.open)) {
		t.Fatal()
	}
	row := f.sessionRow("sess-A")
	if row["summary"] != nil || row["episode_title"] != nil {
		t.Fatal(row)
	}
	if strings.Contains(string(f.get("/api/sessions/sess-A").body), "Résumé A") {
		t.Fatal()
	}
	if f.sessionRow("sess-B")["summary"] != "Résumé B" {
		t.Fatal()
	}
	top := f.point(w.ea, w.a, "fact", "nouveau", 3)
	if top <= w.fact {
		t.Fatal(top)
	}
}

func TestForgetSession(t *testing.T) {
	f := newFixture(t)
	f.memories()
	f.say("unique-1", ts, "autre")
	f.status(t, f.post("/api/sessions/forget", obj{"id": "sess-"}), 400)
	f.status(t, f.post("/api/sessions/forget", obj{"id": "unique"}), 404)
	f.status(t, f.post("/api/sessions/forget", obj{"id": "sess-A"}), 200)
	if got := sorted(field[string](f.sessions(""), "id")...); !slices.Equal(got, []string{"sess-B", "unique-1"}) {
		t.Fatal(got)
	}
	f.status(t, f.get("/api/sessions/sess-A"), 404)
	if got := field[string](f.points(""), "text"); !slices.Equal(got, []string{"Équipe de six"}) {
		t.Fatal(got)
	}
	f.status(t, f.post("/api/sessions/forget", obj{"id": "sess-A"}), 404)
}

func TestSessionSummaryAndCounters(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	f.point(w.ea, w.a, "pending", "Autre suspens", 3)
	row := f.sessionRow("sess-A")
	if row["episode_title"] != "Titre A" || row["summary"] != "Résumé A" || row["decisions"] != float64(1) || row["pending"] != float64(2) {
		t.Fatal(row)
	}
	if fmt.Sprint(row["subjects"]) != "[Alpha]" {
		t.Fatal(row)
	}
	f.post("/api/points/close", obj{"id": w.open})
	if f.sessionRow("sess-A")["pending"] != float64(1) {
		t.Fatal()
	}
}

func TestSessionEpisodes(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	row := f.sessionRow("sess-A")
	if row["episode_title"] != nil || row["summary"] != nil || row["decisions"] != float64(0) || row["pending"] != float64(0) {
		t.Fatal(row)
	}
	f.episode(episodeRow{session: "sess-A", title: "Titre A", summary: "Résumé A", subjects: []string{w.a}})
	f.episode(episodeRow{session: "sess-A", title: "Second", summary: "Suite", start: "2026-10-20T11:00:00.000Z"})
	var session struct {
		Episodes []map[string]any `json:"episodes"`
	}
	f.get("/api/sessions/sess-A").json(t, &session)
	if got := field[string](session.Episodes, "title"); !slices.Equal(got, []string{"Titre A", "Second"}) {
		t.Fatal(got)
	}
	if got := keys(session.Episodes[0]); !slices.Equal(got, sorted("id", "start_ts", "end_ts", "title", "summary", "subjects")) {
		t.Fatal(got)
	}
	if fmt.Sprint(session.Episodes[0]["subjects"]) != "[Alpha]" || fmt.Sprint(session.Episodes[1]["subjects"]) != "[]" {
		t.Fatal(session.Episodes)
	}
}

func TestStaleOrUntitledEpisodeNeverSent(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	f.episode(episodeRow{session: "sess-A", title: "Ancien", summary: "Ancien résumé", subjects: []string{w.a}})
	f.episode(episodeRow{session: "sess-A", title: "TITRE-PERIME", summary: "RESUME-PERIME", stale: true,
		start: "2026-10-20T11:00:00.000Z", subjects: []string{w.a}})
	f.episode(episodeRow{session: "sess-B", summary: "Résumé orphelin"})
	for _, path := range []string{"/api/sessions", "/api/sessions/sess-A", "/api/sessions/sess-B", "/api/points", "/api/memo?subject=" + w.a} {
		body := string(f.get(path).body)
		if strings.Contains(body, "PERIME") || strings.Contains(body, "orphelin") {
			t.Fatal(path, body)
		}
	}
	if row := f.sessionRow("sess-A"); row["episode_title"] != "Ancien" || row["summary"] != "Ancien résumé" {
		t.Fatal(row)
	}
}

func TestMenuCounts(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	f.say("sess-A2", ts, "autre")
	if a := f.get("/api/counts"); string(a.body) != `{"sessions":3,"points":3,"subjects":2}` {
		t.Fatalf("%s", a.body)
	}
	f.post("/api/sessions/forget", obj{"id": "sess-A2"})
	f.post("/api/points/forget", obj{"id": w.fact})
	if a := f.get("/api/counts"); string(a.body) != `{"sessions":2,"points":2,"subjects":2}` {
		t.Fatalf("%s", a.body)
	}
}

func TestForgetSessionWaitsForTheIndexLock(t *testing.T) {
	f := newFixture(t)
	f.memories()
	release, _, err := lock.Writer(f.p.IndexLock(), true)
	if err != nil {
		t.Fatal(err)
	}
	a := f.post("/api/sessions/forget", obj{"id": "sess-A"})
	release()
	f.status(t, a, 503)
	if string(a.body) != `{"error":"la mémoire est occupée, réessaie dans un instant"}` {
		t.Fatalf("%s", a.body)
	}
	if len(f.sessions("")) != 2 {
		t.Fatal()
	}
	release, _, err = lock.Writer(f.p.IndexLock(), true)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()
	f.status(t, f.post("/api/sessions/forget", obj{"id": "sess-A"}), 200)
}

func TestNoRouteStartsASummary(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	f.get("/api/refresh")
	f.get("/api/points")
	f.get("/api/memo?subject=" + w.a)
	f.get("/api/sessions")
	f.get("/api/subjects")
	f.post("/api/points/correct", obj{"id": w.open, "importance": 2})
	f.post("/api/points/forget", obj{"id": w.fact})
	f.post("/api/subjects/merge", obj{"from": w.b, "into": w.a})
	f.post("/api/sessions/forget", obj{"id": "sess-B"})
	if _, err := os.Stat(claudeMarker); err == nil {
		t.Fatal("claude a été appelé")
	}
}

func TestMemo(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	var got struct {
		Text string `json:"text"`
	}
	f.get("/api/memo?subject="+w.a).json(t, &got)
	want, err := memo.Index(f.conn, w.a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != want || !strings.Contains(want, "Une facture est unique") {
		t.Fatalf("%q\n%q", got.Text, want)
	}
	f.get("/api/memo").json(t, &got)
	if !strings.Contains(got.Text, "Dernières sessions :") {
		t.Fatal(got.Text)
	}
	repo, err := subjects.EnsureRepo(f.conn, "example/widget", "widget")
	if err != nil {
		t.Fatal(err)
	}
	f.point(w.ea, repo, "decision", "Utiliser sqlite", 3)
	f.exec("INSERT INTO repo_cache VALUES (?, ?, 't')", f.s.cwd, repo)
	f.get("/api/memo").json(t, &got)
	if !strings.Contains(got.Text, "Sujet « widget »") || !strings.Contains(got.Text, "Utiliser sqlite") {
		t.Fatal(got.Text)
	}
}

func TestViewingTheMemoMarksNothingUsed(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	repo, err := subjects.EnsureRepo(f.conn, "example/widget", "widget")
	if err != nil {
		t.Fatal(err)
	}
	f.point(w.ea, repo, "decision", "Utiliser sqlite", 3)
	f.exec("INSERT INTO repo_cache VALUES (?, ?, 't')", f.s.cwd, repo)
	if !strings.Contains(string(f.get("/api/memo").body), "Utiliser sqlite") {
		t.Fatal()
	}
	f.status(t, f.get("/api/memo?subject="+w.a), 200)
	if n := f.count("SELECT (SELECT count(*) FROM point_usage) + (SELECT count(*) FROM injected)"); n != 0 {
		t.Fatal(n)
	}
}

func TestEmptyMemo(t *testing.T) {
	f := newFixture(t)
	if a := f.get("/api/memo"); string(a.body) != `{"text":""}` {
		t.Fatalf("%s", a.body)
	}
}

type node struct {
	UID       string   `json:"uid"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Aliases   []string `json:"aliases"`
	ParentUID *string  `json:"parent_uid"`
	Synthesis string   `json:"synthesis"`
	Points    int      `json:"points"`
	Links     []struct {
		Type       string `json:"type"`
		Target     string `json:"target"`
		TargetName string `json:"target_name"`
	} `json:"links"`
	Children []node `json:"children"`
}

func (f *fixture) tree() []node {
	f.t.Helper()
	var nodes []node
	a := f.get("/api/subjects")
	f.status(f.t, a, 200)
	a.json(f.t, &nodes)
	return nodes
}

func TestSubjectsTree(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	house := f.subject("Maison", "domicile")
	f.exec("UPDATE subjects SET parent_uid = ?, parent_origin = 'model' WHERE uid = ?", house, w.a)
	if err := subjects.Link(f.conn, w.a, "depends_on", w.b, subjects.Origin{}); err != nil {
		t.Fatal(err)
	}
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, created_at, updated_at) VALUES (?, 'y1', 'Tout sur Alpha.', ?, ?)", w.a, ts, ts)
	f.exec("INSERT INTO syntheses(subject_uid, uid, text, stale, created_at, updated_at) VALUES (?, 'y2', 'SYNTHESE-PERIMEE', 1, ?, ?)", w.b, ts, ts)
	raw := string(f.get("/api/subjects").body)
	if strings.Contains(raw, "PERIMEE") {
		t.Fatal(raw)
	}
	tree := f.tree()
	if len(tree) != 2 || tree[0].Name != "Beta" || tree[1].Name != "Maison" {
		t.Fatal(raw)
	}
	alpha := tree[1].Children[0]
	if alpha.UID != w.a || alpha.Synthesis != "Tout sur Alpha." || alpha.Points != 2 || *alpha.ParentUID != house {
		t.Fatal(raw)
	}
	if len(alpha.Links) != 1 || alpha.Links[0].Type != "depends_on" || alpha.Links[0].Target != w.b || alpha.Links[0].TargetName != "Beta" {
		t.Fatal(raw)
	}
	if fmt.Sprint(tree[1].Aliases) != "[domicile]" || tree[0].Kind != "subject" || tree[0].Links == nil || tree[0].Children == nil {
		t.Fatal(raw)
	}
}

func TestRenameSubject(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	f.status(t, f.post("/api/subjects/rename", obj{"uid": w.a, "name": "  Gamma\u200b rayon "}), 200)
	found, err := subjects.FindByTerm(f.conn, "gamma rayon")
	if err != nil || len(found) != 1 || found[0].UID != w.a || found[0].Name != "Gamma rayon" {
		t.Fatal(found, err)
	}
	f.status(t, f.post("/api/subjects/rename", obj{"uid": w.a, "name": " \u200b "}), 400)
	f.status(t, f.post("/api/subjects/rename", obj{"uid": w.a}), 400)
	f.status(t, f.post("/api/subjects/rename", obj{"uid": "inconnu", "name": "X"}), 404)
	a := f.post("/api/subjects/rename", obj{"uid": w.a, "name": "BÉTA"})
	f.status(t, a, 409)
	if string(a.body) != `{"error":"un autre sujet s'appelle déjà « Beta » : pour les réunir, utilise « Fusionner »"}` {
		t.Fatalf("%s", a.body)
	}
	if s, _ := subjects.Get(f.conn, w.a); s.Name != "Gamma rayon" {
		t.Fatal(s.Name)
	}
	f.status(t, f.post("/api/subjects/rename", obj{"uid": w.a, "name": "GAMMA rayon"}), 200)
}

func TestMergeSubjects(t *testing.T) {
	f := newFixture(t)
	w := f.memories()
	f.status(t, f.post("/api/subjects/merge", obj{"from": w.b, "into": w.a}), 200)
	if _, err := subjects.Get(f.conn, w.b); err != subjects.ErrNotFound {
		t.Fatal(err)
	}
	if got := field[string](f.points("subject="+w.a), "text"); len(got) != 3 {
		t.Fatal(got)
	}
	f.status(t, f.post("/api/subjects/merge", obj{"from": w.a, "into": w.a}), 400)
	f.status(t, f.post("/api/subjects/merge", obj{"from": w.b, "into": w.a}), 404)
	f.status(t, f.post("/api/subjects/merge", obj{"from": w.a}), 400)
}

func TestMoveSubjectIsAUserEdit(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	f.status(t, f.post("/api/subjects/parent", obj{"uid": w.a, "parent": w.b}), 200)
	if f.count("SELECT count(*) FROM subjects WHERE uid = ? AND parent_uid = ? AND parent_origin = 'user'", w.a, w.b) != 1 {
		t.Fatal()
	}
	f.status(t, f.post("/api/subjects/parent", obj{"uid": w.b, "parent": w.a}), 400)
	f.status(t, f.post("/api/subjects/parent", obj{"uid": w.a, "parent": "inconnu"}), 404)
	f.status(t, f.post("/api/subjects/parent", obj{"uid": w.a}), 400)
	f.status(t, f.post("/api/subjects/parent", obj{"uid": w.a, "parent": ""}), 200)
	if f.count("SELECT count(*) FROM subjects WHERE uid = ? AND parent_uid IS NULL AND parent_origin = 'user'", w.a) != 1 {
		t.Fatal()
	}
	if err := subjects.SetParent(f.conn, w.a, w.b, subjects.Origin{}); err != subjects.ErrUserSet {
		t.Fatal(err)
	}
}

func TestLinkAndUnlinkAreUserEdits(t *testing.T) {
	f := newFixture(t)
	w := f.indexed()
	f.status(t, f.post("/api/subjects/link", obj{"source": w.a, "type": "about", "target": w.b}), 200)
	if f.count("SELECT count(*) FROM links WHERE source_uid = ? AND type = 'about' AND target_uid = ? AND active = 1 AND origin = 'user'", w.a, w.b) != 1 {
		t.Fatal()
	}
	f.status(t, f.post("/api/subjects/unlink", obj{"source": w.a, "type": "about", "target": w.b}), 200)
	if f.count("SELECT count(*) FROM links WHERE active = 0 AND origin = 'user'") != 1 {
		t.Fatal()
	}
	if err := subjects.Link(f.conn, w.a, "about", w.b, subjects.Origin{}); err != subjects.ErrUserSet {
		t.Fatal(err)
	}
	f.status(t, f.post("/api/subjects/link", obj{"source": w.a, "type": "part_of", "target": w.b}), 400)
	f.status(t, f.post("/api/subjects/link", obj{"source": w.a, "type": "about", "target": w.a}), 400)
	f.status(t, f.post("/api/subjects/link", obj{"source": w.a, "type": "about", "target": "inconnu"}), 404)
	f.status(t, f.post("/api/subjects/unlink", obj{"source": w.a, "target": w.b}), 400)
}

func TestServePrintsTheURLAndOpensTheBrowser(t *testing.T) {
	dir := t.TempDir()
	p := paths.Paths{Home: filepath.Join(dir, "home"), Transcripts: dir}
	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(saved)
	for _, open := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		out := &lockedBuffer{}
		var opened []string
		done := make(chan error)
		go func() {
			done <- serve(ctx, p, dir, open, out, func(url string) error {
				opened = append(opened, url)
				return nil
			})
		}()
		var url string
		for deadline := time.Now().Add(5 * time.Second); url == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			url = strings.TrimSpace(out.String())
		}
		m := regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/\?token=([A-Za-z0-9_-]{43})$`).FindStringSubmatch(url)
		if m == nil {
			t.Fatalf("%q", url)
		}
		res, err := http.Get(url)
		if err != nil || res.StatusCode != 200 {
			t.Fatal(res, err)
		}
		res.Body.Close()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if open != (len(opened) == 1 && opened[0] == url) {
			t.Fatal(open, opened)
		}
		if _, err := http.Get(url); err == nil {
			t.Fatal("le serveur répond encore")
		}
		if strings.Contains(logged.String(), m[1]) {
			t.Fatal(logged.String())
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
