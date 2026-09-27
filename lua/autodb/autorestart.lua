---@brief autodb.autorestart — a stale backend brings the installed one up
---
--- After an update the running daemon is older than the binary on disk: a
--- shared daemon outlives the frontend that started it, and only a fresh start
--- applies the new schema scripts. This asks the daemon to restart itself ONLY
--- IF IT IS IDLE — no open transaction, no running statement, no PostgreSQL
--- client connected — which the daemon decides in one step
--- (sys.restart_if_idle). Busy, or an older daemon without the verb, and the
--- user is told as before (lifecycle.check_build's warning stands).
---
--- Two ways in:
---   * the same protocol (the usual patch update): after the login, on the
---     session's own client;
---   * the plugin speaks a NEWER protocol than the daemon (a release that
---     bumped it): the handshake is refused, so there is no session. A probe
---     learns the daemon's number, and a LIFECYCLE connection declaring that
---     number reaches it through the verbs whose shapes are frozen (hello,
---     auth.login, sys.inflight, sys.restart_if_idle, sys.shutdown) — the
---     restart is the one thing it is used for.
---
--- The daemon's master key does not survive a restart, so the new daemon asks
--- for a login like any fresh start; the notice says so.

local lifecycle = require("autodb.lifecycle")
local log = require("autodb.log")

local M = {}

-- JSON-RPC's method-not-found: a daemon from before sys.restart_if_idle.
local METHOD_NOT_FOUND = -32601

-- How long to wait for the old daemon to stop listening.
M.EXIT_WAIT_MS = 10000

---describe_busy says what a busy daemon is doing, for the user.
---@param b table { in_transaction, executing, wire_sessions }
---@return string
function M.describe_busy(b)
  local parts = {}
  local function add(n, one, many)
    n = tonumber(n) or 0
    if n > 0 then parts[#parts + 1] = string.format("%d %s", n, n == 1 and one or many) end
  end
  add(b.executing, "statement running", "statements running")
  add(b.in_transaction, "transaction open", "transactions open")
  add(b.wire_sessions, "PostgreSQL client connected", "PostgreSQL clients connected")
  return table.concat(parts, ", ")
end

---describe_start says what the new daemon's start did to the store.
---@param hello table|nil
---@return string
function M.describe_start(hello)
  local sch = hello and hello.schema or nil
  local applied = sch and sch.applied_at_start or {}
  if type(applied) ~= "table" or #applied == 0 then return "no schema change" end
  local s = "schema scripts applied: " .. table.concat(applied, ", ")
  if sch.backup and sch.backup ~= "" then s = s .. "; the store was backed up first to " .. sch.backup end
  return s
end

---_await_exit polls until nothing listens on ep, then calls cb(true), or
---cb(false) after EXIT_WAIT_MS.
function M._await_exit(ep, cb)
  local waited = 0
  local function tick()
    if not lifecycle.is_listening(ep) then return cb(true) end
    waited = waited + 100
    if waited >= M.EXIT_WAIT_MS then return cb(false) end
    vim.defer_fn(tick, 100)
  end
  tick()
end

---_bring_up waits for the old daemon to go, reconnects (which starts the
---installed binary and asks for a login), and says what the start did.
---@param ep table
---@param disk string|nil  the installed binary's version
---@param reconnect fun(cb: fun(ok: boolean, err: string|nil))
function M._bring_up(ep, disk, reconnect)
  log.notify(string.format("restarting the backend to pick up %s (it was idle)…", disk or "the installed binary"),
    { component = "lifecycle" })
  M._await_exit(ep, function(gone)
    if not gone then
      return log.notify("the old backend did not stop within " .. M.EXIT_WAIT_MS .. "ms; " ..
        "restart it from <leader>DX", { level = "warn", component = "lifecycle" })
    end
    reconnect(function(ok, err)
      if not ok then
        return log.notify("the new backend did not come up: " .. tostring(err),
          { level = "error", component = "lifecycle" })
      end
      local c = require("autodb.session").client()
      local hello = c and c:hello() or nil
      log.notify(string.format("restarted the backend: %s — %s",
        hello and hello.version or "?", M.describe_start(hello)), { component = "lifecycle" })
    end)
  end)
end

---on_restart_answer acts on sys.restart_if_idle's answer. Returns what it did,
---for the cells: "restarting", "busy", "unsupported", "refused".
---@param res table|nil
---@param err table|nil
---@param ctx { ep: table, disk: string|nil, detach: fun(reason: string), reconnect: fun(cb) }
---@return string
function M.on_restart_answer(res, err, ctx)
  if err then
    if err.code == METHOD_NOT_FOUND then return "unsupported" end -- an older daemon: the warning stands
    return "refused" -- not an admin, or another shutdown in progress: the warning stands
  end
  if type(res) == "table" and res.stopping == true then
    ctx.detach("idle-restart")
    M._bring_up(ctx.ep, ctx.disk, ctx.reconnect)
    return "restarting"
  end
  local busy = type(res) == "table" and res.busy or {}
  log.notify(string.format("the backend is older than the installed binary but busy (%s): " ..
    "it restarts itself once idle, or restart it now from <leader>DX", M.describe_busy(busy)),
    { level = "warn", component = "lifecycle" })
  return "busy"
end

---after_login is the same-protocol path: a stale backend, a signed-in
---session, and one request.
---@param c AutodbClient
---@param ctx { ep: table, bin: string|nil, detach: fun(reason: string), reconnect: fun(cb) }
function M.after_login(c, ctx)
  local hello = c and c:hello() or nil
  if not hello or not ctx.bin then return end
  local status = lifecycle.build_status(hello.version, ctx.bin)
  if status ~= "stale" then return end
  local disk = lifecycle.binary_version(ctx.bin)
  c:authed("sys.restart_if_idle", {}, function(res, err)
    M.on_restart_answer(res, err, { ep = ctx.ep, disk = disk, detach = ctx.detach, reconnect = ctx.reconnect })
  end)
end

---older_daemon is the protocol-bump path: the plugin is newer than the
---daemon, so it reaches it on a lifecycle connection at the daemon's number.
---@param server_protocol integer
---@param ctx { ep: table, bin: string|nil, login: fun(c, cb), reconnect: fun(cb), connect: fun(opts, cb)? }
function M.older_daemon(server_protocol, ctx)
  local client = require("autodb.client")
  local connect = ctx.connect or client.connect
  connect({ addr = ctx.ep.addr, mode = ctx.ep.mode, protocol = server_protocol }, function(c, cerr)
    if not c then
      return log.notify("autodb: the older backend could not be reached to restart it: " ..
        tostring(cerr), { level = "error", component = "lifecycle" })
    end
    log.notify(string.format("autodb: the running backend speaks protocol %d and this plugin %d; " ..
      "sign in to restart it on the installed binary", server_protocol, client.PROTOCOL),
      { component = "lifecycle" })
    ctx.login(c, function(ok, lerr)
      if not ok then
        c:close()
        return log.notify("autodb: not restarted: " .. tostring(lerr), { level = "warn", component = "lifecycle" })
      end
      local disk = ctx.bin and lifecycle.binary_version(ctx.bin) or nil
      local function done() c:close() end
      if server_protocol >= 9 then
        return c:authed("sys.restart_if_idle", {}, function(res, err)
          M.on_restart_answer(res, err, { ep = ctx.ep, disk = disk, detach = done, reconnect = ctx.reconnect })
          if err or not (type(res) == "table" and res.stopping) then done() end
        end)
      end
      -- A daemon from before the idle restart: say what a restart would
      -- interrupt, and restart only on a yes.
      c:authed("sys.inflight", {}, function(inf, ierr)
        local what = ierr and "what it is running is unknown" or
          (M.describe_busy({ executing = inf.executing, in_transaction = inf.in_transaction }) ~= "" and
            M.describe_busy({ executing = inf.executing, in_transaction = inf.in_transaction }) or "nothing is running")
        local choice = vim.fn.confirm(string.format(
          "Restart the backend (protocol %d) on the installed binary? %s; running statements are cancelled.",
          server_protocol, what), "&Restart\n&Not now", 2)
        if choice ~= 1 then return done() end
        c:authed("sys.shutdown", {}, function(_, serr)
          if serr then
            done()
            return log.notify("restart refused: " .. tostring(serr.message), { level = "error", component = "lifecycle" })
          end
          done()
          M._bring_up(ctx.ep, disk, ctx.reconnect)
        end)
      end)
    end)
  end)
end

return M
