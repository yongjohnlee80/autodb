---autodb history — who ran what, when (ADR-0058 §3.5, requirement 8).
---
---Three panes on `auto-core.ui.float.multi`, the same shape
---`auto-core.log.viewer` uses, because the problem is the same one:
---a list to narrow by, the entries themselves, and the full text of
---whichever entry the cursor is on.
---
---  ┌ Connections ─┬ Entries ──────────────┬ Script ─────────┐
---  │ (all)        │ date · user · script  │ the full SQL,   │
---  │ analytics    │ …                     │ untruncated     │
---  └──────────────┴───────────────────────┴─────────────────┘
---
---Entries are ELLIPSED in the middle pane and shown in full on the
---right, because a script is many lines and a list row is one — the
---same reason the M6 TUI grew a detail view for result rows.
---
---History is the server's, not ours: `history.list` returns what the
---caller is allowed to see (admins everything, everyone else their own),
---so this view renders a reply rather than filtering a global log.
---@module 'autodb.history'

local keys = require("autodb.keys")
local log = require("autodb.log")
local mfloat = require("auto-core.ui.float.multi")
local session = require("autodb.session")

local M = {}

local NAME = "autodb-history"
local DEFAULT_LIMIT = 200
local ALL = "(all connections)"

---@type table|nil
local _state = nil

-- ─── rendering ────────────────────────────────────────────────

local function _fmt_when(iso)
  -- "2026-08-18T09:14:03+09:00" -> "08-18 09:14". The year is almost
  -- always this one and the offset is never what you are scanning for.
  local mon, day, hh, mm = tostring(iso):match("%d+%-(%d+)%-(%d+)T(%d+):(%d+)")
  if not mon then return tostring(iso) end
  return string.format("%s-%s %s:%s", mon, day, hh, mm)
end

local function _one_line(script)
  local s = tostring(script or ""):gsub("%s+", " ")
  return vim.trim(s)
end

---_connections lists the distinct connections present, most-used first.
---
---Derived from the entries rather than fetched: the filter should offer
---what the history actually contains, not every connection that exists.
local function _connections(entries)
  local counts, order = {}, {}
  for _, e in ipairs(entries) do
    local name = e.connection or ("#" .. tostring(e.connection_id))
    if not counts[name] then
      counts[name] = 0
      order[#order + 1] = name
    end
    counts[name] = counts[name] + 1
  end
  table.sort(order, function(a, b)
    if counts[a] ~= counts[b] then return counts[a] > counts[b] end
    return a < b
  end)
  local out = { { label = ALL, key = nil, count = #entries } }
  for _, name in ipairs(order) do
    out[#out + 1] = { label = name, key = name, count = counts[name] }
  end
  return out
end

local function _filtered(state)
  local sel = state.conns[state.conn_idx]
  if not sel or sel.key == nil then return state.entries end
  local out = {}
  for _, e in ipairs(state.entries) do
    local name = e.connection or ("#" .. tostring(e.connection_id))
    if name == sel.key then out[#out + 1] = e end
  end
  return out
end

local function _set_lines(bufnr, lines)
  if not (bufnr and vim.api.nvim_buf_is_valid(bufnr)) then return end
  vim.bo[bufnr].modifiable = true
  vim.api.nvim_buf_set_lines(bufnr, 0, -1, false, lines)
  vim.bo[bufnr].modifiable = false
end

local function _redraw(state)
  local panes = state.float.panes or {}

  -- Left: the connection filter.
  local left = {}
  for i, c in ipairs(state.conns) do
    left[i] = string.format("%s %s (%d)", i == state.conn_idx and "▸" or " ",
      c.label, c.count)
  end
  _set_lines(panes.left and panes.left.bufnr, left)

  -- Middle: date · user · status · one-line script.
  state.rows = _filtered(state)
  local mid = {}
  if #state.rows == 0 then
    mid[1] = "  (no history)"
  else
    for i, e in ipairs(state.rows) do
      local mark = (e.status == "error" or (e.error and e.error ~= "")) and "✗" or "·"
      mid[i] = string.format("%s %s  %-12s %s", mark, _fmt_when(e.started_at),
        tostring(e.user or e.user_id or "?"), _one_line(e.script))
    end
  end
  _set_lines(panes.middle and panes.middle.bufnr, mid)

  -- Right: the entry under the cursor, in full.
  M._preview(state)
end

---_preview paints the full script for the row under the cursor.
function M._preview(state)
  local panes = state.float.panes or {}
  local pane = panes.preview
  if not (pane and pane.bufnr) then return end
  local mid = panes.middle
  local lnum = 1
  if mid and mid.winid and vim.api.nvim_win_is_valid(mid.winid) then
    lnum = vim.api.nvim_win_get_cursor(mid.winid)[1]
  end
  local e = state.rows and state.rows[lnum]
  if not e then
    _set_lines(pane.bufnr, { "" })
    return
  end
  local lines = {
    "connection: " .. tostring(e.connection or e.connection_id or "?"),
    "user:       " .. tostring(e.user or e.user_id or "?"),
    "when:       " .. tostring(e.started_at or "?"),
    string.format("outcome:    %s · %s row(s) · %sms",
      tostring(e.status or "?"), tostring(e.row_count or 0), tostring(e.duration_ms or 0)),
  }
  if e.error and e.error ~= "" then
    lines[#lines + 1] = "error:      " .. tostring(e.error)
  end
  lines[#lines + 1] = ""
  for _, l in ipairs(vim.split(tostring(e.script or ""), "\n", { plain = true })) do
    lines[#lines + 1] = l
  end
  _set_lines(pane.bufnr, lines)
  if pane.bufnr and vim.api.nvim_buf_is_valid(pane.bufnr) then
    vim.bo[pane.bufnr].filetype = "sql"   -- the script is SQL; treat it as such
  end
end

-- ─── keymaps ──────────────────────────────────────────────────

local function _map(bufnr, lhs, fn, desc)
  if not (bufnr and vim.api.nvim_buf_is_valid(bufnr)) then return end
  pcall(vim.keymap.set, "n", lhs, fn, {
    buffer = bufnr, silent = true, nowait = true, desc = desc,
  })
end

---_bind wires the panes.
---
---`<CR>` on an entry YANKS its script into a scratch buffer in the
---editor area rather than running it. Re-running someone else's
---statement from a history browser should be a deliberate act in a
---buffer you can read first, not a keystroke in a list.
local function _bind(state)
  local panes = state.float.panes or {}

  local function move_conn(delta)
    state.conn_idx = math.max(1, math.min(#state.conns, state.conn_idx + delta))
    _redraw(state)
  end
  if panes.left and panes.left.bufnr then
    _map(panes.left.bufnr, "j", function() move_conn(1) end, "autodb history: next connection")
    _map(panes.left.bufnr, "k", function() move_conn(-1) end, "autodb history: previous connection")
    _map(panes.left.bufnr, "<CR>", function() state.float:focus("middle") end,
      "autodb history: focus entries")
  end

  if panes.middle and panes.middle.bufnr then
    _map(panes.middle.bufnr, "<CR>", function() M.open_in_editor(state) end,
      "autodb history: open this script in a buffer")
    _map(panes.middle.bufnr, "y", function() M.yank(state) end,
      "autodb history: yank this script")
    _map(panes.middle.bufnr, "R", function() M.reload() end,
      "autodb history: reload")
  end

  -- The preview follows the middle pane's cursor, which is what makes
  -- this a browser rather than a list you have to poke.
  state._augroup = vim.api.nvim_create_augroup("AutodbHistoryCursor", { clear = true })
  if panes.middle and panes.middle.bufnr then
    vim.api.nvim_create_autocmd("CursorMoved", {
      group = state._augroup,
      buffer = panes.middle.bufnr,
      callback = function() M._preview(state) end,
    })
  end
end

-- ─── actions ──────────────────────────────────────────────────

local function _current(state)
  local panes = state.float.panes or {}
  local mid = panes.middle
  if not (mid and mid.winid and vim.api.nvim_win_is_valid(mid.winid)) then return nil end
  return state.rows and state.rows[vim.api.nvim_win_get_cursor(mid.winid)[1]]
end

---yank copies the script under the cursor.
function M.yank(state)
  local e = _current(state or _state)
  if not e then return end
  vim.fn.setreg('"', e.script or "")
  local cb = vim.o.clipboard or ""
  if cb:find("unnamedplus", 1, true) then vim.fn.setreg("+", e.script or "") end
  log.notify("script yanked", { component = "history" })
end

---open_in_editor puts the script in a scratch SQL buffer.
---
---Deliberately NOT a run. A history browser that executes on `<CR>`
---turns a glance at what someone else did into doing it again.
function M.open_in_editor(state)
  state = state or _state
  local e = _current(state)
  if not e then return end
  state.float:close()
  vim.schedule(function()
    vim.cmd("new")
    local buf = vim.api.nvim_get_current_buf()
    vim.bo[buf].filetype = "sql"
    vim.bo[buf].bufhidden = "wipe"
    vim.api.nvim_buf_set_lines(buf, 0, -1, false,
      vim.split(tostring(e.script or ""), "\n", { plain = true }))
    log.notify("loaded into a scratch buffer — " .. keys.RUN_BUFFER .. " runs it",
      { component = "history" })
  end)
end

-- ─── lifecycle ────────────────────────────────────────────────

function M.is_open()
  return _state ~= nil and _state.float ~= nil
end

---open fetches the history and shows it.
---@param opts { limit: integer? }?
function M.open(opts)
  opts = opts or {}
  if M.is_open() then return M.close() end   -- toggle

  session.authed("history.list", { opts.limit or DEFAULT_LIMIT },
    session.guarded(function(entries, err)
      if err then
        return log.notify("cannot list history: " .. tostring(err.message),
          { level = "error", component = "history" })
      end
      entries = entries or {}
      if #entries == 0 then
        return log.notify("no script history yet", { component = "history" })
      end

      local state = {
        entries = entries,
        conns = _connections(entries),
        conn_idx = 1,
        rows = entries,
      }
      state.float = mfloat.new({
        name = NAME,
        outer = { width_pct = 0.9, height_pct = 0.85, title = " autodb — script history " },
        panes = {
          left    = { width = 28, title = " Connections ", cursorline = true },
          middle  = { title = " Entries ", cursorline = true },
          preview = { width = 64, min_width = 34, min_middle = 30, title = " Script " },
        },
        initial_focus = "middle",
        on_close = function()
          if _state and _state._augroup then
            pcall(vim.api.nvim_del_augroup_by_id, _state._augroup)
          end
          _state = nil
        end,
      })
      state.float:open()
      _state = state
      _redraw(state)
      _bind(state)
    end))
end

function M.close()
  if _state and _state.float then _state.float:close() end
end

---reload refetches without closing, so `R` keeps your place in the list
---rather than dropping you back at the top of a new modal.
function M.reload()
  local limit = DEFAULT_LIMIT
  if not M.is_open() then return M.open({ limit = limit }) end
  session.authed("history.list", { limit }, session.guarded(function(entries, err)
    if err or not _state then return end
    _state.entries = entries or {}
    _state.conns = _connections(_state.entries)
    _state.conn_idx = math.min(_state.conn_idx, #_state.conns)
    _redraw(_state)
  end))
end

-- Test hooks.
M._state_for_tests = function() return _state end
M._connections_for_tests = _connections
M._fmt_when_for_tests = _fmt_when
M._one_line_for_tests = _one_line
M._filtered_for_tests = _filtered

return M
