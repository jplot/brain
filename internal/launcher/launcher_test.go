package launcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	version  = "0.1.0"
	artifact = "paul-linux-arm64"
	fakeCurl = `#!/bin/sh
while [ $# -gt 1 ]; do
  case "$1" in -o) out=$2; shift ;; esac
  shift
done
echo "$1" >> "$FAKE_CURL_LOG"
[ -n "$FAKE_CURL_DELAY" ] && sleep "$FAKE_CURL_DELAY"
[ -n "$FAKE_CURL_FAIL" ] && exit 22
cp "$FAKE_SERVE/$(basename "$1")" "$out"
`
	fakeUname = `#!/bin/sh
case "$1" in -s) echo Linux ;; -m) echo aarch64 ;; esac
`
	fakePaul = "#!/bin/sh\necho \"fake paul $*\"\n"
)

type env struct {
	t     *testing.T
	root  string
	home  string
	serve string
	extra []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, home: filepath.Join(root, "home"), serve: filepath.Join(root, "serve")}
	launcher, err := os.ReadFile("../../bin/paul")
	if err != nil {
		t.Fatal(err)
	}
	e.write("plugin/bin/paul", string(launcher), 0o755)
	e.write("plugin/.claude-plugin/plugin.json", `{"name": "paul", "version": "`+version+`"}`, 0o644)
	e.write("fakebin/curl", fakeCurl, 0o755)
	e.write("fakebin/uname", fakeUname, 0o755)
	e.write("curl.log", "", 0o644)
	e.publish(fakePaul)
	return e
}

func (e *env) write(rel, content string, mode os.FileMode) string {
	e.t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) publish(binary string) {
	sum := sha256.Sum256([]byte(binary))
	e.write("serve/"+artifact, binary, 0o644)
	e.write("serve/checksums.txt",
		"0000000000000000000000000000000000000000000000000000000000000000  paul-darwin-arm64\n"+
			hex.EncodeToString(sum[:])+"  "+artifact+"\n", 0o644)
}

func (e *env) corruptChecksum() {
	e.write("serve/checksums.txt", strings.Repeat("0", 64)+"  "+artifact+"\n", 0o644)
}

func (e *env) installed() string { return filepath.Join(e.home, "bin", "paul-"+version) }

func (e *env) cmd(args ...string) *exec.Cmd {
	cmd := exec.Command("sh", append([]string{filepath.Join(e.root, "plugin/bin/paul")}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(e.root, "fakebin")+":"+os.Getenv("PATH"),
		"PAUL_HOME="+e.home,
		"FAKE_SERVE="+e.serve,
		"FAKE_CURL_LOG="+filepath.Join(e.root, "curl.log"),
	)
	cmd.Env = append(cmd.Env, e.extra...)
	return cmd
}

type result struct {
	stdout, stderr string
	code           int
	elapsed        time.Duration
}

func (e *env) run(args ...string) result {
	e.t.Helper()
	cmd := e.cmd(args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String(), elapsed: time.Since(start)}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) curlCalls(suffix string) int {
	data, err := os.ReadFile(filepath.Join(e.root, "curl.log"))
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasSuffix(line, suffix) && line != "" {
			n++
		}
	}
	return n
}

func (e *env) waitInstalled() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(e.installed()); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("binary never installed")
}

func (e *env) readLog() string {
	data, _ := os.ReadFile(filepath.Join(e.home, "paul.log"))
	return string(data)
}

func (e *env) assertNothingInstalled() {
	e.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(e.home, "bin"))
	for _, entry := range entries {
		e.t.Errorf("leftover in bin/: %s", entry.Name())
	}
}

func TestPresentBinaryIsExecuted(t *testing.T) {
	e := newEnv(t)
	e.write("home/bin/paul-"+version, fakePaul, 0o755)
	r := e.run("search", "mot")
	if r.code != 0 || r.stdout != "fake paul search mot\n" || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
	if n := e.curlCalls(""); n != 0 {
		t.Fatalf("curl called %d times", n)
	}
}

func TestAbsentBinaryInCommandModeDownloadsVerifiesAndRuns(t *testing.T) {
	e := newEnv(t)
	r := e.run("search", "mot")
	if r.code != 0 || r.stdout != "fake paul search mot\n" {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(r.stderr, "paul") || r.stderr == "" {
		t.Fatalf("no progress on stderr: %q", r.stderr)
	}
	info, err := os.Stat(e.installed())
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(e.home, "install.lock")); err == nil {
		t.Fatal("lock left behind")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.home, "bin")); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if !strings.Contains(strings.Join(readLines(t, filepath.Join(e.root, "curl.log")), "\n"),
		"/releases/download/v"+version+"/"+artifact) {
		t.Fatal("unexpected download url")
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestAbsentBinaryInHookModeReturnsAtOnceAndInstallsInBackground(t *testing.T) {
	e := newEnv(t)
	e.extra = []string{"FAKE_CURL_DELAY=1"}
	r := e.run("hook", "start")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
	if r.elapsed >= time.Second {
		t.Fatalf("hook took %v", r.elapsed)
	}
	if _, err := os.Stat(e.installed()); err == nil {
		t.Fatal("installed synchronously")
	}
	e.waitInstalled()
}

func TestBadChecksum(t *testing.T) {
	for _, mode := range []string{"command", "hook"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			e.corruptChecksum()
			args, want := []string{"search", "mot"}, 1
			if mode == "hook" {
				args, want = []string{"hook", "start"}, 0
			}
			r := e.run(args...)
			if r.code != want {
				t.Fatalf("exit %d, want %d (%+v)", r.code, want, r)
			}
			if mode == "hook" {
				if r.stdout != "" || r.stderr != "" {
					t.Fatalf("hook printed: %+v", r)
				}
				waitFor(t, func() bool { return strings.Contains(e.readLog(), "empreinte") })
				waitFor(t, func() bool { _, err := os.Stat(filepath.Join(e.home, "install.lock")); return err != nil })
			}
			if !strings.Contains(e.readLog(), "empreinte") {
				t.Fatalf("paul.log: %q", e.readLog())
			}
			e.assertNothingInstalled()
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition never met")
}

func TestCurlFailureThenNextLaunchRetries(t *testing.T) {
	for _, mode := range []string{"command", "hook"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			e.extra = []string{"FAKE_CURL_FAIL=1"}
			args, want := []string{"search", "mot"}, 1
			if mode == "hook" {
				args, want = []string{"hook", "start"}, 0
			}
			r := e.run(args...)
			if r.code != want {
				t.Fatalf("exit %d, want %d (%+v)", r.code, want, r)
			}
			if mode == "hook" && (r.stdout != "" || r.stderr != "") {
				t.Fatalf("hook printed: %+v", r)
			}
			waitFor(t, func() bool { return strings.Contains(e.readLog(), "téléchargement") })
			waitFor(t, func() bool { _, err := os.Stat(filepath.Join(e.home, "install.lock")); return err != nil })
			e.assertNothingInstalled()

			e.extra = nil
			if r := e.run("search", "mot"); r.code != 0 || r.stdout != "fake paul search mot\n" {
				t.Fatalf("retry: %+v", r)
			}
		})
	}
}

func TestConcurrentLaunchesDownloadOnce(t *testing.T) {
	e := newEnv(t)
	e.extra = []string{"FAKE_CURL_DELAY=0.5"}
	const n = 4
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.run("search", fmt.Sprint(i))
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.code != 0 || r.stdout != fmt.Sprintf("fake paul search %d\n", i) {
			t.Fatalf("launch %d: %+v", i, r)
		}
	}
	if c := e.curlCalls("/" + artifact); c != 1 {
		t.Fatalf("binary downloaded %d times", c)
	}
}

func TestStaleLockIsTakenOver(t *testing.T) {
	e := newEnv(t)
	lock := filepath.Join(e.home, "install.lock")
	if err := os.MkdirAll(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	r := e.run("search", "mot")
	if r.code != 0 || r.stdout != "fake paul search mot\n" {
		t.Fatalf("got %+v", r)
	}
}

func TestFreshLockInHookModeDoesNothing(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.home, "install.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := e.run("hook", "start")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
	time.Sleep(500 * time.Millisecond)
	if c := e.curlCalls(""); c != 0 {
		t.Fatalf("curl called %d times under a fresh lock", c)
	}
}
