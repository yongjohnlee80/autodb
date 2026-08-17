---autodb commands — the `<leader>D` surface (ADR-0058 §3.6).
---
---Thin by design. Every command resolves what it needs, asks the
---session to do the work, and hands the outcome to a display surface.
---No command talks to the transport, holds a token, or decides which
---connection is active — that is `autodb.session`'s job, and two
---commands answering it independently is how they disagree.
---
---  <leader>Dh   history
---  <leader>Dr   run the current buffer (must be SQL)
---  <leader>DR   run the visual selection
---  <leader>Dc   choose a connection (workspace, then connection)
---  <leader>DX   maintenance: restart / refresh / reset
---
---Key strings come from `autodb.keys`, so a message that tells the user
---which key to press cannot drift from the key that is bound.
---@module 'autodb.commands'

local keys = require("autodb.keys")
local log = require("autodb.log")
local results = require("autodb.results")
local session = require("autodb.session")

local M = {}

---@type fun(cb: fun(ok: boolean, err: string|nil))|nil
M._ensure_connected = nil

---_connected runs `fn` with a live session, connecting first if needed.
---
---Every command funnels through here so "not connected" is handled in
---ONE place. A command that checked for itself would each invent its own
---answer to "and what should the user see now?".
local function _connected(fn)
  if session.is_ready() then return fn() end
  if not M._ensure_connected then
    return log.notify("autodb is not set up — call require('autodb').setup()",
      { level = "warn", component = "commands" })
  end
  M._ensure_connected(function(ok, err)
    if not ok then
      return log.notify(tostring(err), { level = "error", component = "commands" })
    end
    fn()
  end)
end

---_with_connection ensures a connection is SELECTED, prompting if not.
---
---Requirement 12: executing with nothing selected prompts rather than
---failing. The prompt is the same one `<leader>Dc` opens, so the user
---learns one flow instead of two.
local function _with_connection(fn)
  _connected(function()
    local conn = session.connection()
    if conn then return fn(conn) end
    log.info("commands", "no connection selected; prompting")
    M.choose_connection(function(chosen)
      if chosen then fn(chosen) end
    end)
  end)
end

-- ─── running SQL ──────────────────────────────────────────────

---_is_sql reports whether the buffer is SQL.
---
---Requirement 9 says `<leader>Dr` runs a `.sql` file. Checked by
---filetype OR extension: a scratch `noname.sql` may not have had its
---filetype set yet, and refusing to run the user's own scratch buffer
---because of an autocmd race would be a bad first impression.
---@param buf integer
---@return boolean
function M.is_sql(buf)
  if vim.bo[buf].filetype == "sql" then return true end
  local name = vim.api.nvim_buf_get_name(buf)
  return name:sub(-4):lower() == ".sql"
end

---_visual_selection returns the last visual selection's text.
local function _visual_selection()
  local s = vim.fn.getpos("'<")
  local e = vim.fn.getpos("'>")
  local lines = vim.api.nvim_buf_get_lines(0, s[2] - 1, e[2], false)
  if #lines == 0 then return "" end
  -- Clamp the ends to the selected columns; a charwise selection that
  -- covers part of a line must not run the whole line.
  local last = math.min(e[3], #lines[#lines])
  lines[#lines] = lines[#lines]:sub(1, last)
  lines[1] = lines[1]:sub(s[3])
  return table.concat(lines, "\n")
end

---run_sql executes `sql` against the active connection.
---
---`exec.run_script` rather than `exec.run`: a buffer or selection is a
---SCRIPT, and the server splits and runs each statement through the same
---guarded path, returning the last result (the M6 behaviour Johno asked
---for — "we should still execute then display whatever comes the last").
---@param sql string
function M.run_sql(sql)
  sql = vim.trim(sql or "")
  if sql == "" then
    return log.notify("nothing to run", { level = "warn", component = "commands" })
  end
  _with_connection(function(conn)
    log.debug("commands", "running against connection " .. tostring(conn.id))
    session.authed("exec.run_script", { conn.id, sql }, session.guarded(function(res, err)
      results.show_result(res, err)
      if err then
        log.notify("query failed: " .. tostring(err.message),
          { level = "error", component = "commands" })
      end
    end))
  end)
end

---run_buffer runs the current buffer (requirement 9).
function M.run_buffer()
  local buf = vim.api.nvim_get_current_buf()
  if not M.is_sql(buf) then
    return log.notify(
      "this is not a .sql buffer — " .. keys.RUN_BUFFER .. " runs SQL files",
      { level = "warn", component = "commands" })
  end
  M.run_sql(table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n"))
end

---run_selection runs the visual selection (requirement 10).
---
---No filetype check: selecting SQL inside a Go string or a markdown
---fence is a legitimate thing to do, and the user has already been
---explicit about what they meant by selecting it.
function M.run_selection()
  M.run_sql(_visual_selection())
end

-- ─── choosing a connection ────────────────────────────────────

---choose_connection prompts for a workspace, then a connection
---(requirement 11).
---
---`workspace.list` embeds each workspace's connections, so both prompts
---come from one request — and a workspace with exactly one connection
---skips the second prompt entirely, because asking a question with one
---answer is just a keystroke tax.
---@param cb fun(conn: table|nil)?
function M.choose_connection(cb)
  _connected(function()
    session.authed("workspace.list", {}, session.guarded(function(spaces, err)
      if err then
        log.notify("cannot list workspaces: " .. tostring(err.message),
          { level = "error", component = "commands" })
        return cb and cb(nil)
      end
      spaces = spaces or {}
      if #spaces == 0 then
        log.notify("no workspaces yet — create one in the TUI (autodb --ui)",
          { level = "warn", component = "commands" })
        return cb and cb(nil)
      end

      local function pick_conn(ws)
        local conns = ws.connections or {}
        if #conns == 0 then
          log.notify("workspace " .. tostring(ws.name) .. " has no connections",
            { level = "warn", component = "commands" })
          return cb and cb(nil)
        end
        local function adopt(conn)
          session.select_workspace(ws)
          session.select_connection(conn)
          log.notify(string.format("connected to %s · %s", ws.name or "?", conn.name or "?"),
            { component = "commands" })
          if cb then cb(conn) end
        end
        if #conns == 1 then return adopt(conns[1]) end
        vim.ui.select(conns, {
          prompt = "autodb — connection in " .. tostring(ws.name),
          format_item = function(c)
            return string.format("%s  (%s)", c.name or c.id, c.engine or "?")
          end,
        }, function(chosen)
          if not chosen then return cb and cb(nil) end
          adopt(chosen)
        end)
      end

      if #spaces == 1 then return pick_conn(spaces[1]) end
      vim.ui.select(spaces, {
        prompt = "autodb — workspace",
        format_item = function(w)
          return string.format("%s  (%d connection%s)", w.name or w.id,
            #(w.connections or {}), #(w.connections or {}) == 1 and "" or "s")
        end,
      }, function(ws)
        if not ws then return cb and cb(nil) end
        pick_conn(ws)
      end)
    end))
  end)
end

-- ─── maintenance ──────────────────────────────────────────────

---maintenance is the `<leader>DX` prompt (requirement 13).
---
---A prompt rather than a single destructive action, and the wording
---names what each choice does to the SERVER, because two of the three
---affect every other frontend sharing this daemon.
function M.maintenance()
  local choices = {
    { key = "restart", label = "Restart the backend (cancels running statements)" },
    { key = "refresh", label = "Refresh autodb (refetch and rebuild the binary)" },
    { key = "reset", label = "Reset the database (DESTRUCTIVE — moves the meta store aside)" },
  }
  vim.ui.select(choices, {
    prompt = "autodb maintenance",
    format_item = function(c) return c.label end,
  }, function(choice)
    if not choice then return end
    if choice.key == "restart" then return M.restart() end
    log.notify(choice.key .. " is not implemented yet — run `autodb --serve` "
      .. "yourself, or rebuild the binary, until it lands",
      { level = "warn", component = "commands" })
  end)
end

---restart asks the SERVER to stop; it decides and drains (§3.7.3).
---
---The frontend has no kill path here: it sends an admin-gated request
---and waits for the connection to end. In-flight statements are
---CANCELLED rather than completed — stated in the prompt above, because
---the user should learn that before pressing it, not from the audit log
---afterwards.
function M.restart()
  _connected(function()
    session.authed("sys.shutdown", {}, function(_, err)
      if err then
        return log.notify("restart refused: " .. tostring(err.message),
          { level = "error", component = "commands" })
      end
      log.notify("backend is draining; it will restart on the next request",
        { component = "commands" })
      session.detach("restart")
    end)
  end)
end

---history opens the script-history modal (requirement 8).
function M.history()
  _connected(function()
    local ok, hist = pcall(require, "autodb.history")
    if ok and hist and hist.open then return hist.open() end
    log.notify("the history modal is not available in this build",
      { level = "warn", component = "commands" })
  end)
end

-- ─── wiring ───────────────────────────────────────────────────

---setup binds the keymaps and user commands. Idempotent.
---@param opts { ensure_connected: fun(cb)?, keys: table? }?
function M.setup(opts)
  opts = opts or {}
  M._ensure_connected = opts.ensure_connected or M._ensure_connected

  local map = opts.keys or {}
  local function set(mode, lhs, fn, desc)
    if lhs == false then return end   -- a consumer may disable one
    pcall(vim.keymap.set, mode, lhs, fn, { silent = true, desc = desc })
  end

  set("n", map.history or keys.HISTORY, M.history, "autodb: script history")
  set("n", map.run_buffer or keys.RUN_BUFFER, M.run_buffer, "autodb: run this SQL buffer")
  set("v", map.run_visual or keys.RUN_VISUAL, function()
    -- Leave visual mode first so '< and '> are set.
    vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes("<Esc>", true, false, true), "nx", false)
    M.run_selection()
  end, "autodb: run the selection")
  set("n", map.connection or keys.CONNECTION, function() M.choose_connection() end,
    "autodb: choose a connection")
  set("n", map.maintenance or keys.MAINTENANCE, M.maintenance,
    "autodb: maintenance (restart / refresh / reset)")

  vim.api.nvim_create_user_command("AutodbRun", function(a)
    if a.range > 0 then return M.run_selection() end
    M.run_buffer()
  end, { range = true, desc = "autodb: run the buffer or selection" })
  vim.api.nvim_create_user_command("AutodbConnection", function() M.choose_connection() end,
    { desc = "autodb: choose a connection" })
  vim.api.nvim_create_user_command("AutodbHistory", M.history, { desc = "autodb: script history" })
  vim.api.nvim_create_user_command("AutodbMaintenance", M.maintenance,
    { desc = "autodb: maintenance prompt" })
end

M._visual_selection_for_tests = _visual_selection

return M
