package lock_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jplot/brain/internal/lock"
)

const (
	holderEnv = "PAUL_LOCK_HOLDER"
	holdEnv   = "PAUL_LOCK_HOLD"
	traceEnv  = "PAUL_LOCK_TRACE"
)

// TestHolderProcess is the child body: it only runs when re-executed.
func TestHolderProcess(t *testing.T) {
	path := os.Getenv(holderEnv)
	if path == "" {
		t.Skip("child process only")
	}
	hold, _ := time.ParseDuration(os.Getenv(holdEnv))
	release, acquired, err := lock.Writer(path, true)
	if err != nil || !acquired {
		os.Exit(2)
	}
	start := time.Now()
	os.Stdout.WriteString("locked\n")
	time.Sleep(hold)
	end := time.Now()
	if trace := os.Getenv(traceEnv); trace != "" {
		f, err := os.OpenFile(trace, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(3)
		}
		f.WriteString(strconv.FormatInt(start.UnixNano(), 10) + " " + strconv.FormatInt(end.UnixNano(), 10) + "\n")
		f.Close()
	}
	release()
}

func spawn(t *testing.T, path, hold, trace string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHolderProcess$")
	cmd.Env = append(os.Environ(), holderEnv+"="+path, holdEnv+"="+hold, traceEnv+"="+trace)
	return cmd
}

func startHolder(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := spawn(t, path, "60s", "")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case got := <-line:
		if got != "locked" {
			t.Fatalf("child announced %q", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("child did not announce the lock")
	}
	return cmd
}

func lockPath(t *testing.T) string { return filepath.Join(t.TempDir(), "x.lock") }

func TestNeverTwoWriters(t *testing.T) {
	path, trace := lockPath(t), filepath.Join(t.TempDir(), "trace")
	var cmds []*exec.Cmd
	for range 8 {
		cmd := spawn(t, path, "100ms", trace)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	type span struct{ start, end int64 }
	var spans []span
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(l)
		a, _ := strconv.ParseInt(f[0], 10, 64)
		b, _ := strconv.ParseInt(f[1], 10, 64)
		spans = append(spans, span{a, b})
	}
	if len(spans) != 8 {
		t.Fatalf("got %d spans", len(spans))
	}
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].start < spans[j].end && spans[j].start < spans[i].end {
				t.Fatalf("overlap: %v %v", spans[i], spans[j])
			}
		}
	}
}

func TestKilledProcessReleasesLock(t *testing.T) {
	path := lockPath(t)
	cmd := startHolder(t, path)
	_, acquired, err := lock.Writer(path, false)
	if err != nil || acquired {
		t.Fatalf("acquired=%v err=%v while held", acquired, err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	release, acquired, err := lock.Writer(path, false)
	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v after kill", acquired, err)
	}
	release()
}

func TestNonBlockingWhenBusy(t *testing.T) {
	path := lockPath(t)
	startHolder(t, path)
	release, acquired, err := lock.Writer(path, false)
	if err != nil || acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	release()
}

func TestReleasedByCallingRelease(t *testing.T) {
	path := lockPath(t)
	release, acquired, err := lock.Writer(path, true)
	if err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	release()
	release, acquired, err = lock.Writer(path, false)
	if err != nil || !acquired {
		t.Fatalf("not re-acquirable: acquired=%v err=%v", acquired, err)
	}
	release()
}

func TestFileMode600(t *testing.T) {
	path := lockPath(t)
	release, _, err := lock.Writer(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}
