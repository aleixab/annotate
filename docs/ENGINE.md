# Engine architecture (`nvim-annotate`)

The engine is the Go half of the project: a single static binary that owns **all**
storage, git interaction and line-remapping. It has no knowledge of Neovim and is
fully usable as a standalone CLI. Every subcommand reads one JSON object on stdin
and writes one JSON object on stdout; errors are written as `{"error": "..."}`
and the process exits non-zero.

The exact wire format for each command lives in [`../CONTRACT.md`](../CONTRACT.md).
This document explains how the program is built and *why*.

## Design goals

- **Correctness over features.** A note must never silently land on the wrong
  line. When the engine is unsure it degrades predictably (fast path → diff →
  fuzzy → orphan) and surfaces orphans rather than guessing.
- **Single static binary.** Pure-Go SQLite (`modernc.org/sqlite`, no cgo) so the
  build is one file you drop on `$PATH`.
- **Multi-repo, multi-instance.** Designed for hundreds of repos with many Neovim
  instances open against one shared database.
- **Private.** The database lives outside every repo (XDG data dir) and is never
  committed.

## Package layout

```
cmd/nvim-annotate/   CLI entry point: arg dispatch, stdin/stdout JSON, DB path
internal/core/       orchestration: command handlers + the remap algorithm
internal/store/      SQLite persistence (schema, migrations, queries)
internal/gitutil/    git plumbing (repo root, hash-object, HEAD, diff)
internal/diff/       unified-diff parser + line walker
internal/fuzzy/      Levenshtein similarity + sliding-window context matcher
```

Dependencies flow one way: `cmd → core → {store, gitutil, diff, fuzzy}`. `core`
is the only package that knows about all the others; the leaf packages are pure
and independently testable.

## Data model

One annotation is one row in the `notes` table (`internal/store/store.go`):

| column                              | meaning                                             |
| ----------------------------------- | --------------------------------------------------- |
| `id`                                | autoincrement primary key                           |
| `repo_root`, `file_path`            | identity key: repo root + repo-relative path        |
| `commit_sha`                        | `HEAD` at creation — the diff baseline              |
| `line`                              | 1-based start line at creation                       |
| `blob_sha`                          | `git hash-object` of the working file at creation   |
| `anchor_text`, `anchor_offset`      | context block around `line`, and the line's index in it |
| `kind`                              | `note` or `section`                                  |
| `end_line`                          | end of the range (== `line` for a plain note)        |
| `end_anchor_text`, `end_anchor_offset` | second anchor, for the end of a section            |
| `note_body`                         | the note text                                        |
| `status`                            | `active` or `orphaned` (cached result of last remap) |
| `created_at`, `updated_at`          | RFC3339 timestamps                                   |

Identity is **always** `(repo_root, file_path)`, where `repo_root` is resolved by
asking git from the *file's own directory* — never the editor's cwd. The same
relative path in two repos therefore never collides.

### SQLite configuration

`store.Open` uses this DSN (the pragma **order matters**):

```
file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)
```

`busy_timeout` must be set *before* `journal_mode(WAL)` so the busy handler is
already active when the connection performs the initial WAL switch (which briefly
needs an exclusive lock under concurrency). WAL + busy_timeout is what lets many
Neovim instances write the shared DB concurrently without "database is locked".
`SetMaxOpenConns(1)` keeps a single CLI invocation from contending with itself;
WAL handles the cross-process case.

Schema changes are additive: `migrate()` reads `PRAGMA table_info(notes)` and
`ALTER TABLE ... ADD COLUMN` for any column in the `migrations` list that an
older database is missing, so upgrades never lose notes.

## The anchoring & remapping algorithm

This is the heart of the engine. At **creation** (`core.Add`) it records, against
the file *as it is on disk* (uncommitted edits included): `blob_sha`,
`commit_sha`, `anchor_text` (the line ± `ContextLines` = 3 lines), and the line
number. Sections additionally anchor their end line.

At **read** (`core.List`, also `Search` and `Reanchor`) every note is relocated
to the current buffer via the cheapest path that works (`core.remapPos`):

1. **Blob fast-path** — if `git hash-object` of the working file equals the stored
   `blob_sha`, the file is byte-identical to creation; use the stored line
   directly. Zero work, the common case.
2. **Diff walk** — otherwise diff the stored `commit_sha` against the working tree
   (`gitutil.Diff`), parse the hunks (`diff.Parse`) and walk the line number
   through them (`diff.MapLine`). The result is **verified**: the walked line must
   still resemble the stored anchor line (`fuzzy.Ratio ≥ VerifyThreshold` = 0.6),
   guarding against a baseline that had uncommitted edits at creation.
3. **Fuzzy fallback** — no usable diff (e.g. the baseline commit is gone after a
   rebase): slide the stored context block over the buffer and take the best
   window whose average per-line similarity ≥ `FuzzyThreshold` (0.7)
   (`fuzzy.BestMatch`).
4. **Orphan** — nothing matched. The note is marked `orphaned`, its line falls
   back to the stored value, and it is **never dropped**.

For a **section**, the start and end lines are remapped independently. A section
is orphaned only if its *start* anchor is lost; if just the end anchor moves out
of reach, the range keeps its original height from the remapped start
(`core.remapNote`).

`List` persists any `active`↔`orphaned` change it discovers, so `status` is a
cache of the most recent remap.

### Why each leaf package exists

- **`gitutil`** — thin wrappers over `git -C <dir>`: `RepoRoot` (`rev-parse
  --show-toplevel` from the file dir), `HashObject` (`hash-object`, does not write
  the object), `HeadSHA` (`""` if the repo has no commits), `Diff` (`diff
  --no-color --unified=3 <commit> -- <relPath>`).
- **`diff`** — `Parse` turns unified-diff text into hunks (`@@` header regex,
  classifying body lines as context/add/del); `MapLine` walks a 1-based old-side
  line to its new-side position, reporting `deleted` when the line itself was
  removed.
- **`fuzzy`** — `Levenshtein` (rolling two-row edit distance), `Ratio`
  (whitespace-trimmed similarity in `[0,1]`), `BestMatch` (sliding window over the
  file returning the 1-based anchored line).

## Command surface

All commands are dispatched in `cmd/nvim-annotate/main.go`. See `CONTRACT.md` for
request/response JSON.

| command    | handler        | what it does                                                              |
| ---------- | -------------- | ------------------------------------------------------------------------ |
| `add`      | `core.Add`     | resolve repo identity, capture anchors, insert. Sections get dual anchors. |
| `list`     | `core.List`    | every note for a file, remapped to current lines; persists status changes. |
| `edit`     | `core.Edit`    | update a note's body.                                                      |
| `delete`   | `core.Delete`  | delete a note by id.                                                       |
| `search`   | `core.Search`  | case-insensitive substring over all bodies, all repos; remaps when the file exists. |
| `reanchor` | `core.Reanchor`| re-capture baseline + context at the current (or an overridden) line.     |
| `prune`    | `core.Prune`   | delete notes whose file no longer exists (`dry_run` to preview).          |
| `export`   | `core.Export`  | dump every note as portable JSON.                                         |
| `import`   | `core.Import`  | insert notes from an export, preserving timestamps/status (new ids).      |
| `stats`    | `core.Stats`   | aggregate totals, per-repo breakdown, missing-file count, DB path + size. |

### Re-anchoring in detail

`Reanchor` is what keeps the cheap paths healthy after heavy churn. With no
override it re-anchors at the note's current *remapped* position and refuses if
the note is orphaned (there is no trustworthy line). A positive `line` override
lets the client re-anchor an orphan to wherever the user moved the cursor; a
section keeps its original span height unless `end_line` is also given. It rewrites
`commit_sha`, `blob_sha`, `line`/`end_line`, the anchor blocks, and resets
`status` to active (`store.UpdateAnchors`).

## Database location

`databasePath()` resolves, in order: `$NVIM_ANNOTATE_DB_DIR` (used by tests) →
`$XDG_DATA_HOME/nvim-annotations` → `~/.local/share/nvim-annotations`, and creates
the directory. The file is `notes.db`. It is deliberately outside any repo so it
can never be committed.

## Building & testing

```sh
go build -o nvim-annotate ./cmd/nvim-annotate   # single static binary
go test ./...                                    # diff walker, fuzzy, remap, repo resolution, reanchor, prune
go vet ./...
```

The tests in `internal/core/core_test.go` exercise the remap algorithm directly
(blob/diff/fuzzy/orphan), plus full `Add→List`, `Reanchor` and `Prune` round trips
against a real temp git repo and a temp database.
