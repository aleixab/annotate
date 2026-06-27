# nvim-annotate

Commit-anchored, **private** code annotations for Neovim. Notes render inline as
virtual text next to your code, are stored entirely outside any repo (never
committed, never shared), and survive file churn — teammate commits, `git pull`,
edits made in other editors — by being anchored to git worktree state and
remapped to the current line every time a buffer loads.

Single-user tool. No server, no sync, no auth. Built for correctness and trust
over features.

## Architecture

Two components with a clean JSON-over-stdio seam between them
(see [`CONTRACT.md`](CONTRACT.md)). Deep dives:
[`docs/ENGINE.md`](docs/ENGINE.md) (the Go binary) and
[`docs/PLUGIN.md`](docs/PLUGIN.md) (the Lua plugin).

- **`nvim-annotate`** — a single static Go binary (pure-Go SQLite, no cgo). Owns
  all storage, git interaction and line-remapping. No knowledge of Neovim;
  usable directly as a CLI.
- **`annotate.nvim`** — a thin Lua client. Captures context, shells out to the
  binary, paints extmarks. No storage or git logic.

### Anchoring & remapping

Each note is anchored to the file's content **as it is on disk** at creation
(uncommitted edits included). On every buffer load the engine relocates the line
via the cheapest path that works:

1. **blob fast-path** — `git hash-object` of the working file unchanged → use the
   stored line directly.
2. **diff walk** — diff the stored `HEAD` baseline against the working tree and
   walk the line number through the hunks (handles edits elsewhere in the file).
   The result is verified against the original line's text.
3. **fuzzy fallback** — slide the stored context block over the buffer and take
   the best match above a similarity threshold.
4. **orphan** — if nothing matches, mark the note `orphaned`. Orphans are
   **never silently dropped**; they render distinctly so you resolve them by hand.

### Multi-repo & concurrency

Designed for working across hundreds of repos with many Neovim instances open:

- Repo identity is resolved **per file** (walk up to `.git`), never from cwd, so
  the same relative path in different repos never collides. Key is
  `(repo_root, file_path)`.
- One shared SQLite DB at `~/.local/share/nvim-annotations/notes.db` (XDG data
  dir — deliberately outside any repo). WAL mode + `busy_timeout` make concurrent
  writers wait rather than error. Each command uses a short-lived connection.

## Install

### Binary

```sh
go build -o nvim-annotate ./cmd/nvim-annotate
# put it on your PATH, e.g.
install -m755 nvim-annotate ~/.local/bin/
```

### Plugin (lazy.nvim)

```lua
{
  dir = "/path/to/this/repo", -- or your fork's URL
  dependencies = { "ibhagwan/fzf-lua" },
  build = "go build -o nvim-annotate ./cmd/nvim-annotate",
  config = function()
    require("annotate").setup({
      bin = "/path/to/this/repo/nvim-annotate", -- absolute path = no PATH needed
    })
  end,
}
```

A complete, ready-to-copy LazyVim spec (every command, keymap, and option,
loaded lazily on file open / command / key) is kept in sync at
[`examples/lazyvim.lua`](examples/lazyvim.lua). It also notes how to free
`<leader>n` from LazyVim's notification-history mapping.

## Commands

| Command            | Action                                                         |
| ------------------ | ------------------------------------------------------------- |
| `:AnnotateAdd`     | Quick one-line note on the current line (`vim.ui.input`).     |
| `:AnnotateAddBig`  | Note in the full floating multi-line editor.                  |
| `:AnnotateSection` | (range) Tint the selected lines as a section + optional title. |
| `:AnnotateEdit`    | Edit the annotation under the cursor.                         |
| `:AnnotateDelete`  | Delete the annotation under the cursor.                       |
| `:AnnotateHover`   | Peek the full annotation body under the cursor in a float.    |
| `:AnnotateReanchor`| Re-baseline the annotation under the cursor (fix orphans).   |
| `:AnnotateList`    | Browse every annotation across all repos (fzf-lua).          |
| `:AnnotateOrphans` | Browse only orphaned annotations (fzf-lua).                  |
| `:AnnotateToggle`  | Show/hide annotation text **and** section tints (signs stay). |
| `:AnnotateUndo`    | Undo the last annotation change (persists across sessions).  |
| `:AnnotateRedo`    | Redo the last undone change.                                 |
| `:AnnotatePrune`   | Delete annotations whose file no longer exists on disk.      |
| `:AnnotateStatus`  | Dashboard: totals, orphans, per-repo breakdown, DB size.    |
| `:AnnotateQuickfix`| Send annotations to the quickfix list (`!` = all repos).    |
| `:AnnotateExport`  | `{path}` — write all annotations to a JSON backup file.      |
| `:AnnotateImport`  | `{path}` — restore annotations from a JSON backup file.      |

With `nav_keys` (default on), `]a` / `[a` jump to the next / previous annotation
in the current buffer.

The fzf-lua pickers (`:AnnotateList` / `:AnnotateOrphans`) degrade gracefully to
the built-in `vim.ui.select` when fzf-lua is not installed.

### Statusline

`require("annotate").statusline()` returns a compact indicator for the current
buffer (note count, plus an orphan marker), or `""` when the buffer has none. It
reads the cache only — no engine call — so it is safe on every redraw. Example
lualine component:

```lua
{ function() return require("annotate").statusline() end }
```

In the floating editor, `:w` or `<C-s>` saves and `q`/`<Esc>` cancels. When more
than one annotation overlaps the cursor, `:AnnotateEdit`/`:AnnotateDelete` prompt
you to choose.

Suggested keymaps:

```lua
vim.keymap.set("n", "<leader>na", "<cmd>AnnotateAdd<cr>",     { desc = "Annotate: quick note" })
vim.keymap.set("n", "<leader>nA", "<cmd>AnnotateAddBig<cr>",  { desc = "Annotate: note (full editor)" })
vim.keymap.set("x", "<leader>ns", ":AnnotateSection<cr>",     { desc = "Annotate: section from selection" })
vim.keymap.set("n", "<leader>ne", "<cmd>AnnotateEdit<cr>",     { desc = "Annotate: edit" })
vim.keymap.set("n", "<leader>nd", "<cmd>AnnotateDelete<cr>",   { desc = "Annotate: delete" })
vim.keymap.set("n", "<leader>nk", "<cmd>AnnotateHover<cr>",    { desc = "Annotate: hover" })
vim.keymap.set("n", "<leader>nR", "<cmd>AnnotateReanchor<cr>", { desc = "Annotate: re-anchor" })
vim.keymap.set("n", "<leader>nl", "<cmd>AnnotateList<cr>",     { desc = "Annotate: list" })
vim.keymap.set("n", "<leader>no", "<cmd>AnnotateOrphans<cr>",  { desc = "Annotate: orphans" })
vim.keymap.set("n", "<leader>nt", "<cmd>AnnotateToggle<cr>",   { desc = "Annotate: toggle" })
vim.keymap.set("n", "<leader>nu", "<cmd>AnnotateUndo<cr>",     { desc = "Annotate: undo" })
vim.keymap.set("n", "<leader>nr", "<cmd>AnnotateRedo<cr>",     { desc = "Annotate: redo" })
```

## Sections & colors

Visually select lines and run `:AnnotateSection` (or `<leader>ns`) to paint a
tinted block over them. At the prompt you can type just a color, a title, or
both (with or without a dash):

```
blue                  → blue section, no title
blue - tests cleanup  → blue section titled "tests cleanup"
tests cleanup         → default tint, titled "tests cleanup"
```

Sections render as `SECTION: tests cleanup`, colored to match — clearly distinct
from plain notes, and the color also shows in the `:AnnotateList` picker.

Built-in colors: `red`, `green`, `blue`, `yellow`, `orange`, `purple`, `cyan`.
By default a section is marked by a **vivid colored bar down the sign column**
for its whole span — it lives in the gutter, so it never tints the code
background and stays crisp on any theme (including dark ones). Configure it:

```lua
require("annotate").setup({
  section_style = "bar",           -- "bar" (default) | "tint" | "both"
  tint_strength = 0.22,            -- block-tint strength when style uses "tint"
  colors = { blue = "#7aa2f7" },   -- override / add accent colors
})
```

`"tint"` switches to a subtle accent-over-background block highlight instead of
the bar; `"both"` shows them together.

Or override the highlight groups directly: `AnnotateSection` (tint),
`AnnotateSectionBar` (gutter bar) and `AnnotateSectionTitle` (defaults), plus
`AnnotateSection_<color>` / `AnnotateSectionBar_<color>` /
`AnnotateSectionTitle_<color>` per color.

Multiple annotations on one line are all kept: the line leads with a `N notes`
count and stacks each note on its own virtual line beneath it; edit/delete let
you pick among them.

## Re-anchoring & orphans

A note is anchored to a baseline commit captured when you created it. After a
lot of churn (rebases, big refactors) the cheap blob/diff remap paths can stop
applying and the note leans on fuzzy matching — or orphans. `:AnnotateReanchor`
re-captures the baseline and context block against the file **as it is now**, at
the note's current line, keeping it healthy long-term.

It's also the manual fix for an orphan: put the cursor on the correct line and
run `:AnnotateReanchor` — if no note overlaps the cursor it offers the buffer's
orphans and re-anchors the chosen one to the cursor line. Browse orphans across
all repos with `:AnnotateOrphans`.

Set `reanchor_on_save = true` to automatically re-baseline a file's (non-orphan)
notes every time you write it — maximally fresh anchors, at the cost of a quick
engine call per save.

## Undo / redo

`:AnnotateUndo` / `:AnnotateRedo` reverse add, delete and edit operations. The
stack is **persisted** to `stdpath("data")/nvim-annotate/history.json`, so it
survives restarts (capped at `max_history`, default 100). Undoing a delete
re-creates the annotation, re-anchored to the file's current state.

## Backup & maintenance

Because annotations live outside every repo and are never committed, back them up
yourself: `:AnnotateExport ~/notes-backup.json` dumps everything;
`:AnnotateImport ~/notes-backup.json` restores into a fresh database (timestamps
and orphan status preserved; importing twice duplicates). `:AnnotatePrune` drops
notes whose file no longer exists on disk.

## CLI usage

The engine is independently usable; every command reads JSON on stdin:

```sh
echo '{"file":"'"$PWD"'/main.go","line":4,"body":"the entrypoint"}' | nvim-annotate add
echo '{"file":"'"$PWD"'/main.go"}'                                  | nvim-annotate list
echo '{"query":"entrypoint"}'                                       | nvim-annotate search
```

## Development

```sh
go test ./...        # unit tests: repo resolution, diff walker, fuzzy, orphan
go build ./...
```

Highlight groups (override as you like): `AnnotateNote`, `AnnotateOrphan`,
`AnnotateSign`, `AnnotateOrphanSign`, `AnnotateSection`, `AnnotateSectionBar`,
`AnnotateSectionTitle`, and per-color `AnnotateSection_<color>` /
`AnnotateSectionBar_<color>` / `AnnotateSectionTitle_<color>`.
