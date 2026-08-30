---autodb.api — the canonical, supported Lua surface (ADR-0078 §3.6).
---
---Everything reachable from the `<leader>D` prefix is callable from
---here, so a user who binds their own keys gets exactly the same
---surface. **The API is the contract; the keymaps are one consumer of
---it.** `autodb.commands` and the other internals stay private and
---refactorable.
---
---Host integration is deliberately NOT here — `register_host` lives on
---`autodb.views.drawer`, and this module is reserved for end-user
---operations (lector r3).
---
---**Async contract.** Anything that talks to the daemon ensures a
---connection first and reports through an optional callback, so a
---caller can sequence it:
---
---    require("autodb.api").choose_connection(function(ok, value)
---      if ok then print(value.name) end
---    end)
---
---`cb(true, value)` on success, `cb(false, err)` on failure, where `err`
---is an `autodb.ApiError`. Errors go to the callback and the family log;
---they are never raised into the caller's keymap. A dismissed picker is
---reported as `ok = false` with `code = "cancelled"` so a caller can
---tell "the user backed out" from "it broke" without matching strings.
---
---`setup()` stays cheap and connects nothing; the first call that needs
---the daemon brings it up.
---
---@module 'autodb.api'

local M = {}

---@class autodb.ApiError
---@field code    autodb.ErrCode
---@field message string
---@field cause   any|nil

---@alias autodb.ErrCode
---| "cancelled"
---| "not_connected"
---| "no_host"
---| "host_failed"
---| "duplicate_priority"
---| "invalid"
---| "daemon"

---@alias autodb.Cb fun(ok: boolean, value: any|autodb.ApiError)

local function commands()
  return require("autodb.commands")
end

local function drawer()
  return require("autodb.views.drawer")
end

---cancelled adapts the pickers, which predate this contract and report a
---dismissal and a failure the same way — as a nil value, having already
---logged anything that went wrong. Reporting that as `cancelled` is the
---honest reading of what the caller can actually distinguish.
---@return autodb.ApiError
local function cancelled(what)
  return { code = "cancelled", message = what .. ": nothing chosen" }
end

-- ─── session ──────────────────────────────────────────────────

---login signs in, retries, or switches user.
---@param cb autodb.Cb?
function M.login(cb)
  return commands().login(cb)
end

-- ─── pickers ──────────────────────────────────────────────────

---choose_workspace prompts for a workspace.
---@param cb autodb.Cb?  success value: { id, name }
function M.choose_workspace(cb)
  return commands().choose_workspace(function(ws)
    if not cb then return end
    if ws then cb(true, ws) else cb(false, cancelled("choose_workspace")) end
  end)
end

---choose_connection prompts for a workspace, then a connection, and
---publishes `dbase.connection:changed`.
---@param cb autodb.Cb?  success value: { id, name, engine? }
function M.choose_connection(cb)
  return commands().choose_connection(function(conn)
    if not cb then return end
    if conn then cb(true, conn) else cb(false, cancelled("choose_connection")) end
  end)
end

---choose_note picks or creates a note in the active workspace.
---@param cb autodb.Cb?  success value: { path }
function M.choose_note(cb)
  return commands().choose_note(function(note)
    if not cb then return end
    if note then cb(true, note) else cb(false, cancelled("choose_note")) end
  end)
end

-- ─── running SQL ──────────────────────────────────────────────

---@class autodb.QueryResult
---@field columns     string[]
---@field rows        any[][]   raw cells, preserved verbatim
---@field verb        string?
---@field class       string?
---@field affected    integer?
---@field more        boolean
---@field duration_ms integer?

---@class autodb.RunResult
---@field statements integer                 statements executed
---@field result     autodb.QueryResult|nil  the LAST statement's result; nil for pure DDL

---run_sql executes SQL against the active connection. The programmatic
---entry point — `run_buffer`/`run_selection` are conveniences over it.
---@param sql string
---@param cb autodb.Cb?  success value: autodb.RunResult
function M.run_sql(sql, cb)
  return commands().run_sql(sql, cb)
end

---run_buffer executes the current `.sql` buffer.
---@param cb autodb.Cb?  success value: autodb.RunResult
function M.run_buffer(cb)
  return commands().run_buffer(cb)
end

---run_selection executes the visual selection (no filetype check).
---@param cb autodb.Cb?  success value: autodb.RunResult
function M.run_selection(cb)
  return commands().run_selection(cb)
end

-- ─── views ────────────────────────────────────────────────────

---history opens the script-history modal.
---@param cb autodb.Cb?
function M.history(cb)
  return commands().history(cb)
end

---maintenance prompts to restart or refresh the backend.
---
---Two actions, not three: a factory reset is destructive, rare and easy
---to hit by accident in a list, so it stays a manual operation
---[DECISION — Johno, 2026-08-18].
---@param cb autodb.Cb?  success value: { action: "restart"|"refresh" }
function M.maintenance(cb)
  return commands().maintenance(cb)
end

-- ─── the drawer ───────────────────────────────────────────────

---drawer_open resolves the host (highest-priority available), mounts the
---drawer there and focuses it.
---@param cb autodb.Cb?  success value: { host: string }
function M.drawer_open(cb)
  return drawer().open(cb)
end

---drawer_toggle opens the drawer, or tears it down if it is mounted.
---@param cb autodb.Cb?  success value: { host: string|nil } — nil when it closed
function M.drawer_toggle(cb)
  return drawer().toggle(cb)
end

---drawer_focus focuses a mounted drawer. Reports `no_host` when nothing
---is mounted — it does not mount one.
---@param cb autodb.Cb?  success value: { host: string }
function M.drawer_focus(cb)
  return drawer().focus(cb)
end

-- ─── diagnostics ──────────────────────────────────────────────

---health returns the `:checkhealth autodb` data. Synchronous.
---@return table
function M.health()
  return require("autodb").health()
end

return M
