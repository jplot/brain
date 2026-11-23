package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jplot/brain/internal/paths"
)

func linkFixture(t *testing.T) (paths.Paths, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PAUL_BIN_DIR", filepath.Join(dir, "bin"))
	p := paths.Paths{Home: filepath.Join(dir, "home")}
	if err := os.MkdirAll(p.Bin(), 0o700); err != nil {
		t.Fatal(err)
	}
	return p, filepath.Join(dir, "bin", "paul")
}

func installVersion(t *testing.T, p paths.Paths, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.Bin(), "paul-"+version), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestLinkCommandFollowsVersion(t *testing.T) {
	p, link := linkFixture(t)
	installVersion(t, p, "1.0.0")
	if err := LinkCommand(p, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if dest, err := os.Readlink(link); err != nil || dest != filepath.Join(p.Bin(), "paul-1.0.0") {
		t.Fatalf("%q %v", dest, err)
	}
	installVersion(t, p, "1.1.0")
	if err := LinkCommand(p, "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if dest, _ := os.Readlink(link); dest != filepath.Join(p.Bin(), "paul-1.1.0") {
		t.Fatalf("not followed: %q", dest)
	}
	if _, err := os.Stat(filepath.Join(p.Bin(), "paul-1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("old version kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Bin(), "paul-1.1.0")); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(link + ".*"); len(left) != 0 {
		t.Fatalf("temporary link left: %v", left)
	}
}

func TestLinkCommandKeepsRegularFile(t *testing.T) {
	p, link := linkFixture(t)
	installVersion(t, p, "1.0.0")
	installVersion(t, p, "0.9.0")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := LinkCommand(p, "1.0.0"); err == nil {
		t.Fatal("no error")
	}
	if raw, _ := os.ReadFile(link); string(raw) != "mine" {
		t.Fatalf("replaced: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(p.Bin(), "paul-0.9.0")); err != nil {
		t.Fatalf("old version removed: %v", err)
	}
}

func TestLinkCommandDoesNothingInDev(t *testing.T) {
	p, link := linkFixture(t)
	installVersion(t, p, "0.9.0")
	for _, v := range []string{"dev", ""} {
		if err := LinkCommand(p, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("linked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Bin(), "paul-0.9.0")); err != nil {
		t.Fatal(err)
	}
}

func TestLinkCommandIsIdempotent(t *testing.T) {
	p, link := linkFixture(t)
	installVersion(t, p, "1.0.0")
	for range 2 {
		if err := LinkCommand(p, "1.0.0"); err != nil {
			t.Fatal(err)
		}
	}
	if dest, _ := os.Readlink(link); dest != filepath.Join(p.Bin(), "paul-1.0.0") {
		t.Fatalf("%q", dest)
	}
}

func TestStartLinksTheCommandAndLogsARegularFile(t *testing.T) {
	f := newFixture(t)
	p, link := linkFixture(t)
	f.p.Home = p.Home
	old := Version
	Version = "2.0.0"
	t.Cleanup(func() { Version = old })
	installVersion(t, p, "2.0.0")
	f.run("start", "{}")
	if dest, _ := os.Readlink(link); dest != filepath.Join(p.Bin(), "paul-2.0.0") {
		t.Fatalf("%q", dest)
	}
	os.Remove(link)
	os.WriteFile(link, []byte("mine"), 0o755)
	f.run("start", "{}")
	if raw, _ := os.ReadFile(link); string(raw) != "mine" || !strings.Contains(f.logs.String(), "commande du terminal") {
		t.Fatalf("%q, log %q", raw, f.logs.String())
	}
}

func TestLinkCommandWaitsForTheBinary(t *testing.T) {
	p, link := linkFixture(t)
	installVersion(t, p, "0.9.0")
	if err := LinkCommand(p, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("dangling link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Bin(), "paul-0.9.0")); err != nil {
		t.Fatal(err)
	}
}

func TestLinkCommandCleanupOnlyRemovesInstalledVersions(t *testing.T) {
	p, _ := linkFixture(t)
	installVersion(t, p, "1.0.0")
	installVersion(t, p, "0.9.0")
	installVersion(t, p, "0.9.1.part")
	if err := os.Mkdir(filepath.Join(p.Bin(), "paul-0.8.0"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := LinkCommand(p, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{"paul-1.0.0": true, "paul-0.9.1.part": true, "paul-0.8.0": true, "paul-0.9.0": false} {
		if _, err := os.Lstat(filepath.Join(p.Bin(), name)); (err == nil) != kept {
			t.Fatalf("%s: kept %v, err %v", name, kept, err)
		}
	}
}

func TestLinkCommandCleanupContinuesAfterAFailure(t *testing.T) {
	p, _ := linkFixture(t)
	installVersion(t, p, "1.0.0")
	installVersion(t, p, "0.8.0")
	installVersion(t, p, "0.9.0")
	stuck := filepath.Join(p.Bin(), "paul-0.8.0")
	if err := exec.Command("chflags", "uchg", stuck).Run(); err != nil {
		t.Skip("no immutable flag on this system")
	}
	defer exec.Command("chflags", "nouchg", stuck).Run()
	if err := LinkCommand(p, "1.0.0"); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(p.Bin(), "paul-0.9.0")); !os.IsNotExist(err) {
		t.Fatalf("cleanup stopped at the first failure: %v", err)
	}
}
