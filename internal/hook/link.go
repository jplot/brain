package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jplot/brain/internal/paths"
)

// A regular file at the target is the user's own and is never replaced.
func LinkCommand(p paths.Paths, version string) error {
	if version == "" || version == "dev" {
		return nil
	}
	current := "paul-" + version
	target := filepath.Join(p.Bin(), current)
	if _, err := os.Stat(target); err != nil {
		return nil
	}
	link := filepath.Join(p.BinDir(), "paul")
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s existe et n'est pas un lien : laissé tel quel", link)
	}
	if err := os.MkdirAll(p.BinDir(), 0o755); err != nil {
		return err
	}
	if dest, err := os.Readlink(link); err != nil || dest != target {
		tmp := fmt.Sprintf("%s.%d.tmp", link, os.Getpid())
		os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, link); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	entries, err := os.ReadDir(p.Bin())
	if err != nil {
		return err
	}
	var failed error
	for _, e := range entries {
		if e.Type().IsRegular() && e.Name() != current && installed.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(p.Bin(), e.Name())); err != nil {
				failed = errors.Join(failed, err)
			}
		}
	}
	return failed
}

var installed = regexp.MustCompile(`^paul-\d+\.\d+\.\d+$`)
