// Package store is the SQLite persistence layer for annotations. It uses the
// pure-Go modernc driver (no cgo) so the binary stays a single static file, and
// opens in WAL mode with a busy_timeout so multiple Neovim instances can write
// the shared DB concurrently without "database is locked" errors.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Status values for a note.
const (
	StatusActive   = "active"
	StatusOrphaned = "orphaned"
)

// Note is one annotation row.
// Kind values for an annotation.
const (
	KindNote    = "note"    // single line, inline virtual text
	KindSection = "section" // a tinted block spanning [Line, EndLine]
)

type Note struct {
	ID           int64
	RepoRoot     string
	FilePath     string // relative to RepoRoot
	CommitSHA    string // HEAD at creation (diff baseline)
	Line         int    // start line number at creation
	BlobSHA      string // git hash-object of the working file at creation
	AnchorText   string // context block around Line, \n-joined
	AnchorOffset int    // index of the anchored line within AnchorText

	// Range support (sections). For a plain note EndLine == Line and the End*
	// anchor fields are empty.
	Kind            string // KindNote | KindSection
	EndLine         int    // end line of the range
	EndAnchorText   string // context block around EndLine
	EndAnchorOffset int    // index of the end line within EndAnchorText

	NoteBody  string
	Status    string
	CreatedAt string
	UpdatedAt string
}

// Store wraps a connection to the annotations database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS notes (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_root     TEXT    NOT NULL,
	file_path     TEXT    NOT NULL,
	commit_sha    TEXT    NOT NULL DEFAULT '',
	line          INTEGER NOT NULL,
	blob_sha      TEXT    NOT NULL DEFAULT '',
	anchor_text   TEXT    NOT NULL DEFAULT '',
	anchor_offset INTEGER NOT NULL DEFAULT 0,
	kind          TEXT    NOT NULL DEFAULT 'note',
	end_line      INTEGER NOT NULL DEFAULT 0,
	end_anchor_text   TEXT    NOT NULL DEFAULT '',
	end_anchor_offset INTEGER NOT NULL DEFAULT 0,
	note_body     TEXT    NOT NULL DEFAULT '',
	status        TEXT    NOT NULL DEFAULT 'active',
	created_at    TEXT    NOT NULL,
	updated_at    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notes_lookup ON notes(repo_root, file_path, status);
`

// migrations add columns introduced after the initial schema, so existing
// databases pick them up without losing notes.
var migrations = []struct{ col, ddl string }{
	{"kind", "kind TEXT NOT NULL DEFAULT 'note'"},
	{"end_line", "end_line INTEGER NOT NULL DEFAULT 0"},
	{"end_anchor_text", "end_anchor_text TEXT NOT NULL DEFAULT ''"},
	{"end_anchor_offset", "end_anchor_offset INTEGER NOT NULL DEFAULT 0"},
}

// Open opens (creating if necessary) the database at path with WAL mode and a
// busy_timeout, and ensures the schema exists.
func Open(path string) (*Store, error) {
	// Order matters: busy_timeout must be set first so the busy handler is
	// active when the connection performs the initial WAL switch (which itself
	// briefly needs an exclusive lock under concurrency).
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A CLI invocation is single-threaded; one connection avoids driver-level
	// contention while WAL handles cross-process concurrency.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate adds any columns missing from an older database.
func (s *Store) migrate() error {
	rows, err := s.db.Query(`PRAGMA table_info(notes)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for _, m := range migrations {
		if !have[m.col] {
			if _, err := s.db.Exec(`ALTER TABLE notes ADD COLUMN ` + m.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// Insert stores a new note and returns its ID. CreatedAt/UpdatedAt/Status are
// set automatically.
func (s *Store) Insert(n Note) (int64, error) {
	ts := now()
	if n.Kind == "" {
		n.Kind = KindNote
	}
	res, err := s.db.Exec(`
		INSERT INTO notes
			(repo_root, file_path, commit_sha, line, blob_sha, anchor_text,
			 anchor_offset, kind, end_line, end_anchor_text, end_anchor_offset,
			 note_body, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.RepoRoot, n.FilePath, n.CommitSHA, n.Line, n.BlobSHA, n.AnchorText,
		n.AnchorOffset, n.Kind, n.EndLine, n.EndAnchorText, n.EndAnchorOffset,
		n.NoteBody, StatusActive, ts, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertWithMeta stores a note preserving its Status/CreatedAt/UpdatedAt (used
// by import/restore). Missing metadata is defaulted as in Insert.
func (s *Store) InsertWithMeta(n Note) (int64, error) {
	if n.Kind == "" {
		n.Kind = KindNote
	}
	if n.Status == "" {
		n.Status = StatusActive
	}
	if n.CreatedAt == "" {
		n.CreatedAt = now()
	}
	if n.UpdatedAt == "" {
		n.UpdatedAt = n.CreatedAt
	}
	res, err := s.db.Exec(`
		INSERT INTO notes
			(repo_root, file_path, commit_sha, line, blob_sha, anchor_text,
			 anchor_offset, kind, end_line, end_anchor_text, end_anchor_offset,
			 note_body, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.RepoRoot, n.FilePath, n.CommitSHA, n.Line, n.BlobSHA, n.AnchorText,
		n.AnchorOffset, n.Kind, n.EndLine, n.EndAnchorText, n.EndAnchorOffset,
		n.NoteBody, n.Status, n.CreatedAt, n.UpdatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanNotes(rows *sql.Rows) ([]Note, error) {
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.RepoRoot, &n.FilePath, &n.CommitSHA,
			&n.Line, &n.BlobSHA, &n.AnchorText, &n.AnchorOffset,
			&n.Kind, &n.EndLine, &n.EndAnchorText, &n.EndAnchorOffset,
			&n.NoteBody, &n.Status, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const selectCols = `id, repo_root, file_path, commit_sha, line, blob_sha,
	anchor_text, anchor_offset, kind, end_line, end_anchor_text, end_anchor_offset,
	note_body, status, created_at, updated_at`

// ListByFile returns all notes (active and orphaned) for a file within a repo,
// ordered by their stored line.
func (s *Store) ListByFile(repoRoot, filePath string) ([]Note, error) {
	rows, err := s.db.Query(`SELECT `+selectCols+`
		FROM notes WHERE repo_root = ? AND file_path = ? ORDER BY line`,
		repoRoot, filePath)
	if err != nil {
		return nil, err
	}
	return scanNotes(rows)
}

// GetByID returns one note by ID. ok is false when no such note exists.
func (s *Store) GetByID(id int64) (Note, bool, error) {
	rows, err := s.db.Query(`SELECT `+selectCols+` FROM notes WHERE id = ?`, id)
	if err != nil {
		return Note{}, false, err
	}
	ns, err := scanNotes(rows)
	if err != nil {
		return Note{}, false, err
	}
	if len(ns) == 0 {
		return Note{}, false, nil
	}
	return ns[0], true, nil
}

// ListAll returns every note across all repos, for prune/export.
func (s *Store) ListAll() ([]Note, error) {
	rows, err := s.db.Query(`SELECT ` + selectCols + `
		FROM notes ORDER BY repo_root, file_path, line`)
	if err != nil {
		return nil, err
	}
	return scanNotes(rows)
}

// UpdateAnchors rewrites a note's anchor fields (position, baseline, context)
// and clears its orphan status — used by re-anchoring.
func (s *Store) UpdateAnchors(n Note) error {
	res, err := s.db.Exec(`UPDATE notes SET
		commit_sha = ?, line = ?, blob_sha = ?, anchor_text = ?, anchor_offset = ?,
		end_line = ?, end_anchor_text = ?, end_anchor_offset = ?,
		status = ?, updated_at = ? WHERE id = ?`,
		n.CommitSHA, n.Line, n.BlobSHA, n.AnchorText, n.AnchorOffset,
		n.EndLine, n.EndAnchorText, n.EndAnchorOffset, n.Status, now(), n.ID)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// UpdateBody changes a note's body and bumps updated_at.
func (s *Store) UpdateBody(id int64, body string) error {
	res, err := s.db.Exec(`UPDATE notes SET note_body = ?, updated_at = ? WHERE id = ?`,
		body, now(), id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// SetStatus persists a note's active/orphaned status (used after remapping).
func (s *Store) SetStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE notes SET status = ?, updated_at = ? WHERE id = ?`,
		status, now(), id)
	return err
}

// Delete removes a note by ID.
func (s *Store) Delete(id int64) error {
	res, err := s.db.Exec(`DELETE FROM notes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// Search returns notes across all repos whose body matches query (substring,
// case-insensitive). An empty query returns every note.
func (s *Store) Search(query string) ([]Note, error) {
	rows, err := s.db.Query(`SELECT `+selectCols+`
		FROM notes WHERE note_body LIKE '%' || ? || '%' COLLATE NOCASE
		ORDER BY updated_at DESC`, query)
	if err != nil {
		return nil, err
	}
	return scanNotes(rows)
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no note matched")
	}
	return nil
}
