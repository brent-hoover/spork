package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DirProbe answers from the filesystem.
type DirProbe struct{}

// Exists reports whether path holds a worktree.
//
// A worktree, not merely a directory: `git worktree add` leaves a .git entry
// behind, so an empty directory someone happened to create at the recorded
// path is correctly reported absent rather than adopted as a checkout that is
// not there.
//
// A linked worktree's .git is a file and a main checkout's is a directory;
// either means a checkout is here, so the kind is not examined.
func (DirProbe) Exists(path string) (bool, error) {
	_, err := os.Stat(filepath.Join(path, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	return true, nil
}
