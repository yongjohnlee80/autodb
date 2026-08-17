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
local log = require("autodb.log")

local M = {}

local WINVAR = "autodb_result_window"
local DEFAULT_HEIGHT = 15

---@type AutoCoreGridView|nil
local _view = nil
local _win = nil

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
    on_inspect = function(cell)
      M.inspect(cell)
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

---inspect opens one cell in a float — the `<CR>` detail view.
---
---Full value, not the truncated grid text: the point of the detail view
---is the part the column was too narrow to show.
---@param cur { row: integer, col: integer, cell: table }
function M.inspect(cur)
  if not (cur and cur.cell) then return end
  local text = cur.cell.null and "NULL"
    or require("auto-core.ui.grid.model").raw_text(cur.cell.value, "NULL")
  local lines = vim.split(tostring(text), "\n", { plain = true })
  local float = require("auto-core").ui.float
  if float and float.help_overlay then
    pcall(float.help_overlay, { title = "cell", lines = lines })
  else
    log.notify(table.concat(lines, "\n"), { component = "results" })
  end
end

---close tears the panel down. Idempotent.
function M.close()
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
end

return M
