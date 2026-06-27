// Package core orchestrates the engine: it implements the JSON command handlers
// (add/list/edit/delete/search) and the remap-on-load algorithm that gives each
// note its current line in the working buffer.
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aleabmo/nvim-annotate/internal/diff"
	"github.com/aleabmo/nvim-annotate/internal/fuzzy"
	"github.com/aleabmo/nvim-annotate/internal/gitutil"
	"github.com/aleabmo/nvim-annotate/internal/store"
)

// Tunables for anchoring and matching.
const (
	// ContextLines is N: how many lines above and below the anchor are stored
	// in anchor_text for the fuzzy fallback.
	ContextLines = 3
	// FuzzyThreshold is the minimum average per-line similarity for the fuzzy
	// matcher to accept a position.
	FuzzyThreshold = 0.7
	// VerifyThreshold is how closely the diff-walked line must still resemble
	// the original anchored line before we trust it (otherwise fall to fuzzy).
	VerifyThreshold = 0.6
)

// ---- JSON contract types ----

type AddRequest struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"` // 0 => single line
	Kind    string `json:"kind"`     // "note" (default) | "section"
	Body    string `json:"body"`
}
type AddResponse struct {
	ID int64 `json:"id"`
}

type ListRequest struct {
	File string `json:"file"`
}
type NoteView struct {
	ID        int64  `json:"id"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	Orphaned  bool   `json:"orphaned"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
type ListResponse struct {
	Notes []NoteView `json:"notes"`
}

type EditRequest struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}
type DeleteRequest struct {
	ID int64 `json:"id"`
}

type SearchRequest struct {
	Query string `json:"query"`
}
type SearchResult struct {
	ID        int64  `json:"id"`
	RepoRoot  string `json:"repo_root"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Kind      string `json:"kind"`
	Body      string `json:"body"`
	Orphaned  bool   `json:"orphaned"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

type OKResponse struct {
	OK bool `json:"ok"`
}

// readLines reads a file into a slice of lines (without trailing newlines). A
// missing file yields an empty slice and no error so callers can still orphan.
func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	s := string(b)
	if s == "" {
		return nil, nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n"), nil
}

// Add resolves repo identity for the file, captures the worktree anchor, and
// inserts the note.
func Add(s *store.Store, req AddRequest) (int64, error) {
	abs, err := filepath.Abs(req.File)
	if err != nil {
		return 0, err
	}
	repoRoot, err := gitutil.RepoRoot(abs)
	if err != nil {
		return 0, err
	}
	relPath, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return 0, err
	}

	lines, err := readLines(abs)
	if err != nil {
		return 0, err
	}
	anchorText, anchorOffset := buildAnchor(lines, req.Line)

	kind := req.Kind
	if kind == "" {
		kind = store.KindNote
	}
	endLine := req.EndLine
	if endLine < req.Line {
		endLine = req.Line
	}
	// Only sections carry a distinct second anchor; plain notes leave it empty.
	var endAnchorText string
	var endAnchorOffset int
	if kind == store.KindSection && endLine != req.Line {
		endAnchorText, endAnchorOffset = buildAnchor(lines, endLine)
	}

	blobSHA, _ := gitutil.HashObject(repoRoot, abs)
	commitSHA := gitutil.HeadSHA(repoRoot)

	return s.Insert(store.Note{
		RepoRoot:        repoRoot,
		FilePath:        relPath,
		CommitSHA:       commitSHA,
		Line:            req.Line,
		BlobSHA:         blobSHA,
		AnchorText:      anchorText,
		AnchorOffset:    anchorOffset,
		Kind:            kind,
		EndLine:         endLine,
		EndAnchorText:   endAnchorText,
		EndAnchorOffset: endAnchorOffset,
		NoteBody:        req.Body,
	})
}

// buildAnchor extracts the context block around a 1-based line and the offset of
// the anchored line within that block.
func buildAnchor(lines []string, line int) (text string, offset int) {
	if len(lines) == 0 {
		return "", 0
	}
	idx := line - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(lines) {
		idx = len(lines) - 1
	}
	start := idx - ContextLines
	if start < 0 {
		start = 0
	}
	end := idx + ContextLines + 1
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), idx - start
}

// List returns every note for a file with its remapped current line, persisting
// any active/orphaned status change discovered during remapping.
func List(s *store.Store, req ListRequest) (ListResponse, error) {
	abs, err := filepath.Abs(req.File)
	if err != nil {
		return ListResponse{}, err
	}
	repoRoot, err := gitutil.RepoRoot(abs)
	if err != nil {
		// Not in a repo: nothing to show, but not an error condition.
		return ListResponse{Notes: []NoteView{}}, nil
	}
	relPath, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return ListResponse{}, err
	}

	notes, err := s.ListByFile(repoRoot, relPath)
	if err != nil {
		return ListResponse{}, err
	}

	lines, err := readLines(abs)
	if err != nil {
		return ListResponse{}, err
	}
	currentBlob, _ := gitutil.HashObject(repoRoot, abs)

	views := make([]NoteView, 0, len(notes))
	for _, n := range notes {
		blobMatches := currentBlob != "" && currentBlob == n.BlobSHA
		diffOut := ""
		if !blobMatches && n.CommitSHA != "" {
			if d, derr := gitutil.Diff(repoRoot, n.CommitSHA, relPath); derr == nil {
				diffOut = d
			}
		}
		line, endLine, orphaned := remapNote(n, lines, blobMatches, diffOut)

		newStatus := store.StatusActive
		if orphaned {
			newStatus = store.StatusOrphaned
		}
		if newStatus != n.Status {
			_ = s.SetStatus(n.ID, newStatus)
		}

		kind := n.Kind
		if kind == "" {
			kind = store.KindNote
		}
		views = append(views, NoteView{
			ID:        n.ID,
			Line:      line,
			EndLine:   endLine,
			Kind:      kind,
			Body:      n.NoteBody,
			Orphaned:  orphaned,
			CreatedAt: n.CreatedAt,
			UpdatedAt: n.UpdatedAt,
		})
	}
	return ListResponse{Notes: views}, nil
}

// remapNote remaps a note's start line, and (for sections) its end line, to the
// current buffer. The annotation is orphaned only if the start anchor cannot be
// located; if just the end anchor is lost, the range degrades to its original
// height from the remapped start.
func remapNote(n store.Note, fileLines []string, blobMatches bool, diffOut string) (line, endLine int, orphaned bool) {
	line, orphaned = remapPos(n.Line, n.AnchorText, n.AnchorOffset, fileLines, blobMatches, diffOut)
	endLine = line

	if n.Kind == store.KindSection && n.EndLine > n.Line {
		el, eOrphan := remapPos(n.EndLine, n.EndAnchorText, n.EndAnchorOffset, fileLines, blobMatches, diffOut)
		if eOrphan {
			el = line + (n.EndLine - n.Line) // keep original span height
		}
		if el < line {
			el = line
		}
		if len(fileLines) > 0 && el > len(fileLines) {
			el = len(fileLines)
		}
		endLine = el
	}
	return line, endLine, orphaned
}

// RemapLine remaps a single anchored line using the cheapest-path-first
// algorithm. It is pure (no git/IO) so it can be unit-tested directly.
func RemapLine(n store.Note, fileLines []string, blobMatches bool, diffOut string) (line int, orphaned bool) {
	return remapPos(n.Line, n.AnchorText, n.AnchorOffset, fileLines, blobMatches, diffOut)
}

// remapPos relocates one line given its own anchor:
//
//  1. blob unchanged  -> original line.
//  2. diff-walk the baseline->working diff, verified against the anchor line.
//  3. fuzzy-match the stored context block.
//  4. give up        -> orphaned (line falls back to the stored line).
func remapPos(origLine int, anchorText string, anchorOffset int, fileLines []string, blobMatches bool, diffOut string) (line int, orphaned bool) {
	if blobMatches {
		return origLine, false
	}

	if diffOut != "" {
		hunks := diff.Parse(diffOut)
		newLine, deleted := diff.MapLine(hunks, origLine)
		if !deleted && newLine >= 1 && newLine <= len(fileLines) && verifyAnchor(fileLines, newLine, anchorText, anchorOffset) {
			return newLine, false
		}
	}

	block := strings.Split(anchorText, "\n")
	if l, ok := fuzzy.BestMatch(fileLines, block, anchorOffset, FuzzyThreshold); ok {
		return l, false
	}

	return origLine, true
}

// verifyAnchor checks that a diff-walked line still resembles the originally
// anchored line, guarding against the case where uncommitted edits existed at
// creation time and threw off the baseline diff.
func verifyAnchor(fileLines []string, line int, anchorText string, anchorOffset int) bool {
	block := strings.Split(anchorText, "\n")
	if anchorOffset < 0 || anchorOffset >= len(block) {
		return true
	}
	want := strings.TrimSpace(block[anchorOffset])
	if want == "" {
		return true
	}
	got := strings.TrimSpace(fileLines[line-1])
	return fuzzy.Ratio(want, got) >= VerifyThreshold
}

// Edit updates a note's body.
func Edit(s *store.Store, req EditRequest) error {
	return s.UpdateBody(req.ID, req.Body)
}

// Delete removes a note.
func Delete(s *store.Store, req DeleteRequest) error {
	return s.Delete(req.ID)
}

// Search returns matching notes across all repos. Each result's line is
// remapped against the working file when it still exists on disk, so the picker
// jumps to the right place.
func Search(s *store.Store, req SearchRequest) (SearchResponse, error) {
	notes, err := s.Search(req.Query)
	if err != nil {
		return SearchResponse{}, err
	}
	results := make([]SearchResult, 0, len(notes))
	for _, n := range notes {
		abs := filepath.Join(n.RepoRoot, n.FilePath)
		line, endLine := n.Line, n.EndLine
		orphaned := n.Status == store.StatusOrphaned
		if lines, lerr := readLines(abs); lerr == nil && len(lines) > 0 {
			blob, _ := gitutil.HashObject(n.RepoRoot, abs)
			blobMatches := blob != "" && blob == n.BlobSHA
			diffOut := ""
			if !blobMatches && n.CommitSHA != "" {
				if d, derr := gitutil.Diff(n.RepoRoot, n.CommitSHA, n.FilePath); derr == nil {
					diffOut = d
				}
			}
			line, endLine, orphaned = remapNote(n, lines, blobMatches, diffOut)
		}
		kind := n.Kind
		if kind == "" {
			kind = store.KindNote
		}
		results = append(results, SearchResult{
			ID:        n.ID,
			RepoRoot:  n.RepoRoot,
			FilePath:  n.FilePath,
			Line:      line,
			EndLine:   endLine,
			Kind:      kind,
			Body:      n.NoteBody,
			Orphaned:  orphaned,
			CreatedAt: n.CreatedAt,
			UpdatedAt: n.UpdatedAt,
		})
	}
	return SearchResponse{Results: results}, nil
}

// ---- reanchor ----

type ReanchorRequest struct {
	ID      int64 `json:"id"`
	Line    int   `json:"line"`     // optional override start (0 => current remapped position)
	EndLine int   `json:"end_line"` // optional override end (sections only)
}

// Reanchor re-captures a note's baseline (commit/blob) and context block against
// the file as it is right now, so the cheap blob/diff paths keep working after
// heavy churn. With no override it re-anchors at the note's current remapped
// position (and refuses if the note is orphaned — there is no trustworthy line
// to anchor to). An explicit Line lets the client re-anchor an orphan to where
// the user moved the cursor.
func Reanchor(s *store.Store, req ReanchorRequest) error {
	n, ok, err := s.GetByID(req.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no note with id %d", req.ID)
	}

	abs := filepath.Join(n.RepoRoot, n.FilePath)
	lines, err := readLines(abs)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return fmt.Errorf("file is empty or missing: %s", abs)
	}

	line, endLine := req.Line, req.EndLine
	if line <= 0 {
		currentBlob, _ := gitutil.HashObject(n.RepoRoot, abs)
		blobMatches := currentBlob != "" && currentBlob == n.BlobSHA
		diffOut := ""
		if !blobMatches && n.CommitSHA != "" {
			if d, derr := gitutil.Diff(n.RepoRoot, n.CommitSHA, n.FilePath); derr == nil {
				diffOut = d
			}
		}
		var orphaned bool
		line, endLine, orphaned = remapNote(n, lines, blobMatches, diffOut)
		if orphaned {
			return fmt.Errorf("note is orphaned; place the cursor on the correct line and re-anchor there")
		}
	}

	if line < 1 {
		line = 1
	}
	if line > len(lines) {
		line = len(lines)
	}

	n.CommitSHA = gitutil.HeadSHA(n.RepoRoot)
	n.BlobSHA, _ = gitutil.HashObject(n.RepoRoot, abs)
	n.Line = line
	n.AnchorText, n.AnchorOffset = buildAnchor(lines, line)

	if n.Kind == store.KindSection {
		if endLine < line {
			endLine = line + (n.EndLine - n.Line) // preserve span height
		}
		if endLine < line {
			endLine = line
		}
		if endLine > len(lines) {
			endLine = len(lines)
		}
		n.EndLine = endLine
		if endLine != line {
			n.EndAnchorText, n.EndAnchorOffset = buildAnchor(lines, endLine)
		} else {
			n.EndAnchorText, n.EndAnchorOffset = "", 0
		}
	} else {
		n.EndLine = line
		n.EndAnchorText, n.EndAnchorOffset = "", 0
	}

	n.Status = store.StatusActive
	return s.UpdateAnchors(n)
}

// ---- prune ----

type PruneRequest struct {
	DryRun bool `json:"dry_run"`
}
type PruneResponse struct {
	Pruned []SearchResult `json:"pruned"`
	Count  int            `json:"count"`
}

// Prune deletes notes whose underlying file no longer exists on disk. With
// DryRun it reports what would be removed without deleting.
func Prune(s *store.Store, req PruneRequest) (PruneResponse, error) {
	notes, err := s.ListAll()
	if err != nil {
		return PruneResponse{}, err
	}
	resp := PruneResponse{Pruned: []SearchResult{}}
	for _, n := range notes {
		abs := filepath.Join(n.RepoRoot, n.FilePath)
		if _, statErr := os.Stat(abs); statErr == nil || !os.IsNotExist(statErr) {
			continue // exists, or an ambiguous error: keep to be safe
		}
		kind := n.Kind
		if kind == "" {
			kind = store.KindNote
		}
		resp.Pruned = append(resp.Pruned, SearchResult{
			ID: n.ID, RepoRoot: n.RepoRoot, FilePath: n.FilePath,
			Line: n.Line, EndLine: n.EndLine, Kind: kind, Body: n.NoteBody,
			Orphaned:  n.Status == store.StatusOrphaned,
			CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
		})
		if !req.DryRun {
			if derr := s.Delete(n.ID); derr != nil {
				return resp, derr
			}
		}
	}
	resp.Count = len(resp.Pruned)
	return resp, nil
}

// ---- export / import ----

// ExportNote is the full, portable representation of a note (the DB is outside
// any repo and never committed, so this is how you back it up or move it).
type ExportNote struct {
	RepoRoot        string `json:"repo_root"`
	FilePath        string `json:"file_path"`
	CommitSHA       string `json:"commit_sha"`
	Line            int    `json:"line"`
	BlobSHA         string `json:"blob_sha"`
	AnchorText      string `json:"anchor_text"`
	AnchorOffset    int    `json:"anchor_offset"`
	Kind            string `json:"kind"`
	EndLine         int    `json:"end_line"`
	EndAnchorText   string `json:"end_anchor_text"`
	EndAnchorOffset int    `json:"end_anchor_offset"`
	NoteBody        string `json:"note_body"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}
type ExportResponse struct {
	Notes []ExportNote `json:"notes"`
}

func toExportNote(n store.Note) ExportNote {
	return ExportNote{
		RepoRoot: n.RepoRoot, FilePath: n.FilePath, CommitSHA: n.CommitSHA,
		Line: n.Line, BlobSHA: n.BlobSHA, AnchorText: n.AnchorText,
		AnchorOffset: n.AnchorOffset, Kind: n.Kind, EndLine: n.EndLine,
		EndAnchorText: n.EndAnchorText, EndAnchorOffset: n.EndAnchorOffset,
		NoteBody: n.NoteBody, Status: n.Status,
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
	}
}

// Export dumps every note as portable JSON.
func Export(s *store.Store) (ExportResponse, error) {
	notes, err := s.ListAll()
	if err != nil {
		return ExportResponse{}, err
	}
	out := ExportResponse{Notes: make([]ExportNote, 0, len(notes))}
	for _, n := range notes {
		out.Notes = append(out.Notes, toExportNote(n))
	}
	return out, nil
}

type ImportRequest struct {
	Notes []ExportNote `json:"notes"`
}
type ImportResponse struct {
	Imported int `json:"imported"`
}

// Import inserts notes from an export, preserving timestamps and status. Rows
// are added as new records (ids are reassigned); importing the same file twice
// duplicates, so it is meant for restoring into a fresh database.
func Import(s *store.Store, req ImportRequest) (ImportResponse, error) {
	count := 0
	for _, e := range req.Notes {
		_, err := s.InsertWithMeta(store.Note{
			RepoRoot: e.RepoRoot, FilePath: e.FilePath, CommitSHA: e.CommitSHA,
			Line: e.Line, BlobSHA: e.BlobSHA, AnchorText: e.AnchorText,
			AnchorOffset: e.AnchorOffset, Kind: e.Kind, EndLine: e.EndLine,
			EndAnchorText: e.EndAnchorText, EndAnchorOffset: e.EndAnchorOffset,
			NoteBody: e.NoteBody, Status: e.Status,
			CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
		})
		if err != nil {
			return ImportResponse{Imported: count}, err
		}
		count++
	}
	return ImportResponse{Imported: count}, nil
}

// ---- stats ----

type RepoStat struct {
	RepoRoot string `json:"repo_root"`
	Total    int    `json:"total"`
	Orphaned int    `json:"orphaned"`
}
type StatsResponse struct {
	Total    int        `json:"total"`
	Orphaned int        `json:"orphaned"`
	Notes    int        `json:"notes"`
	Sections int        `json:"sections"`
	Repos    int        `json:"repos"`
	Missing  int        `json:"missing"` // notes whose file no longer exists
	PerRepo  []RepoStat `json:"per_repo"`
	DBPath   string     `json:"db_path"`
	DBSize   int64      `json:"db_size"`
}

// Stats aggregates the whole store for the status dashboard. dbPath is supplied
// by the caller (the store does not know its own path) so its size can be shown.
func Stats(s *store.Store, dbPath string) (StatsResponse, error) {
	notes, err := s.ListAll()
	if err != nil {
		return StatsResponse{}, err
	}
	resp := StatsResponse{DBPath: dbPath, PerRepo: []RepoStat{}}
	idx := map[string]int{} // repo_root -> index into PerRepo
	for _, n := range notes {
		resp.Total++
		if n.Status == store.StatusOrphaned {
			resp.Orphaned++
		}
		if n.Kind == store.KindSection {
			resp.Sections++
		} else {
			resp.Notes++
		}
		if _, statErr := os.Stat(filepath.Join(n.RepoRoot, n.FilePath)); os.IsNotExist(statErr) {
			resp.Missing++
		}
		i, ok := idx[n.RepoRoot]
		if !ok {
			resp.PerRepo = append(resp.PerRepo, RepoStat{RepoRoot: n.RepoRoot})
			i = len(resp.PerRepo) - 1
			idx[n.RepoRoot] = i
		}
		resp.PerRepo[i].Total++
		if n.Status == store.StatusOrphaned {
			resp.PerRepo[i].Orphaned++
		}
	}
	resp.Repos = len(resp.PerRepo)
	sort.Slice(resp.PerRepo, func(a, b int) bool {
		if resp.PerRepo[a].Total != resp.PerRepo[b].Total {
			return resp.PerRepo[a].Total > resp.PerRepo[b].Total // busiest first
		}
		return resp.PerRepo[a].RepoRoot < resp.PerRepo[b].RepoRoot
	})
	if fi, statErr := os.Stat(dbPath); statErr == nil {
		resp.DBSize = fi.Size()
	}
	return resp, nil
}
