-- annotate.nvim — LazyVim plugin spec.
--
-- Copy this into e.g. ~/.config/nvim/lua/plugins/annotate.lua (it must `return`
-- the table). This file is kept in sync with the plugin's commands/options.
--
-- NOTE: LazyVim binds <leader>n to "Notification History", which shadows this
-- plugin's <leader>n* group (and adds a timeout before each chord). Free it in
-- ~/.config/nvim/lua/config/keymaps.lua:
--
--     pcall(vim.keymap.del, "n", "<leader>n")
--     vim.keymap.set("n", "<leader>sn", function()
--       require("snacks").notifier.show_history()
--     end, { desc = "Notification History" })

-- This file returns a LIST of plugin specs: the plugin itself, plus an optional
-- lualine component. (A plugins file may return one spec or a list of them.)
return {
  {
  dir = "/home/aleix/Projects/notes", -- local path to this repo
  dependencies = { "ibhagwan/fzf-lua" }, -- LazyVim already has this if fzf is your picker
  build = "go build -o nvim-annotate ./cmd/nvim-annotate", -- compiles into the repo dir
  -- load on file open + when a command/key is used
  event = { "BufReadPost", "BufNewFile" },
  cmd = {
    "AnnotateAdd",
    "AnnotateAddBig",
    "AnnotateSection",
    "AnnotateEdit",
    "AnnotateDelete",
    "AnnotateHover",
    "AnnotateReanchor",
    "AnnotateList",
    "AnnotateOrphans",
    "AnnotateToggle",
    "AnnotateUndo",
    "AnnotateRedo",
    "AnnotatePrune",
    "AnnotateExport",
    "AnnotateImport",
    "AnnotateStatus",
    "AnnotateQuickfix",
  },
  keys = {
    { "<leader>na", "<cmd>AnnotateAdd<cr>", desc = "Annotate: quick note" },
    { "<leader>nA", "<cmd>AnnotateAddBig<cr>", desc = "Annotate: note (full editor)" },
    { "<leader>ns", ":AnnotateSection<cr>", mode = "x", desc = "Annotate: section from selection" },
    { "<leader>ne", "<cmd>AnnotateEdit<cr>", desc = "Annotate: edit" },
    { "<leader>nd", "<cmd>AnnotateDelete<cr>", desc = "Annotate: delete" },
    { "<leader>nk", "<cmd>AnnotateHover<cr>", desc = "Annotate: hover (full body)" },
    { "<leader>nR", "<cmd>AnnotateReanchor<cr>", desc = "Annotate: re-anchor / fix orphan" },
    { "<leader>nl", "<cmd>AnnotateList<cr>", desc = "Annotate: list (all repos)" },
    { "<leader>no", "<cmd>AnnotateOrphans<cr>", desc = "Annotate: orphans" },
    { "<leader>nt", "<cmd>AnnotateToggle<cr>", desc = "Annotate: toggle" },
    { "<leader>nu", "<cmd>AnnotateUndo<cr>", desc = "Annotate: undo" },
    { "<leader>nr", "<cmd>AnnotateRedo<cr>", desc = "Annotate: redo" },
    { "<leader>nP", "<cmd>AnnotatePrune<cr>", desc = "Annotate: prune missing-file notes" },
    { "<leader>nS", "<cmd>AnnotateStatus<cr>", desc = "Annotate: status dashboard" },
    { "<leader>nq", "<cmd>AnnotateQuickfix<cr>", desc = "Annotate: quickfix (buffer)" },
    { "<leader>nQ", "<cmd>AnnotateQuickfix!<cr>", desc = "Annotate: quickfix (all repos)" },
    -- ]a / [a (next/previous annotation) are bound automatically by nav_keys.
  },
  opts = {
    -- point at the binary lazy just built (absolute path = no PATH needed)
    bin = "/home/aleix/Projects/notes/nvim-annotate",

    -- ---- optional tuning (defaults shown) ----
    -- section_style = "bar",     -- "bar" (gutter stripe) | "tint" | "both"
    -- tint_strength = 0.22,      -- block-tint strength when style uses "tint"
    -- reanchor_on_save = false,  -- re-baseline a file's notes on every :w
    -- nav_keys = true,           -- bind ]a / [a to jump between annotations
    -- max_history = 100,         -- persisted undo/redo depth
    -- statusline_icon = "✎",     -- prefix for the statusline note count
    -- statusline_orphan_icon = "⚠",
    -- colors = { blue = "#7aa2f7" }, -- override / add section accent colors
  },
  config = function(_, opts)
    require("annotate").setup(opts)
  end,
  },

  -- Optional: show the current buffer's note/orphan count in lualine. This
  -- MERGES into LazyVim's existing lualine config (it does not replace it), and
  -- only applies if lualine is installed (`optional = true`).
  {
    "nvim-lualine/lualine.nvim",
    optional = true,
    opts = function(_, opts)
      table.insert(opts.sections.lualine_x, 1, {
        function()
          return require("annotate").statusline()
        end,
      })
    end,
  },
}
