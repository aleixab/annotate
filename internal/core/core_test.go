package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleabmo/nvim-annotate/internal/gitutil"
	"github.com/aleabmo/nvim-annotate/internal/store"
)

func TestRemapLine_BlobUnchanged(t *testing.T) {
	n := store.Note{Line: 10, BlobSHA: "abc"}
	line, orphaned := RemapLine(n, nil, true, "")
	if orphaned || line != 10 {
		t.Fatalf("want 10,false got %d,%v", line, orphaned)
	}
}

func TestRemapLine_DiffWalk(t *testing.T) {
	file := []string{"a", "x", "y", "b", "c"}
	n := store.Note{
		Line:         2, // "b" was at line 2 in the baseline
		AnchorText:   "a\nb\nc",
		AnchorOffset: 1,
	}
	diffOut := "@@ -1,3 +1,5 @@\n a\n+x\n+y\n b\n c\n"
	line, orphaned := RemapLine(n, file, false, diffOut)
	if orphaned || line != 4 {
		t.Fatalf("want 4,false got %d,%v", line, orphaned)
	}
}

func TestRemapLine_FuzzyFallback(t *testing.T) {
	// No usable diff, but the context block still exists further down.
	file := []string{
		"// a brand new header",
		"// inserted up top",
		"func helper() int {",
		"\treturn 42",
		"}",
	}
	n := store.Note{
		Line:         1,
		AnchorText:   "func helper() int {\n\treturn 42\n}",
		AnchorOffset: 1,
	}
	line, orphaned := RemapLine(n, file, false, "")
	if orphaned {
		t.Fatalf("expected fuzzy match, got orphaned")
	}
	if line != 4 {
		t.Fatalf("want line 4 got %d", line)
	}
}

func TestRemapLine_Orphaned(t *testing.T) {
	file := []string{"completely", "unrelated", "file", "contents"}
	n := store.Note{
		Line:         2,
		AnchorText:   "func helper() int {\n\treturn 42\n}",
		AnchorOffset: 1,
	}
	line, orphaned := RemapLine(n, file, false, "")
	if !orphaned {
		t.Fatalf("expected orphaned")
	}
	if line != 2 { // falls back to stored line
		t.Fatalf("want fallback line 2 got %d", line)
	}
}

func TestRemapNote_SectionRangeShifts(t *testing.T) {
	// A section originally spanning lines 2-4, after two lines are inserted
	// above it, should shift to 4-6.
	file := []string{"a", "x", "y", "b", "c", "d", "e"}
	n := store.Note{
		Kind:            store.KindSection,
		Line:            2,
		EndLine:         4,
		AnchorText:      "a\nb\nc",
		AnchorOffset:    1, // "b" at line 2
		EndAnchorText:   "c\nd\ne",
		EndAnchorOffset: 1, // "d" at line 4
	}
	diffOut := "@@ -1,4 +1,6 @@\n a\n+x\n+y\n b\n c\n d\n"
	line, endLine, orphaned := remapNote(n, file, false, diffOut)
	if orphaned {
		t.Fatalf("unexpected orphan")
	}
	if line != 4 || endLine != 6 {
		t.Fatalf("want range 4-6 got %d-%d", line, endLine)
	}
}

func TestBuildAnchor(t *testing.T) {
	lines := []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7"}
	// Anchor at line 4 with ContextLines=3 -> lines 1..7, offset 3.
	text, off := buildAnchor(lines, 4)
	if off != 3 {
		t.Fatalf("want offset 3 got %d", off)
	}
	if strings.Split(text, "\n")[off] != "l4" {
		t.Fatalf("anchor offset does not point at l4: %q", text)
	}

	// Near the top: anchor at line 2 -> offset clamps to 1.
	_, off = buildAnchor(lines, 2)
	if off != 1 {
		t.Fatalf("want offset 1 got %d", off)
	}
}

// TestRepoRoot_Resolution exercises gitutil against a real temp repo and the
// full Add->List round trip against a temp database.
func TestRepoRoot_Resolution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "t@t")
	runGit(t, repo, "config", "user.name", "t")

	sub := filepath.Join(repo, "internal", "server")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "main.go")
	writeFile(t, file, "package server\n\nfunc Run() {}\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")

	// RepoRoot resolves from the file's directory, not any cwd.
	root, err := gitutil.RepoRoot(file)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(root); resolved != mustEval(t, repo) {
		t.Fatalf("repo root %q != %q", root, repo)
	}

	db := filepath.Join(t.TempDir(), "notes.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := Add(st, AddRequest{File: file, Line: 3, Body: "the Run func"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	resp, err := List(st, ListRequest{File: file})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Notes) != 1 {
		t.Fatalf("want 1 note got %d", len(resp.Notes))
	}
	// File unchanged since add -> fast path keeps the original line.
	if resp.Notes[0].Line != 3 || resp.Notes[0].Orphaned {
		t.Fatalf("unexpected note view: %+v", resp.Notes[0])
	}
}

// TestReanchor_RebaselinesAfterEdit verifies that after the file changes,
// re-anchoring re-captures the blob so the next List uses the fast path, and
// keeps pointing at the same code.
func TestReanchor_RebaselinesAfterEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "t@t")
	runGit(t, repo, "config", "user.name", "t")

	file := filepath.Join(repo, "main.go")
	writeFile(t, file, "package main\n\nfunc target() {}\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")

	db := filepath.Join(t.TempDir(), "notes.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := Add(st, AddRequest{File: file, Line: 3, Body: "the target"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Insert two lines above the anchored line; it now lives at line 5.
	writeFile(t, file, "package main\n\n// a\n// b\nfunc target() {}\n")

	resp, err := List(st, ListRequest{File: file})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.Notes[0].Line != 5 || resp.Notes[0].Orphaned {
		t.Fatalf("expected remap to line 5, got %+v", resp.Notes[0])
	}

	// Re-anchor at the current position, then confirm the stored line moved and
	// the blob now matches (fast path) by reading the row back.
	if err := Reanchor(st, ReanchorRequest{ID: id}); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	n, ok, err := st.GetByID(id)
	if err != nil || !ok {
		t.Fatalf("GetByID: %v ok=%v", err, ok)
	}
	if n.Line != 5 {
		t.Fatalf("re-anchored stored line = %d, want 5", n.Line)
	}
	if strings.Split(n.AnchorText, "\n")[n.AnchorOffset] != "func target() {}" {
		t.Fatalf("anchor offset no longer points at target: %q", n.AnchorText)
	}
}

// TestPrune_RemovesNotesForMissingFiles checks that prune drops notes whose
// file is gone but keeps notes whose file still exists.
func TestPrune_RemovesNotesForMissingFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "t@t")
	runGit(t, repo, "config", "user.name", "t")

	keep := filepath.Join(repo, "keep.go")
	gone := filepath.Join(repo, "gone.go")
	writeFile(t, keep, "package main\n")
	writeFile(t, gone, "package main\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")

	db := filepath.Join(t.TempDir(), "notes.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := Add(st, AddRequest{File: keep, Line: 1, Body: "keep me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(st, AddRequest{File: gone, Line: 1, Body: "drop me"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	// Dry run reports the orphaned-file note without deleting it.
	dry, err := Prune(st, PruneRequest{DryRun: true})
	if err != nil {
		t.Fatalf("Prune dry: %v", err)
	}
	if dry.Count != 1 {
		t.Fatalf("dry-run count = %d, want 1", dry.Count)
	}
	if all, _ := st.ListAll(); len(all) != 2 {
		t.Fatalf("dry-run must not delete; have %d notes", len(all))
	}

	got, err := Prune(st, PruneRequest{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got.Count != 1 {
		t.Fatalf("prune count = %d, want 1", got.Count)
	}
	all, _ := st.ListAll()
	if len(all) != 1 || all[0].NoteBody != "keep me" {
		t.Fatalf("after prune want only 'keep me', got %+v", all)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
