# Plugin architecture (`annotate.nvim`)

The plugin is the Neovim half: a **thin client** in a single Lua module
(`lua/annotate/init.lua`). It owns no storage and no git logic. Its whole job is
to capture context (cursor, file, range), shell out to the `nvim-annotate` binary
over JSON, and paint the returned annotations as extmarks. All anchoring,
remapping and persistence lives in the engine (see [`ENGINE.md`](ENGINE.md)).

## Why thin

Keeping the client dumb means the hard, correctness-critical work (remapping,
git, SQLite) is in one tested Go binary that is also usable on its own. The Lua
side can be reloaded freely, has no state worth losing, and never has to reason
about lines moving — it just asks the engine "where is this note now?" on every
buffer load.

## Engine I/O

`run(subcmd, payload)` is the single seam to the engine:

- encodes `payload` as JSON, runs `{ bin, subcmd }` via `vim.system` (falling back
  to `vim.fn.system` on older Neovim), feeding the JSON on stdin;
- decodes the one-line JSON response;
- returns `nil, err` if the process failed, produced no output, emitted invalid
  JSON, or returned an `{"error": ...}` object.

Every higher-level function goes through `run`. Background refreshes stay silent
on error (a file outside any repo is normal); user-initiated actions surface
errors via `notify` (`vim.notify` with an "annotate" title).

## State model

```lua
state[bufnr] = { notes = { ... }, visible = bool }
```

A per-buffer cache. Each cached note is `{ id, line, end_line, kind, body,
orphaned }` — already remapped to the current buffer by the engine. `visible`
backs `:AnnotateToggle`. `state` is cleared on `BufWipeout`.

`M.refresh(bufnr)` calls `list`, rebuilds `state[bufnr].notes`, and re-renders.
It runs automatically on `BufReadPost` / `BufEnter` and after `:w`
(`BufWritePost`).

## Configuration

`require("annotate").setup(opts)` deep-merges `opts` over `M.config`:

| option                   | default            | meaning                                                  |
| ------------------------ | ------------------ | -------------------------------------------------------- |
| `bin`                    | `"nvim-annotate"`  | path to the engine binary                                |
| `virt_text_prefix`       | `"  ▎ "`           | separator between code and the inline note               |
| `max_virt_len`           | `120`              | truncate long bodies in the inline display               |
| `highlights`             | (table)            | highlight group names (see Rendering)                    |
| `section_style`          | `"bar"`            | `"bar"` (gutter stripe) \| `"tint"` (block bg) \| `"both"` |
| `colors`                 | 7 accents          | named section accent colors (hex)                        |
| `tint_strength`          | `0.22`             | accent-over-background blend strength for `"tint"`       |
| `reanchor_on_save`       | `false`            | re-baseline a file's notes on every `:w`                 |
| `nav_keys`               | `true`             | bind `]a` / `[a`                                          |
| `max_history`            | `100`              | persisted undo/redo depth                                |
| `statusline_icon`        | `"✎"`              | prefix for the statusline note count                     |
| `statusline_orphan_icon` | `"⚠"`              | prefix for the statusline orphan count                   |

## Rendering

`render(bufnr)` clears the plugin's namespace and repaints from `state` using
extmarks. Three visual layers:

1. **Sign column** — always shown (even when toggled off), so a note leaves a
   trace. Plain notes get `▌` (or `○` when orphaned). Sections get a **vivid
   colored bar `▎` down the whole span** in the accent color — this is the default
   marker (`section_style = "bar"`) and lives in the gutter so it never tints the
   code background.
2. **Section block tint** — only with `section_style = "tint"`/`"both"` and only
   when visible: a `line_hl_group` across the range, the accent blended over the
   editor's `Normal` background at `tint_strength`.
3. **Inline text** (only when visible) — one note on a line renders as end-of-line
   virtual text; several notes render as an end-of-line `N notes` count plus the
   notes stacked on `virt_lines` beneath.

### Colors

`define_highlights()` (re-run on `ColorScheme`) builds, per named color:

- `AnnotateSection_<color>` — `bg` = accent blended over `Normal` bg (the tint),
- `AnnotateSectionBar_<color>` — `fg` = the vivid accent (the gutter bar),
- `AnnotateSectionTitle_<color>` — `fg` = accent, bold (the `SECTION:` label).

Helpers: `hex2rgb`, `blend(accent, bg, alpha)`, `normal_bg()` (reads the `Normal`
highlight, falls back per `background`), and `ansi(hex, s)` (a truecolor escape
for fzf's `--ansi`). Section bodies are parsed by `parse_section` which pulls an
optional leading color word: `"red"`, `"red - title"`, `"red title"`, or just a
title all work.

## Public functions & commands

`setup` registers a user command for each and (optionally) the nav keys.

| command             | function          | notes                                                       |
| ------------------- | ----------------- | ----------------------------------------------------------- |
| `:AnnotateAdd`      | `add_quick`       | one-line note via `vim.ui.input`                            |
| `:AnnotateAddBig`   | `add`             | note in the floating multi-line editor                      |
| `:AnnotateSection`  | `section`         | range command; prompts `color - title`                      |
| `:AnnotateEdit`     | `edit`            | edit note under cursor (picks if several)                   |
| `:AnnotateDelete`   | `delete`          | delete note under cursor (picks if several)                 |
| `:AnnotateHover`    | `hover`           | full body in a read-only float                              |
| `:AnnotateReanchor` | `reanchor`        | re-baseline under cursor; else anchor an orphan to cursor   |
| `:AnnotateList`     | `list`            | all annotations, fzf-lua (→ `vim.ui.select` fallback)       |
| `:AnnotateOrphans`  | `orphans`         | same picker, filtered to orphans                            |
| `:AnnotateToggle`   | `toggle`          | hide text + tint (signs stay)                               |
| `:AnnotateUndo`     | `undo`            | persistent undo                                             |
| `:AnnotateRedo`     | `redo`            | persistent redo                                             |
| `:AnnotatePrune`    | `prune`           | drop notes for missing files                                |
| `:AnnotateStatus`   | `status`          | store dashboard in a float                                  |
| `:AnnotateQuickfix` | `quickfix`        | buffer's notes → quickfix (`!` = all repos)                 |
| `:AnnotateExport`   | `export {path}`   | write all notes to a JSON file                              |
| `:AnnotateImport`   | `import {path}`   | restore notes from a JSON file                              |
| (`]a` / `[a`)       | `next_note` / `prev_note` | jump between annotations in the buffer (wraps)       |
| —                   | `statusline`      | string for a statusline component (cache-only, no engine call) |

### Selection & pickers

`notes_at_cursor()` returns the notes whose `[line, end_line]` span the cursor.
`pick(notes, cb)` calls back immediately for one note, or disambiguates with
`vim.ui.select` for several. `run_picker(title, filter)` builds the item list once
and dispatches to fzf-lua when present (ANSI-tinted sections, jump on select) or
to `vim.ui.select` otherwise — so the plugin works without fzf installed.

### The floating editor & float

`open_editor` creates an `acwrite` scratch buffer (so `:w` triggers a
`BufWriteCmd` instead of failing with E382); `:w` or `<C-s>` saves, `q`/`<Esc>`
cancels. `open_float` is the read-only variant used by hover and the status
dashboard.

## Undo / redo (persistent)

The undo stack is **data only** — no closures — so it serializes to
`stdpath("data")/nvim-annotate/history.json` and survives restarts (capped at
`max_history`). Each entry is `{ op, file, line, end_line, kind, body, id,
old_body }`. `reverse(e)` undoes and `forward(e)` re-applies, deriving the inverse
from `op` and updating `e.id` in place when a re-add assigns a fresh row id (an
undone delete is re-created, re-anchored to the file's current state). `record`,
`undo` and `redo` all persist after mutating the stacks; `load_history()` runs in
`setup`.

The low-level engine ops (`op_add`, `op_delete`, `op_edit`) are shared by the
public operations (`do_add`, `do_delete`, `do_edit`) and by the undo machinery,
so there is one code path per engine call.

## Autocommands

Registered under the `annotate` augroup in `setup`:

- `BufReadPost`, `BufEnter` → `M.refresh` (re-list + re-render on load/enter).
- `BufWritePost` → optional `reanchor_buffer` (when `reanchor_on_save`) then
  `M.refresh`.
- `ColorScheme` → `define_highlights` (rebuild blended colors for the new theme).
- `BufWipeout` → drop the buffer's `state`.

## Extending

Because the client is thin, most changes are either a new engine command surfaced
through `run`, or a new way to render/select the cached `state`. The
[`examples/lazyvim.lua`](../examples/lazyvim.lua) spec lists every command, the
suggested `<leader>n*` keymaps, all config options, and an optional lualine
component that merges `statusline()` into your existing config.
