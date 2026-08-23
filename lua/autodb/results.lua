---autodb results — an exec reply on screen, in a bottom split.
---
---The adapter between the wire and `auto-core.ui.grid`. The server's
---result map already carries everything the grid model wants, so this
---is deliberately thin: name the mapping, own the window, and get out
---of the way.
---
---A bottom SPLIT rather than a float [DECISION — Johno: "bottom split
---will be easier to work with in nvim environment"]. A split keeps
---normal window motions, survives other floats opening over it, and can
---stay visible while you edit the query above it.
---@module 'autodb.results'

local grid = require("auto-core").ui.grid
local model_mod = require("auto-core.ui.grid.model")
local viewer = require("auto-core.ui.float.viewer")
local log = require("autodb.log")

local M = {}

local WINVAR = "autodb_result_window"
local DEFAULT_HEIGHT = 15

---@type AutoCoreGridView|nil
local _view = nil
local _win = nil

---The selection mode is STICKY FOR THE SESSION, and it has to live here
---rather than in the view: `M.show` disposes and re-attaches the grid on
---every query (below), so a mode held by the view would reset each time the
---user ran a statement. auto-core owns the mode as validated view state;
---this side owns the policy of remembering it. Nothing is written to disk
---[DECISION — ADR-0066 §2.7 / r2].
local _sel_mode = "cell"

---The detail views form a two-level stack: a ROOT (a row detail, or a cell
---opened straight from the grid) which may own AT MOST ONE cell child.
---auto-core's viewer knows nothing about rows and cells — it only restores a
---still-valid `opener` — so the parent/child relation is owned here, which is
---the only side that knows what those words mean.
---
---There is ONE root handle, and opening a root closes the previous one. An
---earlier version kept `_row_viewer` and overwrote it: opening a second row
---detail left the first floating on screen with nothing tracking it, and
---`M.close()` then tore down only the newer one. A child is NOT tracked here
---at all — each `open_row` closure owns its own, so a direct-cell root and a
---row's child can never be confused for one another.
local _root = nil
local _root_kind = nil

---from_wire turns an `exec.run` reply into a grid model.
---
---The wire shape (`rpc/methods.go: resultMap`) is columns, rows, verb,
---class, affected, more, duration_ms — which is the model's constructor
---almost exactly. Values arrive RAW, and the grid keeps them that way:
---the display string is derived, never substituted, so a yank still
---reproduces what the database returned.
---@param res table
---@return AutoCoreGridModel
function M.from_wire(res)
  res = res or {}
  return grid.model({
    columns = res.columns or {},
    rows = res.rows or {},
    verb = res.verb,
    affected = tonumber(res.affected),
    duration_ms = tonumber(res.duration_ms),
    more = res.more and true or false,
  })
end

---from_error builds the model for a failed statement.
---
---An error is a RESULT STATE, not an absence of one: the panel shows
---what happened rather than going blank and leaving the user to guess
---whether anything ran.
---@param err table  -- { code, message }
---@return AutoCoreGridModel
function M.from_error(err)
  return grid.model({ error = (err and err.message) or "unknown error" })
end

---_ensure_window returns the result split, creating it if needed.
---
---It is found by a window-local marker rather than remembered by id, so
---a window the user closed and a window they moved are both handled
---without tracking events for either.
local function _ensure_window(height)
  if _win and vim.api.nvim_win_is_valid(_win) then return _win end
  for _, w in ipairs(vim.api.nvim_list_wins()) do
    if vim.w[w][WINVAR] == 1 then
      _win = w
      return w
    end
  end

  local prev = vim.api.nvim_get_current_win()
  vim.cmd("botright " .. tostring(height or DEFAULT_HEIGHT) .. "split")
  local w = vim.api.nvim_get_current_win()
  vim.w[w][WINVAR] = 1
  vim.api.nvim_set_option_value("winfixheight", true, { win = w, scope = "local" })
  -- Return focus: running a query should not move the cursor out of the
  -- buffer the user is writing in.
  pcall(vim.api.nvim_set_current_win, prev)
  _win = w
  return w
end

---show renders a model in the result split.
---@param model AutoCoreGridModel
---@param opts { height: integer?, focus: boolean? }?
---@return AutoCoreGridView|nil
function M.show(model, opts)
  opts = opts or {}
  local win = _ensure_window(opts.height)
  if not win or not vim.api.nvim_win_is_valid(win) then
    log.error("results", "could not open a result window")
    return nil
  end

  -- One view per window: dispose the previous before attaching, so the
  -- old buffer, extmarks, keymaps and autocmds go with it.
  if _view and not _view:disposed() then
    pcall(function() _view:dispose() end)
  end
  _view = grid.attach(model, {
    win = win,
    -- Seeded from session state; auto-core deliberately does NOT fire
    -- on_selection_mode for this initial value, so there is no echo back.
    selection_mode = _sel_mode,
    on_selection_mode = function(mode) _sel_mode = mode end,
    on_inspect = function(cur, m, mode)
      M.inspect(cur, m, mode)
    end,
  })
  if opts.focus then pcall(vim.api.nvim_set_current_win, win) end
  return _view
end

---show_result is the common path: a reply or an error, either way shown.
---@param res table|nil
---@param err table|nil
function M.show_result(res, err)
  if err then return M.show(M.from_error(err)) end
  return M.show(M.from_wire(res))
end

---Register text for one value. NULL yanks EMPTY, which is the rule the grid
---already follows (`view.lua` yank_cell, and `csv_field` writes an empty
---field so a spreadsheet can tell it from the literal text "NULL"). The
---detail views must not invent a second convention.
---@param value any
---@param null boolean
---@return string
local function register_text(value, null)
  if null then return "" end
  return model_mod.raw_text(value, "")
end

---Display lines for one value: the FAITHFUL form, split across buffer lines.
---This is the one place the multi-line shape of a value is supposed to
---appear — everywhere else it is flattened (ADR-0066 §2.4).
---@param value any
---@param null boolean
---@return string[]
local function cell_lines(value, null)
  if null then return { "NULL" } end
  return vim.split(model_mod.raw_text(value, "NULL"), "\n", { plain = true })
end

---open_cell shows ONE value in full.
---
---`opener` is the window focus returns to — the row detail when drilled into
---from there, the grid when `<CR>` opened it directly. `on_closed` lets the
---OWNER (a row detail, or this module when the cell is the root) drop its
---reference; nothing about child bookkeeping lives at module scope.
---@param label string
---@param value any
---@param null boolean
---@param model AutoCoreGridModel
---@param row integer
---@param opener integer|nil
---@param on_closed fun()|nil
function M.open_cell(label, value, null, model, row, opener, on_closed)
  return viewer(cell_lines(value, null), {
    title = label .. " — y: value, Y: row CSV, q: close",
    opener = opener,
    wrap = true,
    keymaps = {
      ["y"] = function(h)
        grid.set_clipboard(register_text(value, null))
        h:close()
      end,
      -- `Y` is absolute EVERYWHERE: the grid in both modes, and inside both
      -- detail views. Only `y` is ever mode- or selection-dependent.
      ["Y"] = function()
        local csv = model and model:csv(row)
        if csv then grid.set_clipboard(csv) end
      end,
    },
    on_close = function() if on_closed then on_closed() end end,
  })
end

---open_row shows one row as a navigable list of its columns.
---
---The list is built from `model:row_entries` and rendered by
---`model.row_detail_lines`, which returns the line→entry MAP. The cursor line
---is resolved through that map, never by reading the rendered text back: both
---a value and a quoted column name may legally contain a newline, and
---recovering a column from text is the defect ADR-0066 removed twice.
---@param model AutoCoreGridModel
---@param row integer
---@param opener integer|nil
function M.open_row(model, row, opener)
  local entries = model and model:row_entries(row)
  if not entries then return nil end
  local lines, map = model_mod.row_detail_lines(entries)

  M.close_details()
  ---This row's OWN child. A local, captured by the keymaps and by on_close
  ---below, so two row details can never share a child slot.
  local child = nil
  local handle
  handle = viewer(lines, {
    title = "row " .. tostring(row) .. " — CR: cell, y: value, Y: row CSV, q: close",
    opener = opener,
    cursorline = true,
    keymaps = {
      ["<CR>"] = function(h)
        local e = map[vim.api.nvim_win_get_cursor(h:win())[1]]
        if not e then return end
        -- At most one child: a second drill REPLACES the first rather than
        -- stacking a third float nobody asked for.
        if child and child:is_open() then child:close() end
        child = M.open_cell(e.label, e.value, e.null, model, row, h:win(),
          function() child = nil end)
      end,
      ["y"] = function(h)
        local e = map[vim.api.nvim_win_get_cursor(h:win())[1]]
        if e then grid.set_clipboard(register_text(e.value, e.null)) end
      end,
      ["Y"] = function()
        local csv = model:csv(row)
        if csv then grid.set_clipboard(csv) end
      end,
    },
    on_close = function()
      -- The cascade: a parent never leaves a child behind, however the parent
      -- died. auto-core fires this on_close for every in-session close —
      -- including `:q` on the window — which is what makes that true.
      if child and child:is_open() then child:close() end
      child = nil
      if _root == handle then _root, _root_kind = nil, nil end
    end,
  })
  _root, _root_kind = handle, "row"
  return handle
end

---close_details tears down the current root (and, through its own on_close,
---any child it owns). Idempotent, and safe to call before opening a new root.
function M.close_details()
  local r = _root
  _root, _root_kind = nil, nil
  if r and r:is_open() then r:close() end
end

---inspect opens the detail view for whatever is SELECTED.
---
---Cell mode goes straight to the value: routing through a row list to reach
---a value the cursor is already on is a wasted keystroke. Row mode opens the
---row, which drills to the same cell view.
---@param cur { row: integer, col: integer, cell: table }
---@param model AutoCoreGridModel|nil
---@param mode string|nil  "cell" (default) | "row"
function M.inspect(cur, model, mode)
  if not (cur and cur.cell) then return end
  model = model or (_view and _view:model())
  local opener = _view and _view:win() or nil

  if mode == "row" and model then
    return M.open_row(model, cur.row, opener)
  end

  local cols = model and model:columns() or {}
  local label = (cols[cur.col] and cols[cur.col].label) or ("column " .. tostring(cur.col))
  -- A cell opened straight from the grid is a ROOT in its own right, so it
  -- replaces whatever root was showing and is what M.close() tears down.
  M.close_details()
  local h
  h = M.open_cell(label, cur.cell.value, cur.cell.null, model, cur.row, opener,
    function() if _root == h then _root, _root_kind = nil, nil end end)
  _root, _root_kind = h, "cell"
  return h
end

---close tears the panel down. Idempotent.
function M.close()
  -- The root cascades to its own child. The sticky mode SURVIVES — closing
  -- the panel is not a decision to forget how the user likes to select.
  M.close_details()
  if _view and not _view:disposed() then pcall(function() _view:dispose() end) end
  _view = nil
  if _win and vim.api.nvim_win_is_valid(_win) then
    pcall(vim.api.nvim_win_close, _win, true)
  end
  _win = nil
end

function M.view() return _view end
function M.window() return _win end

---reset_for_tests drops state without touching windows.
function M.reset_for_tests()
  _view, _win = nil, nil
  _root, _root_kind = nil, nil
  _sel_mode = "cell"
end

---selection_mode exposes the sticky mode, for tests and for a status line.
function M.selection_mode() return _sel_mode end

---detail_root is the current top-level detail view, and what kind it is.
---Children are deliberately NOT exposed: they are owned by the root that
---opened them, not by this module.
---@return table|nil handle, string|nil kind
function M.detail_root() return _root, _root_kind end

return M
