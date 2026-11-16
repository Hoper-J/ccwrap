package update

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// replaceExe puts tmp in place of exePath. Windows does not let the file of
// a running program be replaced (this ccwrap's, and that of any session or
// ccwrap ui running it) but does let it be renamed: the old file moves aside
// first, to a name RemoveReplaced deletes once nothing runs it.
func replaceExe(tmp, exePath string) error {
	old := fmt.Sprintf("%s.old-%d", exePath, time.Now().UnixNano())
	if err := os.Rename(exePath, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exePath); err != nil {
		_ = os.Rename(old, exePath)
		return err
	}
	_ = os.Remove(old) // fails while something still runs it
	return nil
}

// RemoveReplaced deletes the files that replaceExe and FreeNPMPackage moved
// aside for exePath and that nothing runs any more.
func RemoveReplaced(exePath string) {
	olds, _ := filepath.Glob(exePath + ".old-*")
	for _, p := range olds {
		_ = os.Remove(p)
	}
	if root := npmGlobalRoot(exePath); root != "" {
		aside := filepath.Join(root, asideDir)
		entries, _ := os.ReadDir(aside)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(aside, e.Name()))
		}
		_ = os.Remove(aside) // goes once it is empty
	}
}

// asideDir is where FreeNPMPackage puts programs that are still running, in
// the node_modules folder holding ccwrap's package: npm leaves folders
// there alone when their names start with a dot.
const asideDir = ".ccwrap-old"

// npmGlobalRoot is the node_modules folder holding the ccwrap-cli package
// that exePath is in, or "" when it is in none.
func npmGlobalRoot(exePath string) string {
	sep := string(filepath.Separator)
	root, _, ok := strings.Cut(exePath, sep+"node_modules"+sep+"ccwrap-cli"+sep)
	if !ok {
		return ""
	}
	return root + sep + "node_modules"
}

// FreeNPMPackage gets running programs out of ccwrap's npm package before
// npm replaces it. npm moves the package's folder aside, installs the new
// one and deletes the old. Windows does not let a running program's file be
// deleted, and the ccwrap.exe running the upgrade, like that of every
// session and ccwrap ui, is in that folder: npm then warns about what it
// could not delete, and its next upgrade fails while one of them still
// runs. A running program's file can be moved, though: it goes to a folder
// next to the package and a copy takes its place, so npm finds nothing in
// use. What an earlier upgrade had to leave goes the same way.
// RemoveReplaced deletes the moved files once nothing runs them. After an
// error the package is as it was.
func FreeNPMPackage(exePath string) error {
	root := npmGlobalRoot(exePath)
	if root == "" {
		return nil
	}
	aside := filepath.Join(root, asideDir)
	if err := os.MkdirAll(aside, 0o755); err != nil {
		return err
	}
	clearRetired(root, aside)
	copied := exePath + ".copy"
	if err := copyFile(exePath, copied); err != nil {
		_ = os.Remove(copied)
		return err
	}
	moved := asideName(aside, filepath.Base(exePath))
	if err := os.Rename(exePath, moved); err != nil {
		_ = os.Remove(copied)
		return err
	}
	if err := os.Rename(copied, exePath); err != nil {
		_ = os.Rename(moved, exePath)
		_ = os.Remove(copied)
		return err
	}
	return nil
}

// clearRetired removes the folders npm moved ccwrap's package to in earlier
// upgrades and could not delete (.ccwrap-cli-<hash>), moving the files
// still in use to aside: npm's next move into such a folder fails on them.
func clearRetired(root, aside string) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".ccwrap-cli-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if os.RemoveAll(dir) == nil {
			continue
		}
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && os.Remove(p) != nil {
				_ = os.Rename(p, asideName(aside, d.Name()))
			}
			return nil
		})
		_ = os.RemoveAll(dir)
	}
}

var asideSeq atomic.Uint64

// asideName is a new name in aside for a file called base.
func asideName(aside, base string) string {
	return filepath.Join(aside, fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), asideSeq.Add(1), base))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
