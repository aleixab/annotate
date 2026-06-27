// Package gitutil shells out to the git binary for the small set of plumbing
// operations the annotation engine needs: locating a repo root, hashing a
// working-tree file, reading HEAD, and diffing a baseline against the working
// tree. Repo identity is always resolved from the *file's* directory so the
// same relative path in different repos never collides.
package gitutil

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotInRepo is returned when a path is not inside any git repository.
var ErrNotInRepo = errors.New("not in a git repository")

// run executes git with -C dir and the given args, returning trimmed stdout.
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// RepoRoot resolves the absolute repository root that contains file, by asking
// git from the file's own directory. This works regardless of the editor's cwd
// and handles worktrees, submodules and .git-file links correctly.
func RepoRoot(file string) (string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	root, err := run(filepath.Dir(abs), "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", ErrNotInRepo
	}
	return root, nil
}

// HashObject returns the git blob SHA of the working-tree file as it currently
// is on disk (including uncommitted edits). It does not write the object.
func HashObject(repoRoot, file string) (string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	return run(repoRoot, "hash-object", "--", abs)
}

// HeadSHA returns the current HEAD commit SHA, or "" if the repo has no commits.
func HeadSHA(repoRoot string) string {
	sha, err := run(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return sha
}

// Diff returns the unified diff of the committed version of relPath at commit
// against the current working-tree file. Empty output means no differences.
func Diff(repoRoot, commit, relPath string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "diff", "--no-color", "--unified=3", commit, "--", relPath)
	out, err := cmd.Output()
	if err != nil {
		// git diff exits 0 with output; a non-zero exit here is a real error
		// (e.g. unknown commit). Surface empty so callers fall back to fuzzy.
		return "", err
	}
	return string(out), nil
}
