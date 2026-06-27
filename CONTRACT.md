# JSON Contract: `nvim-annotate` engine ↔ `annotate.nvim` client

The Go binary and the Lua plugin communicate over a stable JSON-on-stdio
boundary. Each invocation is `nvim-annotate <command>`, reads **one** JSON object
on stdin, and writes **one** JSON object on stdout (newline-terminated).

This contract is the only coupling between the two halves; either can be
developed and tested in isolation against it.

## Conventions

- All line numbers are **1-based**.
- `file` is always an **absolute path** to the working file on disk. The engine
  resolves the repository root and the repo-relative path itself, by walking up
  from the file's directory — never from any editor cwd.
- On error the engine writes `{"error": "<message>"}` to stdout and exits
  non-zero. On success it exits zero.
- The database location is fixed by the engine (XDG data dir, or
  `$NVIM_ANNOTATE_DB_DIR` for tests); it is never passed over the wire.

## Commands

### `add`

Create an annotation anchored to the current worktree state.

Request:
```json
{
  "file": "/abs/path/to/file.go",
  "line": 42,
  "end_line": 0,
  "kind": "note",
  "body": "note text"
}
```
- `kind` is `"note"` (default) or `"section"`. A section is a tinted block
  spanning `line`..`end_line`; both ends are anchored independently.
- `end_line` is optional (default `0` → single line). Ignored unless
  `kind` is `"section"` and `end_line > line`.
- A `color:NAME` token inside `body` (e.g. `color:blue`) is interpreted by the
  client to tint a section; the engine treats it as opaque text.

Response:
```json
{ "id": 123 }
```

### `list`

Return every note for a file with its **remapped** current line and orphan
flag. The engine recomputes each line against the file as it is right now
(blob fast-path → diff walk → fuzzy fallback → orphan).

Request:
```json
{ "file": "/abs/path/to/file.go" }
```
Response:
```json
{
  "notes": [
    {
      "id": 123,
      "line": 44,
      "end_line": 46,
      "kind": "section",
      "body": "note text",
      "orphaned": false,
      "created_at": "2026-06-26T20:06:51Z",
      "updated_at": "2026-06-26T20:06:51Z"
    }
  ]
}
```
`line` (and, for sections, `end_line`) are positions in the current buffer. For
a plain note `end_line == line` and `kind == "note"`. When `orphaned` is `true`
the line could not be located and falls back to the stored line; the client
renders these distinctly and never drops them. A section is orphaned only if its
start anchor is lost — if just the end anchor moves out of reach the range keeps
its original height from the remapped start.

### `edit`

Update a note's body.

Request:
```json
{ "id": 123, "body": "new text" }
```
Response:
```json
{ "ok": true }
```

### `delete`

Delete a note by ID.

Request:
```json
{ "id": 123 }
```
Response:
```json
{ "ok": true }
```

### `search`

Substring search (case-insensitive) over all note bodies across **all** repos.
An empty `query` returns everything. Each result's `line` is remapped against
the working file when it still exists on disk.

Request:
```json
{ "query": "TODO" }
```
Response:
```json
{
  "results": [
    {
      "id": 123,
      "repo_root": "/home/me/src/project",
      "file_path": "internal/server/main.go",
      "line": 44,
      "end_line": 46,
      "kind": "section",
      "body": "note text",
      "orphaned": false,
      "created_at": "2026-06-26T20:06:51Z",
      "updated_at": "2026-06-26T20:06:51Z"
    }
  ]
}
```

### `reanchor`

Re-capture a note's baseline (`commit_sha`, `blob_sha`) and context block against
the file as it is **right now**, so the cheap blob/diff remap paths keep working
after heavy churn.

Request:
```json
{ "id": 123, "line": 0, "end_line": 0 }
```
- With `line` omitted/`0`, the note is re-anchored at its current remapped
  position. If it is **orphaned** (no trustworthy line) the engine returns an
  error instead of guessing.
- A positive `line` (and, for sections, `end_line`) overrides the target — this
  is how the client re-anchors an orphan to wherever the user moved the cursor.
  A section with no `end_line` override keeps its original span height.

Response:
```json
{ "ok": true }
```

### `prune`

Delete notes whose underlying file no longer exists on disk.

Request:
```json
{ "dry_run": false }
```
- With `dry_run: true` the matches are reported but nothing is deleted.

Response:
```json
{ "pruned": [ { "id": 123, "repo_root": "...", "file_path": "...", "line": 1,
               "end_line": 1, "kind": "note", "body": "...", "orphaned": false,
               "created_at": "...", "updated_at": "..." } ],
  "count": 1 }
```

### `export`

Dump every note as portable JSON (the DB lives outside any repo, so this is how
you back it up or move it between machines).

Request: `{}` (ignored)

Response:
```json
{ "notes": [ { "repo_root": "...", "file_path": "...", "commit_sha": "...",
               "line": 5, "blob_sha": "...", "anchor_text": "...",
               "anchor_offset": 3, "kind": "note", "end_line": 5,
               "end_anchor_text": "", "end_anchor_offset": 0,
               "note_body": "...", "status": "active",
               "created_at": "...", "updated_at": "..." } ] }
```

### `import`

Insert notes from an `export` payload, preserving timestamps and status. Rows
are added as **new** records (ids are reassigned), so it is meant for restoring
into a fresh database — importing the same file twice duplicates.

Request:
```json
{ "notes": [ /* same shape as export */ ] }
```
Response:
```json
{ "imported": 12 }
```
