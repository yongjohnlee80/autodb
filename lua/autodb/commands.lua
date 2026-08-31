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
---  <leader>DX   maintenance: restart / refresh
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
---_connected runs fn once a session is live, and otherwise REPORTS.
---
---It used to log the failure and return, which left the caller's
---callback dangling forever: `run_sql(sql, cb)` never called cb when
---the daemon could not be reached (lector impl-r0 MF3). An operation
---that cannot complete has to say so, or a caller cannot sequence
---anything after it.
---@param fn fun()
---@param on_fail fun(err: autodb.ApiError)?
local function _connected(fn, on_fail)
  local function fail(code, msg, cause)
    log.notify(msg, { level = "error", component = "commands" })
    if on_fail then on_fail({ code = code, message = msg, cause = cause }) end
  end
  if session.is_ready() then return fn() end
  if not M._ensure_connected then
    return fail("invalid", "autodb is not set up — call require('autodb').setup()")
  end
  M._ensure_connected(function(ok, err)
    if not ok then
      return fail("not_connected", tostring(err), err)
    end
    fn()
  end)
end

---_with_connection ensures a connection is SELECTED, prompting if not.
---
---Requirement 12: executing with nothing selected prompts rather than
---failing. The prompt is the same one `<leader>Dc` opens, so the user
---learns one flow instead of two.
---@param fn fun(conn: table)
---@param on_fail fun(err: autodb.ApiError)?
local function _with_connection(fn, on_fail)
  _connected(function()
    local conn = session.connection()
    if conn then return fn(conn) end
    log.info("commands", "no connection selected; prompting")
    M.choose_connection(function(chosen, cerr)
      if chosen then return fn(chosen) end
      -- Declining the prompt is a cancellation; a picker that FAILED
      -- reports its own error and must not be relabelled (MF3).
      if on_fail then
        on_fail(cerr or { code = "cancelled", message = "no connection chosen" })
      end
    end)
  end, on_fail)
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
---
---Since protocol 5 the server also decides how to RUN it. A script that
---contains a transaction boundary runs inside ONE transaction, so
---`BEGIN; …; COMMIT;` typed into the editor means what it looks like and a
---failure part-way applies nothing. A script without a boundary is
---unchanged: independent statements, and the ones before a failure have
---already run. Nothing here needs to know which — the distinction is the
---server's, so all three editors get it from one place.
---@param sql string
function M.run_sql(sql, cb)
  sql = vim.trim(sql or "")
  if sql == "" then
    log.notify("nothing to run", { level = "warn", component = "commands" })
    if cb then cb(false, { code = "invalid", message = "nothing to run" }) end
    return
  end
  _with_connection(function(conn)
    log.debug("commands", "running against connection " .. tostring(conn.id))
    session.authed("exec.run_script", { conn.id, sql }, session.guarded(function(res, err)
      if err then
        results.show_result(nil, err)
        log.notify("query failed: " .. tostring(err.message),
          { level = "error", component = "commands" })
        if cb then cb(false, { code = "daemon", message = tostring(err.message), cause = err }) end
        return
      end
      -- exec.run_script wraps the LAST statement's result in an envelope
      -- { statements = N, result = {...} }. The grid wants the inner
      -- result; handing it the envelope made a SELECT render as the
      -- write summary ("0 row(s) affected") because columns/rows sat one
      -- level down. A script with no result set (pure DDL) still reports
      -- that it ran.
      local inner = type(res) == "table" and res.result or nil
      if inner then
        results.show_result(inner, nil)
      else
        local n = type(res) == "table" and tonumber(res.statements) or nil
        log.notify((n and (n .. " statement(s)") or "executed") .. " — no result set",
          { component = "commands" })
      end
      -- The envelope is reported verbatim: `statements` plus the LAST
      -- statement's result, which is nil for pure DDL (ADR-0078 §3.6).
      if cb then
        cb(true, {
          statements = type(res) == "table" and tonumber(res.statements) or nil,
          result = inner,
        })
      end
    end))
  end, function(e) if cb then cb(false, e) end end)
end

---run_buffer runs the current buffer (requirement 9).
function M.run_buffer(cb)
  local buf = vim.api.nvim_get_current_buf()
  if not M.is_sql(buf) then
    log.notify(
      "this is not a .sql buffer — " .. keys.RUN_BUFFER .. " runs SQL files",
      { level = "warn", component = "commands" })
    if cb then cb(false, { code = "invalid", message = "not a .sql buffer" }) end
    return
  end
  M.run_sql(table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n"), cb)
end

---run_selection runs the visual selection (requirement 10).
---
---No filetype check: selecting SQL inside a Go string or a markdown
---fence is a legitimate thing to do, and the user has already been
---explicit about what they meant by selecting it.
function M.run_selection(cb)
  M.run_sql(_visual_selection(), cb)
end

-- ─── choosing a connection ────────────────────────────────────

---choose_connection prompts for a workspace, then a connection
---(requirement 11).
---
---`workspace.list` embeds each workspace's connections, so both prompts
---come from one request — and a workspace with exactly one connection
---skips the second prompt entirely, because asking a question with one
---answer is just a keystroke tax.
---_open_note_file opens a note path in a normal editor window (never the
---dbase panel). Notes are real files, so :w saves them in place.
---@param path string
function M._open_note_file(path)
  local esc = vim.fn.fnameescape(path)
  -- Prefer a listed, normal-buftype window that is not a side panel.
  for _, win in ipairs(vim.api.nvim_list_wins()) do
    local buf = vim.api.nvim_win_get_buf(win)
    if vim.bo[buf].buftype == "" and vim.bo[buf].buflisted then
      vim.api.nvim_set_current_win(win)
      pcall(vim.cmd, "edit " .. esc)
      return
    end
  end
  -- No ordinary window (e.g. focus is in the panel): make one.
  pcall(vim.cmd, "botright vsplit " .. esc)
end

---choose_note lists the workspace's notes and can create one
---(`<leader>Dn`). Notes are the workspace's SQL scratch files; selecting
---one opens it in the editor, Create makes a new one. It resolves the
---active workspace, or asks which one when none is selected yet.
---@param cb fun(path: string|nil)?
function M.choose_note(cb)
  local notes = require("autodb.notes")
  local function in_ws(ws)
    local items = notes.list(ws.id)
    local function create_new()
      vim.ui.input({ prompt = "autodb — new note name: " }, function(name)
        name = name and vim.trim(name) or ""
        if name == "" then return cb and cb(nil) end
        local path, err = notes.create(ws.id, name, "")
        if not path then
          log.notify(tostring(err), { level = "error", component = "commands" })
          return cb and cb(nil, { code = "daemon", message = tostring(err), cause = err })
        end
        M._open_note_file(path)
        -- A TABLE, per ADR-0078 §3.6 — a bare string cannot grow a field
        -- and gives a caller nothing to branch on.
        if cb then cb({ path = path }) end
      end)
    end
    if #items == 0 then return create_new() end
    local CREATE = {}
    local list = {}
    for _, n in ipairs(items) do list[#list + 1] = n end
    list[#list + 1] = CREATE
    vim.ui.select(list, {
      prompt = "autodb — note in " .. tostring(ws.name),
      format_item = function(x)
        if x == CREATE then return "＋ Create a new note…" end
        return x.name
      end,
    }, function(choice)
      if not choice then return cb and cb(nil) end
      if choice == CREATE then return create_new() end
      M._open_note_file(choice.path)
      if cb then cb({ path = choice.path }) end
    end)
  end

  _connected(function()
    local ws = session.workspace()
    if ws then return in_ws(ws) end
    -- No active workspace: pick one first (notes live under a workspace).
    session.authed("workspace.list", {}, session.guarded(function(spaces, err)
      if err then
        log.notify("cannot list workspaces: " .. tostring(err.message),
          { level = "error", component = "commands" })
        return cb and cb(nil, { code = "daemon", message = tostring(err.message), cause = err })
      end
      spaces = spaces or {}
      if #spaces == 0 then
        log.notify("no workspaces yet — press " .. keys.WORKSPACE .. " to create one",
          { level = "warn", component = "commands" })
        return cb and cb(nil, { code = "invalid", message = "no workspaces yet" })
      end
      if #spaces == 1 then return in_ws(spaces[1]) end
      vim.ui.select(spaces, {
        prompt = "autodb — workspace",
        format_item = function(w) return w.name or w.id end,
      }, function(ws2)
        if not ws2 then return cb and cb(nil) end
        session.select_workspace(ws2)
        in_ws(ws2)
      end)
    end))
  end, function(e) if cb then cb(nil, e) end end)
end

---choose_workspace lists workspaces and can create one — the surface
---the TUI used to own alone. Selecting one makes it the active
---workspace; "Create…" prompts for a name, creates it over
---`workspace.create`, then selects it and asks the explorer to refresh
---so the new (empty) workspace is visible at once.
---@param cb fun(ws: table|nil)?
function M.choose_workspace(cb)
  _connected(function()
    session.authed("workspace.list", {}, session.guarded(function(spaces, err)
      if err then
        log.notify("cannot list workspaces: " .. tostring(err.message),
          { level = "error", component = "commands" })
        return cb and cb(nil, { code = "daemon", message = tostring(err.message), cause = err })
      end
      spaces = spaces or {}

      local function create_new()
        vim.ui.input({ prompt = "autodb — new workspace name: " }, function(name)
          name = name and vim.trim(name) or ""
          if name == "" then return cb and cb(nil) end
          session.authed("workspace.create", { name }, session.guarded(function(id, cerr)
            if cerr then
              log.notify("cannot create workspace: " .. tostring(cerr.message),
                { level = "error", component = "commands" })
              return cb and cb(nil, { code = "daemon", message = tostring(cerr.message), cause = cerr })
            end
            -- workspace.create returns the new id; a fresh workspace has
            -- no connections yet. Select it and refresh the explorer.
            local ws = { id = id, name = name, connections = {} }
            session.select_workspace(ws)
            session.workspaces_changed()
            log.notify("created and selected workspace " .. name, { component = "commands" })
            if cb then cb(ws) end
          end))
        end)
      end

      -- An empty store has nothing to pick, so go straight to creating.
      if #spaces == 0 then return create_new() end

      -- Existing workspaces, with Create always offered as the last row.
      -- CREATE is a private sentinel matched by identity below.
      local CREATE = {}
      local items = {}
      for _, w in ipairs(spaces) do items[#items + 1] = w end
      items[#items + 1] = CREATE

      vim.ui.select(items, {
        prompt = "autodb — workspace",
        format_item = function(w)
          if w == CREATE then return "＋ Create a new workspace…" end
          local n = #(w.connections or {})
          return string.format("%s  (%d connection%s)", w.name or w.id, n, n == 1 and "" or "s")
        end,
      }, function(choice)
        if not choice then return cb and cb(nil) end
        if choice == CREATE then return create_new() end
        session.select_workspace(choice)
        log.notify("workspace " .. tostring(choice.name or choice.id),
          { component = "commands" })
        if cb then cb(choice) end
      end)
    end))
  end, function(e) if cb then cb(nil, e) end end)
end

---@param cb fun(conn: table|nil)?
function M.choose_connection(cb)
  _connected(function()
    session.authed("workspace.list", {}, session.guarded(function(spaces, err)
      if err then
        log.notify("cannot list workspaces: " .. tostring(err.message),
          { level = "error", component = "commands" })
        return cb and cb(nil, { code = "daemon", message = tostring(err.message), cause = err })
      end
      spaces = spaces or {}
      if #spaces == 0 then
        log.notify("no workspaces yet — press " .. keys.WORKSPACE .. " to create one",
          { level = "warn", component = "commands" })
        return cb and cb(nil, { code = "invalid", message = "no workspaces yet" })
      end

      local function pick_conn(ws)
        local function adopt(conn)
          session.select_workspace(ws)
          session.select_connection(conn)
          log.notify(string.format("connected to %s · %s", ws.name or "?", conn.name or "?"),
            { component = "commands" })
          if cb then cb(conn) end
        end

        -- Create a connection AND attach it to this workspace in one go,
        -- mirroring the TUI's "add connection to workspace": conn.create
        -- then workspace.attach. The engine is a fixed three-way choice,
        -- so it is a select, not free text.
        local function create_conn()
          vim.ui.input({ prompt = "autodb — connection name: " }, function(name)
            name = name and vim.trim(name) or ""
            if name == "" then return cb and cb(nil) end
            vim.ui.select({ "postgres", "mysql", "sqlite" }, {
              prompt = "autodb — engine",
            }, function(engine)
              if not engine then return cb and cb(nil) end
              vim.ui.input({ prompt = "autodb — dsn (stored encrypted): " }, function(dsn)
                dsn = dsn and vim.trim(dsn) or ""
                if dsn == "" then return cb and cb(nil) end
                session.authed("conn.create", { name, engine, dsn },
                  session.guarded(function(conn_id, cerr)
                    if cerr then
                      log.notify("cannot create connection: " .. tostring(cerr.message),
                        { level = "error", component = "commands" })
                      return cb and cb(nil, { code = "daemon", message = tostring(cerr.message), cause = cerr })
                    end
                    session.authed("workspace.attach", { ws.id, conn_id },
                      session.guarded(function(_, aerr)
                        if aerr then
                          log.notify("created " .. name
                            .. " but could not attach it: " .. tostring(aerr.message),
                            { level = "error", component = "commands" })
                          return cb and cb(nil, { code = "daemon",
                            message = "created " .. name .. " but could not attach it: "
                              .. tostring(aerr.message), cause = aerr })
                        end
                        -- The explorer re-reads its root off this topic,
                        -- so the new connection shows under the workspace.
                        session.workspaces_changed()
                        adopt({ id = conn_id, name = name, engine = engine })
                      end))
                  end))
              end)
            end)
          end)
        end

        local conns = ws.connections or {}
        -- An empty workspace has only one thing to do: create.
        if #conns == 0 then return create_conn() end

        -- Otherwise list the connections with Create always last.
        local CREATE = {}
        local items = {}
        for _, c in ipairs(conns) do items[#items + 1] = c end
        items[#items + 1] = CREATE
        vim.ui.select(items, {
          prompt = "autodb — connection in " .. tostring(ws.name),
          format_item = function(c)
            if c == CREATE then return "＋ Create a new connection…" end
            return string.format("%s  (%s)", c.name or c.id, c.engine or "?")
          end,
        }, function(chosen)
          if not chosen then return cb and cb(nil) end
          if chosen == CREATE then return create_conn() end
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
  end, function(e) if cb then cb(nil, e) end end)
end

-- ─── maintenance ──────────────────────────────────────────────

---maintenance is the `<leader>DX` prompt (requirement 13).
---
---A prompt rather than a single destructive action, and the wording
---names what each choice does to the SERVER, because two of the three
---affect every other frontend sharing this daemon.
function M.maintenance(cb)
  -- Two choices, not three. A factory reset is destructive, rare, and
  -- easy to hit by accident in a list — it stays a manual operation
  -- [DECISION — Johno, 2026-08-18: "factory_reset isn't required, and
  -- should only be performed manually in my opinion"].
  local choices = {
    { key = "restart", label = "Restart the backend (cancels running statements)" },
    { key = "refresh", label = "Refresh autodb (pull the latest branch, rebuild, restart)" },
  }
  vim.ui.select(choices, {
    prompt = "autodb maintenance",
    format_item = function(c) return c.label end,
  }, function(choice)
    if not choice then
      if cb then cb(false, { code = "cancelled", message = "maintenance dismissed" }) end
      return
    end
    if choice.key == "restart" then
      M.restart()
      if cb then cb(true, { action = "restart" }) end
      return
    end
    if choice.key == "refresh" then
      M.refresh()
      if cb then cb(true, { action = "refresh" }) end
      return
    end
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

---refresh pulls, rebuilds and relaunches (see `autodb.refresh`).
function M.refresh()
  local autodb = require("autodb")
  require("autodb.refresh").run({
    bin = autodb.config and autodb.config.bin or nil,
    config = autodb.config and autodb.config.config or nil,
  })
end

---login signs in, and is the way back from a mistyped passphrase.
---
---Bound because getting a passphrase wrong is an ordinary accident and
---the recovery should not be "restart Neovim". Pressed while already
---signed in it re-authenticates, which is also how a user moves between
---a reader and an admin account on the same shared daemon.
function M.login(cb)
  local c = session.client()
  if c and c:is_ready() then
    -- The socket is live: prompt straight away. Going through
    -- `_connected` here would be a no-op for a signed-in user, so a
    -- deliberate press would appear to do nothing.
    return require("autodb")._login(c, function(ok, err)
      if not ok then
        log.notify(tostring(err), { level = "error", component = "commands" })
      end
      if cb then
        if ok then
          cb(true, { user = require("autodb").signed_in_user() })
        else
          cb(false, { code = "not_connected", message = tostring(err), cause = err })
        end
      end
    end, { force = true })
  end
  -- Nothing live to sign in to. The ordinary connect path already ends
  -- in a login prompt, so reuse it rather than growing a second one.
  _connected(function()
    -- MF4: the documented success value is { user }, not nil.
    if cb then cb(true, { user = require("autodb").signed_in_user() }) end
  end, function(e) if cb then cb(false, e) end end)
end

---history opens the script-history modal (requirement 8).
function M.history(cb)
  _connected(function()
    local ok, hist = pcall(require, "autodb.history")
    if ok and hist and hist.open then
      hist.open()
      if cb then cb(true, nil) end
      return
    end
    log.notify("the history modal is not available in this build",
      { level = "warn", component = "commands" })
    if cb then cb(false, { code = "invalid", message = "history modal unavailable" }) end
  end, function(e) if cb then cb(false, e) end end)
end

-- ─── wiring ───────────────────────────────────────────────────

---setup binds the keymaps and user commands. Idempotent.
---@param opts { ensure_connected: fun(cb)?, keys: table? }?
function M.setup(opts)
  opts = opts or {}
  M._ensure_connected = opts.ensure_connected or M._ensure_connected

  -- Keymaps and user commands bind on top of the PUBLIC api, not on
  -- these internals (ADR-0078 §3.6): the api is the contract, and the
  -- keymaps are one consumer of it. A user who binds their own keys
  -- therefore drives exactly the same surface these do. Required
  -- lazily -- autodb.api resolves this module at call time, so there
  -- is no load-order cycle.
  local api = require("autodb.api")

  local map = opts.keys or {}
  local function set(mode, lhs, fn, desc)
    if lhs == false then return end   -- a consumer may disable one
    pcall(vim.keymap.set, mode, lhs, fn, { silent = true, desc = desc })
  end

  set("n", map.login or keys.LOGIN, function() api.login() end,
    "autodb: sign in (retry, or switch user)")
  set("n", map.workspace or keys.WORKSPACE, function() api.choose_workspace() end,
    "autodb: choose or create a workspace")
  set("n", map.note or keys.NOTE, function() api.choose_note() end,
    "autodb: choose or create a note")
  set("n", map.history or keys.HISTORY, function() api.history() end,
    "autodb: script history")
  set("n", map.run_buffer or keys.RUN_BUFFER, function() api.run_buffer() end,
    "autodb: run this SQL buffer")
  set("v", map.run_visual or keys.RUN_VISUAL, function()
    -- Leave visual mode first so '< and '> are set.
    vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes("<Esc>", true, false, true), "nx", false)
    api.run_selection()
  end, "autodb: run the selection")
  set("n", map.connection or keys.CONNECTION, function() api.choose_connection() end,
    "autodb: choose a connection")
  set("n", map.maintenance or keys.MAINTENANCE, function() api.maintenance() end,
    "autodb: maintenance (restart / refresh)")

  vim.api.nvim_create_user_command("AutodbRun", function(a)
    if a.range > 0 then return api.run_selection() end
    api.run_buffer()
  end, { range = true, desc = "autodb: run the buffer or selection" })
  -- The drawer's discoverable entry point. Deliberately a command and
  -- not a new <leader>D key: claiming a keystroke is the user's call,
  -- and the api + this command already make it reachable.
  vim.api.nvim_create_user_command("AutodbDrawer", function() api.drawer_toggle() end,
    { desc = "autodb: toggle the database explorer drawer" })
  vim.api.nvim_create_user_command("AutodbConnection", function() api.choose_connection() end,
    { desc = "autodb: choose a connection" })
  vim.api.nvim_create_user_command("AutodbLogin", function() api.login() end,
    { desc = "autodb: sign in (retry, or switch user)" })
  vim.api.nvim_create_user_command("AutodbWorkspace", function() api.choose_workspace() end,
    { desc = "autodb: choose or create a workspace" })
  vim.api.nvim_create_user_command("AutodbNote", function() api.choose_note() end,
    { desc = "autodb: choose or create a note" })
  vim.api.nvim_create_user_command("AutodbHistory", M.history, { desc = "autodb: script history" })
  vim.api.nvim_create_user_command("AutodbMaintenance", M.maintenance,
    { desc = "autodb: maintenance prompt" })
end

M._visual_selection_for_tests = _visual_selection

return M
