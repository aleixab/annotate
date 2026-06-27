-- annotate.nvim — thin client for the nvim-annotate engine.
--
-- This module owns no storage or git logic. It captures cursor/file/range
-- context, shells out to the Go binary over JSON, and paints returned
-- annotations as extmarks (inline virtual text, tinted section backgrounds, and
-- sign-column markers). All anchoring, remapping and persistence lives in the
-- binary.

local M = {}

M.config = {
  bin = "nvim-annotate", -- path to the engine binary (on PATH by default)
  virt_text_prefix = "  ▎ ", -- separates code from the note on the same line
  max_virt_len = 120, -- truncate long bodies in the inline display
  highlights = {
    note = "AnnotateNote",
    orphan = "AnnotateOrphan",
    sign = "AnnotateSign",
    orphan_sign = "AnnotateOrphanSign",
    section = "AnnotateSection", -- default section tint (background)
    section_title = "AnnotateSectionTitle", -- default section title (foreground)
    section_bar = "AnnotateSectionBar", -- default section gutter bar (foreground)
  },
  -- How a section's span is marked:
  --   "bar"  — a vivid colored bar down the sign column (default; never tints
  --            the code background, so it stays crisp on any theme).
  --   "tint" — a subtle accent-over-background block highlight.
  --   "both" — bar + tint together.
  section_style = "bar",
  -- Named section accent colors (vivid). The block tint is derived by blending
  -- the accent over the editor's background, so it stays subtle and matches any
  -- theme; the accent itself colors the "SECTION:" label and the picker entry.
  colors = {
    red = "#f7768e",
    green = "#9ece6a",
    blue = "#7aa2f7",
    yellow = "#e0af68",
    orange = "#ff9e64",
    purple = "#bb9af7",
    cyan = "#7dcfff",
  },
  tint_strength = 0.22, -- how strongly the accent tints the block (0..1)
  reanchor_on_save = false, -- re-baseline a file's notes to its content on :w
  nav_keys = true, -- bind ]a / [a to jump between annotations
  max_history = 100, -- cap on the persisted undo/redo stack
  statusline_icon = "✎", -- prefix for the statusline note count
  statusline_orphan_icon = "⚠", -- prefix for the statusline orphan count
}

local ns = vim.api.nvim_create_namespace("annotate")
local set_extmark = vim.api.nvim_buf_set_extmark

-- Per-buffer cache: bufnr -> { notes = {...}, visible = bool }
local state = {}

-- Session-scoped undo/redo of annotation operations.
local history = { undo = {}, redo = {} }

-- ---------------------------------------------------------------------------
-- Engine I/O
-- ---------------------------------------------------------------------------

local function run(subcmd, payload)
  local input = vim.json.encode(payload or {})
  local cmd = { M.config.bin, subcmd }

  local out, code
  if vim.system then
    local res = vim.system(cmd, { stdin = input, text = true }):wait()
    out, code = res.stdout, res.code
    if (out == nil or out == "") and res.stderr and res.stderr ~= "" then
      out = res.stderr
    end
  else
    out = vim.fn.system(cmd, input)
    code = vim.v.shell_error
  end

  if out == nil or out == "" then
    return nil, ("annotate: empty response (exit %s)"):format(tostring(code))
  end
  local ok, decoded = pcall(vim.json.decode, out)
  if not ok then
    return nil, "annotate: bad JSON from engine: " .. out
  end
  if type(decoded) == "table" and decoded.error then
    return nil, "annotate: " .. decoded.error
  end
  return decoded, nil
end

local function notify(msg, level)
  vim.notify(msg, level or vim.log.levels.INFO, { title = "annotate" })
end

-- ---------------------------------------------------------------------------
-- Color helpers (theme-aware tints + fzf ANSI)
-- ---------------------------------------------------------------------------

local function hex2rgb(h)
  h = h:gsub("#", "")
  return tonumber(h:sub(1, 2), 16), tonumber(h:sub(3, 4), 16), tonumber(h:sub(5, 6), 16)
end

-- blend mixes accent over bg with the given alpha and returns a #rrggbb string.
local function blend(accent, bg, alpha)
  local ar, ag, ab = hex2rgb(accent)
  local br, bg_, bb = hex2rgb(bg)
  local mix = function(a, b)
    return math.floor(a * alpha + b * (1 - alpha) + 0.5)
  end
  return string.format("#%02x%02x%02x", mix(ar, br), mix(ag, bg_), mix(ab, bb))
end

local function normal_bg()
  local ok, hl = pcall(vim.api.nvim_get_hl, 0, { name = "Normal", link = false })
  if ok and hl and hl.bg then
    return string.format("#%06x", hl.bg)
  end
  return vim.o.background == "light" and "#eeeeee" or "#1a1b26"
end

-- ansi wraps text in a truecolor escape for fzf's --ansi rendering.
local function ansi(hex, s)
  local r, g, b = hex2rgb(hex)
  return string.format("\27[38;2;%d;%d;%dm%s\27[0m", r, g, b, s)
end

-- ---------------------------------------------------------------------------
-- Body / section parsing
-- ---------------------------------------------------------------------------

-- parse_section pulls an optional leading color word off a section body. All of
-- these work: "red", "red - title", "red title". Without a recognized color the
-- whole body is the title and the default tint is used.
local function parse_section(body)
  body = body or ""
  local first, rest = body:match("^%s*(%w+)%s*(.-)%s*$")
  if first and M.config.colors[first:lower()] then
    rest = rest:gsub("^%-%s*", "") -- drop an optional "- " separator
    return first:lower(), vim.trim(rest)
  end
  return nil, vim.trim(body)
end

local function section_tint(color)
  return color and ("AnnotateSection_" .. color) or M.config.highlights.section
end
local function section_title_hl(color)
  return color and ("AnnotateSectionTitle_" .. color) or M.config.highlights.section_title
end
local function section_bar_hl(color)
  return color and ("AnnotateSectionBar_" .. color) or M.config.highlights.section_bar
end

local function oneline(s)
  return vim.trim((s or ""):gsub("%s*\n%s*", " ⏎ "))
end

local function truncate(s)
  if vim.fn.strdisplaywidth(s) > M.config.max_virt_len then
    s = vim.fn.strcharpart(s, 0, M.config.max_virt_len) .. "…"
  end
  return s
end

-- entry_for produces the inline {text, hl} label for one annotation.
local function entry_for(n)
  local hl = M.config.highlights
  if n.kind == "section" then
    local color, title = parse_section(n.body)
    local text = "SECTION" .. (title ~= "" and (": " .. title) or "")
    return { text = text, hl = section_title_hl(color) }
  end
  local body = oneline(n.body)
  if body == "" then
    return nil
  end
  return { text = body, hl = n.orphaned and hl.orphan or hl.note }
end

-- ---------------------------------------------------------------------------
-- Rendering
-- ---------------------------------------------------------------------------

local function render(bufnr)
  local st = state[bufnr]
  if not st then
    return
  end
  vim.api.nvim_buf_clear_namespace(bufnr, ns, 0, -1)
  local lc = vim.api.nvim_buf_line_count(bufnr)
  local hl = M.config.highlights
  local prefix = M.config.virt_text_prefix

  local inline = {} -- line -> list of {text, hl}

  local style = M.config.section_style
  for _, n in ipairs(st.notes) do
    if n.kind == "section" then
      -- Sections are marked by a vivid colored bar running down the sign
      -- column for the whole span (and, optionally, a subtle block tint). The
      -- bar lives in the gutter so it never muddies the code background, and
      -- like other signs it stays even when text is toggled off.
      local color = parse_section(n.body)
      local bar = n.orphaned and hl.orphan_sign or section_bar_hl(color)
      local tint = section_tint(color)
      for l = n.line, n.end_line do
        if l >= 1 and l <= lc then
          local em = { sign_text = "▎", sign_hl_group = bar }
          if st.visible and (style == "tint" or style == "both") then
            em.line_hl_group = tint
          end
          pcall(set_extmark, bufnr, ns, l - 1, 0, em)
        end
      end
    elseif n.line >= 1 and n.line <= lc then
      -- Plain note: a single sign marker on its line, always shown.
      pcall(set_extmark, bufnr, ns, n.line - 1, 0, {
        sign_text = n.orphaned and "○" or "▌",
        sign_hl_group = n.orphaned and hl.orphan_sign or hl.sign,
      })
    end

    local e = entry_for(n)
    if e then
      inline[n.line] = inline[n.line] or {}
      table.insert(inline[n.line], e)
    end
  end

  if not st.visible then
    return
  end

  for line, items in pairs(inline) do
    if line >= 1 and line <= lc then
      if #items == 1 then
        -- Single annotation: show it inline at end of line.
        pcall(set_extmark, bufnr, ns, line - 1, 0, {
          virt_text = { { prefix .. truncate(items[1].text), items[1].hl } },
          virt_text_pos = "eol",
        })
      else
        -- Several annotations: lead with the count, then stack them below.
        pcall(set_extmark, bufnr, ns, line - 1, 0, {
          virt_text = { { ("%s%d notes"):format(prefix, #items), hl.note } },
          virt_text_pos = "eol",
        })
        local vlines = {}
        for _, it in ipairs(items) do
          vlines[#vlines + 1] = { { prefix .. truncate(it.text), it.hl } }
        end
        pcall(set_extmark, bufnr, ns, line - 1, 0, { virt_lines = vlines })
      end
    end
  end
end

-- ---------------------------------------------------------------------------
-- Refresh
-- ---------------------------------------------------------------------------

function M.refresh(bufnr)
  bufnr = bufnr or vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(bufnr)
  if file == "" or vim.bo[bufnr].buftype ~= "" then
    return
  end
  local resp, err = run("list", { file = file })
  if err then
    return -- stay quiet on background refreshes (files outside repos are normal)
  end
  local st = state[bufnr] or { visible = true }
  st.notes = {}
  for _, nt in ipairs(resp.notes or {}) do
    table.insert(st.notes, {
      id = nt.id,
      line = nt.line,
      end_line = (nt.end_line and nt.end_line > 0) and nt.end_line or nt.line,
      kind = nt.kind or "note",
      body = nt.body or "",
      orphaned = nt.orphaned or false,
    })
  end
  state[bufnr] = st
  render(bufnr)
end

local function refresh_file(file)
  for _, b in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_is_loaded(b) and vim.api.nvim_buf_get_name(b) == file then
      M.refresh(b)
    end
  end
end

-- ---------------------------------------------------------------------------
-- Undo / redo
-- ---------------------------------------------------------------------------

-- The undo stack is data-only (no closures) so it can be serialized to disk and
-- survive Neovim restarts. Each entry: { op, file, line, end_line, kind, body,
-- id, old_body }. Reversing/replaying is derived from the op; entry.id is
-- updated in place when a re-add assigns a fresh row id.

local function history_path()
  local dir = vim.fn.stdpath("data") .. "/nvim-annotate"
  vim.fn.mkdir(dir, "p")
  return dir .. "/history.json"
end

local function save_history()
  local ok, encoded = pcall(vim.json.encode, history)
  if not ok then
    return
  end
  local f = io.open(history_path(), "w")
  if f then
    f:write(encoded)
    f:close()
  end
end

local function load_history()
  local f = io.open(history_path(), "r")
  if not f then
    return
  end
  local data = f:read("*a")
  f:close()
  if not data or data == "" then
    return
  end
  local ok, decoded = pcall(vim.json.decode, data)
  if ok and type(decoded) == "table" then
    history.undo = decoded.undo or {}
    history.redo = decoded.redo or {}
  end
end

-- Low-level engine ops shared by the operations and undo/redo.
local function op_add(e) -- returns new id or nil
  local r, err = run("add", { file = e.file, line = e.line, end_line = e.end_line, kind = e.kind, body = e.body })
  if err then
    notify(err, vim.log.levels.ERROR)
    return nil
  end
  return r.id
end
local function op_delete(id)
  local _, err = run("delete", { id = id })
  if err then
    notify(err, vim.log.levels.ERROR)
    return false
  end
  return true
end
local function op_edit(id, body)
  local _, err = run("edit", { id = id, body = body })
  if err then
    notify(err, vim.log.levels.ERROR)
    return false
  end
  return true
end

-- reverse undoes an entry; forward re-applies it. Both mutate e.id as needed.
local function reverse(e)
  if e.op == "add" then
    op_delete(e.id)
  elseif e.op == "delete" then
    e.id = op_add(e) or e.id
  elseif e.op == "edit" then
    op_edit(e.id, e.old_body)
  end
end
local function forward(e)
  if e.op == "add" then
    e.id = op_add(e) or e.id
  elseif e.op == "delete" then
    op_delete(e.id)
  elseif e.op == "edit" then
    op_edit(e.id, e.body)
  end
end

local function record(entry)
  table.insert(history.undo, entry)
  history.redo = {}
  while #history.undo > M.config.max_history do
    table.remove(history.undo, 1)
  end
  save_history()
end

function M.undo()
  local e = table.remove(history.undo)
  if not e then
    notify("nothing to undo")
    return
  end
  reverse(e)
  table.insert(history.redo, e)
  save_history()
  refresh_file(e.file)
end

function M.redo()
  local e = table.remove(history.redo)
  if not e then
    notify("nothing to redo")
    return
  end
  forward(e)
  table.insert(history.undo, e)
  save_history()
  refresh_file(e.file)
end

-- ---------------------------------------------------------------------------
-- Engine-backed operations (each records an undo entry)
-- ---------------------------------------------------------------------------

-- do_add inserts an annotation. req is a full add request (file/line/...).
local function do_add(req, bufnr)
  local entry = {
    op = "add",
    file = req.file,
    line = req.line,
    end_line = req.end_line,
    kind = req.kind,
    body = req.body,
  }
  local id = op_add(entry)
  if not id then
    return
  end
  entry.id = id
  record(entry)
  M.refresh(bufnr)
end

local function do_delete(note, file, bufnr)
  if not op_delete(note.id) then
    return
  end
  record({
    op = "delete",
    file = file,
    line = note.line,
    end_line = note.end_line,
    kind = note.kind,
    body = note.body,
    id = note.id,
  })
  M.refresh(bufnr)
end

local function do_edit(note, new_body, file, bufnr)
  if not op_edit(note.id, new_body) then
    return
  end
  record({
    op = "edit",
    file = file,
    id = note.id,
    body = new_body,
    old_body = note.body,
  })
  M.refresh(bufnr)
end

-- do_reanchor re-baselines a note. Orphans pass the cursor line as the target.
local function do_reanchor(note, bufnr)
  local req = { id = note.id }
  if note.orphaned then
    req.line = vim.api.nvim_win_get_cursor(0)[1]
  end
  local _, err = run("reanchor", req)
  if err then
    notify(err, vim.log.levels.ERROR)
    return
  end
  notify("re-anchored")
  M.refresh(bufnr)
end

-- reanchor_buffer re-baselines every active note in a buffer (used on save).
local function reanchor_buffer(bufnr)
  local st = state[bufnr]
  if not st then
    return
  end
  for _, n in ipairs(st.notes) do
    if not n.orphaned then
      run("reanchor", { id = n.id })
    end
  end
end

-- ---------------------------------------------------------------------------
-- Floating editor (multi-line)
-- ---------------------------------------------------------------------------

local function open_editor(opts)
  local buf = vim.api.nvim_create_buf(false, true)
  -- acwrite (not the scratch default of nofile) lets `:w` run our BufWriteCmd
  -- instead of failing with E382.
  vim.bo[buf].buftype = "acwrite"
  vim.bo[buf].bufhidden = "wipe"
  vim.bo[buf].filetype = "markdown"
  pcall(vim.api.nvim_buf_set_name, buf, "annotate://" .. opts.title:gsub("%s+", "-") .. "-" .. buf)

  local init_lines = {}
  if opts.initial and opts.initial ~= "" then
    init_lines = vim.split(opts.initial, "\n")
    vim.api.nvim_buf_set_lines(buf, 0, -1, false, init_lines)
  end

  local width = math.min(100, math.max(50, math.floor(vim.o.columns * 0.6)))
  local height = math.min(math.floor(vim.o.lines * 0.6), math.max(8, #init_lines + 1))
  local win = vim.api.nvim_open_win(buf, true, {
    relative = "cursor",
    row = 1,
    col = 0,
    width = width,
    height = height,
    border = "rounded",
    title = " " .. opts.title .. " (:w / <C-s> save · q cancel) ",
    title_pos = "center",
    style = "minimal",
  })

  local function close()
    if vim.api.nvim_win_is_valid(win) then
      vim.api.nvim_win_close(win, true)
    end
  end
  local function save()
    local lines = vim.api.nvim_buf_get_lines(buf, 0, -1, false)
    local body = vim.trim(table.concat(lines, "\n"))
    vim.bo[buf].modified = false
    close()
    if body ~= "" then
      opts.on_save(body)
    end
  end

  vim.api.nvim_create_autocmd("BufWriteCmd", { buffer = buf, callback = save })
  vim.keymap.set({ "n", "i" }, "<C-s>", function()
    vim.cmd("stopinsert")
    save()
  end, { buffer = buf, nowait = true, silent = true })
  for _, key in ipairs({ "q", "<Esc>" }) do
    vim.keymap.set("n", key, close, { buffer = buf, nowait = true, silent = true })
  end
  vim.cmd("startinsert")
end

-- open_float shows read-only text in a small bordered window near the cursor.
local function open_float(title, body)
  local lines = vim.split((body ~= "" and body) or "(empty)", "\n")
  local buf = vim.api.nvim_create_buf(false, true)
  vim.api.nvim_buf_set_lines(buf, 0, -1, false, lines)
  vim.bo[buf].modifiable = false
  vim.bo[buf].bufhidden = "wipe"
  vim.bo[buf].filetype = "markdown"

  local width = 30
  for _, l in ipairs(lines) do
    width = math.max(width, vim.fn.strdisplaywidth(l))
  end
  width = math.min(100, width + 2)
  local height = math.min(math.floor(vim.o.lines * 0.6), math.max(1, #lines))

  local win = vim.api.nvim_open_win(buf, true, {
    relative = "cursor",
    row = 1,
    col = 0,
    width = width,
    height = height,
    border = "rounded",
    title = " " .. title .. " (q to close) ",
    title_pos = "center",
    style = "minimal",
  })
  local function close()
    if vim.api.nvim_win_is_valid(win) then
      vim.api.nvim_win_close(win, true)
    end
  end
  for _, key in ipairs({ "q", "<Esc>" }) do
    vim.keymap.set("n", key, close, { buffer = buf, nowait = true, silent = true })
  end
end

-- ---------------------------------------------------------------------------
-- Cursor / selection lookups
-- ---------------------------------------------------------------------------

local function notes_at_cursor()
  local bufnr = vim.api.nvim_get_current_buf()
  local st = state[bufnr]
  if not st then
    return bufnr, {}
  end
  local cur = vim.api.nvim_win_get_cursor(0)[1]
  local res = {}
  for _, n in ipairs(st.notes) do
    if cur >= n.line and cur <= n.end_line then
      table.insert(res, n)
    end
  end
  return bufnr, res
end

local function pick(notes, cb)
  if #notes == 0 then
    notify("no annotation on this line", vim.log.levels.WARN)
    return
  end
  if #notes == 1 then
    cb(notes[1])
    return
  end
  vim.ui.select(notes, {
    prompt = "Annotation:",
    format_item = function(n)
      local e = entry_for(n) or { text = "(empty)" }
      return e.text
    end,
  }, function(choice)
    if choice then
      cb(choice)
    end
  end)
end

-- ---------------------------------------------------------------------------
-- Public commands
-- ---------------------------------------------------------------------------

function M.add_quick()
  local bufnr = vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(bufnr)
  if file == "" then
    notify("buffer has no file", vim.log.levels.WARN)
    return
  end
  local line = vim.api.nvim_win_get_cursor(0)[1]
  vim.ui.input({ prompt = "Annotation: " }, function(input)
    if not input or vim.trim(input) == "" then
      return
    end
    do_add({ file = file, line = line, body = input }, bufnr)
  end)
end

function M.add()
  local bufnr = vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(bufnr)
  if file == "" then
    notify("buffer has no file", vim.log.levels.WARN)
    return
  end
  local line = vim.api.nvim_win_get_cursor(0)[1]
  open_editor({
    title = "New annotation",
    on_save = function(body)
      do_add({ file = file, line = line, body = body }, bufnr)
    end,
  })
end

-- section: tint a visual-mode (or :range) span; title accepts a "color - title".
function M.section(line1, line2)
  local bufnr = vim.api.nvim_get_current_buf()
  local file = vim.api.nvim_buf_get_name(bufnr)
  if file == "" then
    notify("buffer has no file", vim.log.levels.WARN)
    return
  end
  if line2 < line1 then
    line1, line2 = line2, line1
  end
  local colors = table.concat(vim.tbl_keys(M.config.colors), "|")
  vim.ui.input({ prompt = ("Section (e.g. 'red - Tests') [%s]: "):format(colors) }, function(input)
    do_add({
      file = file,
      line = line1,
      end_line = line2,
      kind = "section",
      body = input or "",
    }, bufnr)
  end)
end

function M.edit()
  local bufnr, notes = notes_at_cursor()
  pick(notes, function(note)
    open_editor({
      title = note.kind == "section" and "Edit section" or "Edit annotation",
      initial = note.body,
      on_save = function(body)
        do_edit(note, body, vim.api.nvim_buf_get_name(bufnr), bufnr)
      end,
    })
  end)
end

function M.delete()
  local bufnr, notes = notes_at_cursor()
  pick(notes, function(note)
    do_delete(note, vim.api.nvim_buf_get_name(bufnr), bufnr)
  end)
end

function M.toggle()
  local bufnr = vim.api.nvim_get_current_buf()
  local st = state[bufnr]
  if not st then
    M.refresh(bufnr)
    return
  end
  st.visible = not st.visible
  render(bufnr)
end

-- jump_to opens a search result's file at its line.
local function jump_to(r)
  vim.cmd("edit " .. vim.fn.fnameescape(r.repo_root .. "/" .. r.file_path))
  pcall(vim.api.nvim_win_set_cursor, 0, { r.line, 0 })
  vim.cmd("normal! zz")
end

-- run_picker lists annotations (optionally filtered) for selection. With fzf-lua
-- it shows ANSI-tinted section entries; without it, it degrades to the built-in
-- vim.ui.select so the plugin still works on a machine that has no fzf. Each
-- line shows repo/path:line, the body, and the last-edited date.
local function run_picker(title, filter)
  local resp, err = run("search", { query = "" })
  if err then
    notify(err, vim.log.levels.ERROR)
    return
  end

  local items = {} -- { plain, ansi, r }
  for _, r in ipairs(resp.results or {}) do
    if not filter or filter(r) then
      local repo = vim.fn.fnamemodify(r.repo_root, ":t")
      local e = entry_for(r) or { text = "" }
      local flag = r.orphaned and "[orphan] " or ""
      local date = (r.updated_at or ""):sub(1, 10)
      local plain = string.format("%s%s/%s:%d  %s  (%s)", flag, repo, r.file_path, r.line, oneline(e.text), date)
      local label = plain
      if r.kind == "section" then
        local color = parse_section(r.body)
        if color then
          label = ansi(M.config.colors[color], plain)
        end
      end
      items[#items + 1] = { plain = plain, ansi = label, r = r }
    end
  end
  if #items == 0 then
    notify(filter and "no matching annotations" or "no annotations yet")
    return
  end

  local ok, fzf = pcall(require, "fzf-lua")
  if ok then
    local entries, lookup = {}, {}
    for _, it in ipairs(items) do
      entries[#entries + 1] = it.ansi
      lookup[it.plain] = it.r -- fzf --ansi strips codes from the returned line
    end
    fzf.fzf_exec(entries, {
      prompt = title .. "> ",
      fzf_opts = { ["--ansi"] = true },
      actions = {
        ["default"] = function(selected)
          local r = lookup[selected[1]]
          if r then
            jump_to(r)
          end
        end,
      },
    })
  else
    vim.ui.select(items, {
      prompt = title,
      format_item = function(it)
        return it.plain
      end,
    }, function(choice)
      if choice then
        jump_to(choice.r)
      end
    end)
  end
end

function M.list()
  run_picker("Annotations", nil)
end

function M.orphans()
  run_picker("Orphans", function(r)
    return r.orphaned
  end)
end

-- ---------------------------------------------------------------------------
-- Navigation, hover, re-anchor, maintenance
-- ---------------------------------------------------------------------------

local function jump(dir)
  local bufnr = vim.api.nvim_get_current_buf()
  local st = state[bufnr]
  if not st or #st.notes == 0 then
    notify("no annotations in buffer")
    return
  end
  local cur = vim.api.nvim_win_get_cursor(0)[1]
  local lines = {}
  for _, n in ipairs(st.notes) do
    lines[#lines + 1] = n.line
  end
  table.sort(lines)
  local target
  if dir > 0 then
    for _, l in ipairs(lines) do
      if l > cur then
        target = l
        break
      end
    end
    target = target or lines[1] -- wrap to top
  else
    for i = #lines, 1, -1 do
      if lines[i] < cur then
        target = lines[i]
        break
      end
    end
    target = target or lines[#lines] -- wrap to bottom
  end
  pcall(vim.api.nvim_win_set_cursor, 0, { target, 0 })
  vim.cmd("normal! zz")
end

function M.next_note()
  jump(1)
end
function M.prev_note()
  jump(-1)
end

function M.hover()
  local _, notes = notes_at_cursor()
  pick(notes, function(note)
    open_float(note.kind == "section" and "Section" or "Annotation", note.body)
  end)
end

-- reanchor: re-baseline the note(s) under the cursor. If none overlap the
-- cursor, offer the buffer's orphans to re-anchor at the cursor line.
function M.reanchor()
  local bufnr, notes = notes_at_cursor()
  if #notes > 0 then
    pick(notes, function(note)
      do_reanchor(note, bufnr)
    end)
    return
  end
  local st = state[bufnr]
  local orphans = {}
  if st then
    for _, n in ipairs(st.notes) do
      if n.orphaned then
        orphans[#orphans + 1] = n
      end
    end
  end
  if #orphans == 0 then
    notify("no annotation under cursor", vim.log.levels.WARN)
    return
  end
  pick(orphans, function(note)
    do_reanchor(note, bufnr)
  end)
end

-- statusline returns a compact indicator for the buffer (current by default):
-- the note count, plus an orphan marker when any are present. Empty string when
-- the buffer has none — cheap (reads the cache, no engine call) so it is safe to
-- call on every redraw. Drop `require("annotate").statusline()` into lualine etc.
function M.statusline(bufnr)
  bufnr = bufnr or vim.api.nvim_get_current_buf()
  local st = state[bufnr]
  if not st or #st.notes == 0 then
    return ""
  end
  local orphans = 0
  for _, n in ipairs(st.notes) do
    if n.orphaned then
      orphans = orphans + 1
    end
  end
  local s = ("%s %d"):format(M.config.statusline_icon, #st.notes)
  if orphans > 0 then
    s = s .. (" %s%d"):format(M.config.statusline_orphan_icon, orphans)
  end
  return s
end

-- quickfix sends annotations to the quickfix list: the current buffer by
-- default, or every repo when `all` is true (:AnnotateQuickfix!).
function M.quickfix(all)
  local items = {}
  if all then
    local resp, err = run("search", { query = "" })
    if err then
      notify(err, vim.log.levels.ERROR)
      return
    end
    for _, r in ipairs(resp.results or {}) do
      local e = entry_for(r) or { text = "" }
      items[#items + 1] = {
        filename = r.repo_root .. "/" .. r.file_path,
        lnum = r.line,
        text = (r.orphaned and "[orphan] " or "") .. oneline(e.text),
      }
    end
  else
    local bufnr = vim.api.nvim_get_current_buf()
    local file = vim.api.nvim_buf_get_name(bufnr)
    local st = state[bufnr]
    for _, n in ipairs(st and st.notes or {}) do
      local e = entry_for(n) or { text = "" }
      items[#items + 1] = {
        filename = file,
        lnum = n.line,
        text = (n.orphaned and "[orphan] " or "") .. oneline(e.text),
      }
    end
  end
  if #items == 0 then
    notify("no annotations" .. (all and "" or " in buffer"))
    return
  end
  table.sort(items, function(a, b)
    if a.filename == b.filename then
      return a.lnum < b.lnum
    end
    return a.filename < b.filename
  end)
  vim.fn.setqflist({}, " ", { title = "Annotations", items = items })
  vim.cmd("copen")
end

local function human_size(bytes)
  bytes = bytes or 0
  local units = { "B", "KB", "MB", "GB" }
  local n, i = bytes, 1
  while n >= 1024 and i < #units do
    n = n / 1024
    i = i + 1
  end
  if i == 1 then
    return string.format("%d %s", n, units[i])
  end
  return string.format("%.1f %s", n, units[i])
end

-- status shows a read-only dashboard of the whole annotation store.
function M.status()
  local resp, err = run("stats", {})
  if err then
    notify(err, vim.log.levels.ERROR)
    return
  end
  local L = {}
  local function add(s)
    L[#L + 1] = s
  end
  add("# Annotations — status")
  add("")
  add(("Total:     %d   (%d notes, %d sections)"):format(resp.total or 0, resp.notes or 0, resp.sections or 0))
  add(("Orphaned:  %d"):format(resp.orphaned or 0))
  add(("Repos:     %d"):format(resp.repos or 0))
  add(("Missing:   %d%s"):format(resp.missing or 0, (resp.missing or 0) > 0 and "   → :AnnotatePrune" or ""))
  add("")
  if resp.per_repo and #resp.per_repo > 0 then
    add("# Per repo")
    for _, r in ipairs(resp.per_repo) do
      local name = vim.fn.fnamemodify(r.repo_root, ":t")
      local orph = (r.orphaned or 0) > 0 and ("  (%d orphaned)"):format(r.orphaned) or ""
      add(("  %-24s %d%s"):format(name, r.total, orph))
    end
    add("")
  end
  add("# Database")
  add("  " .. (resp.db_path or "?"))
  add("  " .. human_size(resp.db_size))
  open_float("Annotate status", table.concat(L, "\n"))
end

function M.prune()
  local resp, err = run("prune", {})
  if err then
    notify(err, vim.log.levels.ERROR)
    return
  end
  notify(("pruned %d annotation(s) for missing files"):format(resp.count or 0))
  M.refresh()
end

function M.export(path)
  if not path or path == "" then
    notify("usage: :AnnotateExport <path>", vim.log.levels.WARN)
    return
  end
  local resp, err = run("export", {})
  if err then
    notify(err, vim.log.levels.ERROR)
    return
  end
  local f = io.open(vim.fn.expand(path), "w")
  if not f then
    notify("cannot write " .. path, vim.log.levels.ERROR)
    return
  end
  f:write(vim.json.encode(resp))
  f:close()
  notify(("exported %d annotations → %s"):format(#(resp.notes or {}), path))
end

function M.import(path)
  if not path or path == "" then
    notify("usage: :AnnotateImport <path>", vim.log.levels.WARN)
    return
  end
  local f = io.open(vim.fn.expand(path), "r")
  if not f then
    notify("cannot read " .. path, vim.log.levels.ERROR)
    return
  end
  local data = f:read("*a")
  f:close()
  local ok, decoded = pcall(vim.json.decode, data)
  if not ok or type(decoded) ~= "table" then
    notify("invalid export file", vim.log.levels.ERROR)
    return
  end
  local resp, err = run("import", { notes = decoded.notes or {} })
  if err then
    notify(err, vim.log.levels.ERROR)
    return
  end
  notify(("imported %d annotations"):format(resp.imported or 0))
  M.refresh()
end

-- ---------------------------------------------------------------------------
-- Setup
-- ---------------------------------------------------------------------------

local function define_highlights()
  local set = function(name, attrs)
    vim.api.nvim_set_hl(0, name, vim.tbl_extend("keep", { default = true }, attrs))
  end
  local hl = M.config.highlights
  set(hl.note, { link = "Comment" })
  set(hl.orphan, { link = "DiagnosticWarn" })
  set(hl.sign, { link = "Comment" })
  set(hl.orphan_sign, { link = "DiagnosticWarn" })
  set(hl.section, { link = "CursorLine" }) -- default tint follows the theme
  set(hl.section_title, { link = "Title" })
  set(hl.section_bar, { link = "Title" }) -- default bar color (no named color)
  local bg = normal_bg()
  for name, accent in pairs(M.config.colors) do
    set("AnnotateSection_" .. name, { bg = blend(accent, bg, M.config.tint_strength) })
    set("AnnotateSectionTitle_" .. name, { fg = accent, bold = true })
    set("AnnotateSectionBar_" .. name, { fg = accent }) -- vivid gutter bar
  end
end

function M.setup(opts)
  M.config = vim.tbl_deep_extend("force", M.config, opts or {})
  define_highlights()

  local cmd = vim.api.nvim_create_user_command
  cmd("AnnotateAdd", M.add_quick, { desc = "Add a quick one-line annotation" })
  cmd("AnnotateAddBig", M.add, { desc = "Add an annotation in the full editor" })
  cmd("AnnotateSection", function(a)
    M.section(a.line1, a.line2)
  end, { range = true, desc = "Tint the selected lines as a section (+ optional title)" })
  cmd("AnnotateEdit", M.edit, { desc = "Edit the annotation under the cursor" })
  cmd("AnnotateDelete", M.delete, { desc = "Delete the annotation under the cursor" })
  cmd("AnnotateList", M.list, { desc = "Browse all annotations (fzf-lua)" })
  cmd("AnnotateOrphans", M.orphans, { desc = "Browse orphaned annotations (fzf-lua)" })
  cmd("AnnotateHover", M.hover, { desc = "Show the full annotation under the cursor" })
  cmd("AnnotateReanchor", M.reanchor, { desc = "Re-anchor the annotation under the cursor" })
  cmd("AnnotateToggle", M.toggle, { desc = "Toggle annotation text + section tints" })
  cmd("AnnotateUndo", M.undo, { desc = "Undo the last annotation change" })
  cmd("AnnotateRedo", M.redo, { desc = "Redo the last undone annotation change" })
  cmd("AnnotatePrune", M.prune, { desc = "Delete annotations whose file no longer exists" })
  cmd("AnnotateStatus", M.status, { desc = "Show the annotation store dashboard" })
  cmd("AnnotateQuickfix", function(a)
    M.quickfix(a.bang)
  end, { bang = true, desc = "Send annotations to the quickfix list (! = all repos)" })
  cmd("AnnotateExport", function(a)
    M.export(a.args)
  end, { nargs = 1, complete = "file", desc = "Export all annotations to a JSON file" })
  cmd("AnnotateImport", function(a)
    M.import(a.args)
  end, { nargs = 1, complete = "file", desc = "Import annotations from a JSON file" })

  if M.config.nav_keys then
    vim.keymap.set("n", "]a", M.next_note, { desc = "Annotate: next annotation" })
    vim.keymap.set("n", "[a", M.prev_note, { desc = "Annotate: previous annotation" })
  end

  load_history()

  local grp = vim.api.nvim_create_augroup("annotate", { clear = true })
  vim.api.nvim_create_autocmd({ "BufReadPost", "BufEnter" }, {
    group = grp,
    callback = function(args)
      M.refresh(args.buf)
    end,
  })
  vim.api.nvim_create_autocmd("BufWritePost", {
    group = grp,
    callback = function(args)
      if M.config.reanchor_on_save then
        reanchor_buffer(args.buf)
      end
      M.refresh(args.buf)
    end,
  })
  vim.api.nvim_create_autocmd("ColorScheme", { group = grp, callback = define_highlights })
  vim.api.nvim_create_autocmd("BufWipeout", {
    group = grp,
    callback = function(args)
      state[args.buf] = nil
    end,
  })
end

return M
