// Package web serves the local page of paul: 127.0.0.1 only, one token per launch, a closed list of static files and a JSON API.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jplot/brain/internal/db"
	"github.com/jplot/brain/internal/paths"
)

const (
	maxBody     = 64 << 10
	busyTimeout = 5 * time.Second
	csp         = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'"
)

//go:embed static
var embedded embed.FS

var headers = map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer",
	"X-Content-Type-Options": "nosniff", "Content-Security-Policy": csp}

var pages = map[string]struct{ file, kind string }{
	"/":          {"index.html", "text/html; charset=utf-8"},
	"/app.js":    {"app.js", "text/javascript; charset=utf-8"},
	"/style.css": {"style.css", "text/css; charset=utf-8"},
}

type server struct {
	p      paths.Paths
	conn   *sql.DB
	ln     net.Listener
	token  string
	host   string
	cwd    string
	static fs.FS
}

type reply struct {
	status  int
	message string
}

func (r *reply) Error() string { return r.message }

func fail(status int, message string) error { return &reply{status, message} }

// The address carries the token: it goes to out, never to paul.log.
func Serve(p paths.Paths, open bool, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cwd, _ := os.Getwd()
	return serve(ctx, p, cwd, open, out, browse)
}

func serve(ctx context.Context, p paths.Paths, cwd string, open bool, out io.Writer, browser func(string) error) error {
	s, err := listen(p, cwd)
	if err != nil {
		return err
	}
	address := "http://" + s.host + "/?token=" + s.token
	fmt.Fprintln(out, address)
	if open {
		if err := browser(address); err != nil {
			log.Printf("web : navigateur non ouvert (%T)", err)
		}
	}
	h := &http.Server{Handler: s, ErrorLog: log.New(io.Discard, "", 0)}
	go func() {
		<-ctx.Done()
		h.Close()
	}()
	err = h.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, s.conn.Close())
}

func browse(address string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, address)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func listen(p paths.Paths, cwd string) (*server, error) {
	conn, err := db.Open(p, busyTimeout)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		conn.Close()
		return nil, err
	}
	var secret [32]byte
	rand.Read(secret[:])
	static, _ := fs.Sub(embedded, "static")
	return &server{p: p, conn: conn, ln: ln, token: base64.RawURLEncoding.EncodeToString(secret[:]),
		host: ln.Addr().String(), cwd: cwd, static: static}, nil
}

// A site that rebinds its own name to 127.0.0.1 (DNS rebinding) still sends that name as Host;
// an absolute-form request line puts its own host in r.Host and hides the Host header.
func (s *server) trusted(r *http.Request, query url.Values) bool {
	if r.Host != s.host || r.URL.Host != "" {
		return false
	}
	given := query.Get("token")
	if r.Method == http.MethodPost {
		given = r.Header.Get("X-Paul-Token")
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) == 1
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for name, value := range headers {
		w.Header().Set(name, value)
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || !s.trusted(r, query) {
		send(w, http.StatusForbidden, map[string]string{"error": "refusé"})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		send(w, http.StatusNotImplemented, map[string]string{"error": "requête refusée"})
		return
	}
	if page, ok := pages[r.URL.Path]; ok && r.Method == http.MethodGet {
		s.page(w, r.URL.Path, page.file, page.kind)
		return
	}
	name, handler, req := s.route(r.Method, r.URL.Path, query)
	if handler == nil {
		send(w, http.StatusNotFound, map[string]string{"error": "introuvable"})
		return
	}
	if r.Method == http.MethodPost {
		req.body, err = readBody(r)
	}
	var out any
	if err == nil {
		out, err = handler(s, req)
	}
	if err != nil {
		status, message := classify(err)
		if status == http.StatusInternalServerError {
			log.Printf("web %s : %T", name, err)
		}
		send(w, status, map[string]string{"error": message})
		return
	}
	send(w, http.StatusOK, out)
}

func send(w http.ResponseWriter, status int, payload any) {
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func (s *server) page(w http.ResponseWriter, path, file, kind string) {
	body, err := fs.ReadFile(s.static, file)
	if err != nil {
		send(w, http.StatusNotFound, map[string]string{"error": "introuvable"})
		return
	}
	if path == "/" {
		body = []byte(strings.ReplaceAll(string(body), "__TOKEN__", s.token))
	}
	w.Header().Set("Content-Type", kind)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func readBody(r *http.Request) (map[string]json.RawMessage, error) {
	if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
		return nil, fail(http.StatusUnsupportedMediaType, "JSON attendu")
	}
	if r.ContentLength < 0 {
		return nil, fail(http.StatusBadRequest, "longueur invalide")
	}
	if r.ContentLength > maxBody {
		return nil, fail(http.StatusRequestEntityTooLarge, "corps trop gros")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || !json.Valid(raw) {
		return nil, fail(http.StatusBadRequest, "JSON invalide")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, fail(http.StatusBadRequest, "objet JSON attendu")
	}
	return body, nil
}
