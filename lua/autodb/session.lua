---autodb session — the one live client, and what is selected in it.
---
---Frontends (the auto-finder `dbase` view, the `<leader>D` commands,
---the history modal) all ask THIS module for the connection rather than
---each building their own. One resolver, one source of truth
---([[shared-resolver-single-source-of-truth]]): two views that each
---dialled their own daemon would disagree about which workspace is
---active the moment either changed it.
---
---**Everything shared runs on auto-core's infrastructure.** Events go
---through `auto-core.events`, not a private listener table; selection
---lives in an `auto-core.state` namespace, not a module local. That is
---the family contract, and it also buys the things a bespoke version
---would have had to grow later: a documented topic registry, one
---subscription lifecycle, `:AutoCoreEvents` visibility, and a
---persistence backend chosen by declaration.
---
---Every read is cheap and synchronous; everything that touches the
---network is a callback. Nothing here blocks the editor.
---@module 'autodb.session'

local events = require("auto-core").events
local state = require("auto-core").state
local log = require("autodb.log")

local M = {}

---Topics this plugin publishes. Registered dynamically (the additive
---path) so auto-core needs no edit to carry them.
---
---`dbase.connection:changed` is NOT here: it already exists in
---auto-core's static registry, reserved for exactly this, so the
---selection change publishes there rather than inventing a parallel
---name a subscriber would have to know about separately.
M.TOPIC_CONNECTED    = "autodb.session:connected"
M.TOPIC_DISCONNECTED = "autodb.session:disconnected"
M.TOPIC_SELECTION    = "dbase.connection:changed"
-- The set of workspaces changed (one was created/renamed/deleted). Its
-- own topic, not TOPIC_SELECTION: the explorer must re-fetch the tree
-- root here, but must NOT throw that fetch away on every connection
-- pick, which is what folding this into the selection topic would do.
M.TOPIC_WORKSPACES   = "autodb.session:workspaces"

local _registered = false
local function ensure_topics()
  if _registered then return end
  _registered = true
  pcall(events.register_topics, "autodb", {
    [M.TOPIC_CONNECTED] = {
      doc = "A connection to the autodb daemon is ready to serve requests.",
      payload = "{ instance = string, addr = string, version = string? }",
      publishers = { "autodb" },
    },
    [M.TOPIC_DISCONNECTED] = {
      doc = "The autodb connection ended; every in-flight request has settled.",
      payload = "{ reason = string }",
      publishers = { "autodb" },
    },
    [M.TOPIC_WORKSPACES] = {
      doc = "The set of workspaces changed (created / renamed / deleted).",
      payload = "{}",
      publishers = { "autodb" },
    },
  })
end

---workspaces_changed announces that the workspace set was mutated, so
---the explorer re-reads its root. Registration is lazy, so publish
---through the same gate the other topics use.
function M.workspaces_changed()
  ensure_topics()
  events.publish(M.TOPIC_WORKSPACES, {})
end

---Selection state.
---
---**Ephemeral by declaration.** Workspace and connection ids are
---meaningful only to the server instance that issued them, so
---persisting them would let a query run against whatever happens to
---share an id after a restart (ADR-0058 MF6). `persist = "ephemeral"`
---makes that a property of the namespace rather than a rule someone has
---to remember not to break.
local _ns
local function ns()
  _ns = _ns or state.namespace("autodb", {
    persist = "ephemeral",
    defaults = { workspace = nil, connection = nil },
  })
  return _ns
end

local _client = nil
local _epoch = 0

---epoch identifies the current connection.
---
---An async load that started before a reconnect must not paint its
---result afterwards: the rows would describe a server that is no longer
---the one being shown. Callers stamp their request with `epoch()` and
---compare on arrival (ADR-0058 §3.2, MF3).
---@return integer
function M.epoch() return _epoch end

function M.client() return _client end
function M.workspace() return ns():get("workspace") end
function M.connection() return ns():get("connection") end

---notes_dir resolves where this server keeps per-workspace notes
---(`<dir>/ws-<id>/`). The daemon reports it over hello — it owns the
---config that may override the default — and an older daemon that does
---not is matched by the SAME default the server itself would compute
---([[shared-resolver-single-source-of-truth]]).
function M.notes_dir()
  local hello = _client and _client.hello and _client:hello() or nil
  local dir = hello and hello.notes_dir
  if type(dir) == "string" and dir ~= "" then return dir end
  local xdg = vim.env.XDG_DATA_HOME
  if xdg and xdg ~= "" then return xdg .. "/autodb/notes" end
  return (vim.env.HOME or vim.fn.expand("~")) .. "/.local/share/autodb/notes"
end

---is_ready reports whether a call would reach a live daemon AS SOMEONE.
---
---A token is part of readiness, not an extra someone remembers to check.
---The transport and the session are different questions: `Client` answers
---"is the socket up and handshaken", and this answers "would an
---authenticated verb succeed". Conflating them wedged the plugin — a
---mistyped passphrase left a client that was transport-ready but
---token-less, so `_connected` sailed straight past the login prompt and
---every command answered "not logged in" until Neovim restarted. One
---slip, no way back.
function M.is_ready()
  return _client ~= nil and _client:is_ready() and _client:token() ~= nil
end

---attach adopts a connected client as the session's own.
---
---Connecting is also when a build mismatch becomes knowable, so it is
---checked here rather than left to whichever frontend remembers. A
---stale backend is the one failure that presents as "my change did
---nothing", and the user should hear about it at the moment we learn
---it, not the next time they open a modal.
---@param c AutodbClient
---@param opts { bin: string? }?
function M.attach(c, opts)
  ensure_topics()
  _client = c
  _epoch = _epoch + 1

  local hello = c.hello and c:hello() or nil
  if hello and hello.version then
    local ok, lifecycle = pcall(require, "autodb.lifecycle")
    if ok then
      pcall(lifecycle.check_build, hello.version, opts and opts.bin or nil)
    end
  end

  log.debug("session", "connected to " .. tostring(c.addr and c:addr() or "?"))
  events.publish(M.TOPIC_CONNECTED, {
    instance = hello and hello.instance or "",
    addr = c.addr and c:addr() or "",
    version = hello and hello.version or nil,
  })
end

---detach drops the client and clears the selection.
---
---The SELECTION goes too, deliberately: a workspace id from a previous
---server means nothing to a new one, and quietly keeping it would let a
---query run against whatever happens to share that id (ADR-0057's
---epoch lesson).
---@param reason string?
function M.detach(reason)
  ensure_topics()
  -- A restart may be exactly what fixes a build mismatch, so the next
  -- connection is allowed to report one again.
  local ok, lifecycle = pcall(require, "autodb.lifecycle")
  if ok then pcall(lifecycle.forget_build_warnings) end

  _client = nil
  _epoch = _epoch + 1
  ns():set("workspace", nil)
  ns():set("connection", nil)
  events.publish(M.TOPIC_DISCONNECTED, { reason = reason or "closed" })
end

---select_workspace / select_connection record what the user is working
---on. Both publish so every view repaints from one signal.
function M.select_workspace(ws)
  ensure_topics()
  ns():set("workspace", ws)
  ns():set("connection", nil)   -- a connection belongs to a workspace
  events.publish(M.TOPIC_SELECTION, { id = "", name = ws and ws.name or nil })
end

function M.select_connection(conn)
  ensure_topics()
  ns():set("connection", conn)
  -- The static topic's payload contract: { id, name?, type? }.
  events.publish(M.TOPIC_SELECTION, {
    id = tostring(conn and conn.id or ""),
    name = conn and conn.name or nil,
    type = conn and (conn.driver or conn.type) or nil,
  })
end

---call / authed forward to the live client, or fail politely.
---
---A view that asks for data before anything is connected gets an error
---object shaped exactly like a server error, so it has one failure path
---rather than two.
function M.call(method, params, cb, opts)
  if not M.is_ready() then
    return cb(nil, { code = nil, message = "autodb: not connected" })
  end
  return _client:call(method, params, cb, opts)
end

function M.authed(method, params, cb, opts)
  if not M.is_ready() then
    return cb(nil, { code = nil, message = "autodb: not connected" })
  end
  return _client:authed(method, params, cb, opts)
end

---guarded wraps a callback so it only runs if the epoch is unchanged.
---
---This is the staleness guard every async load uses. Written once, here,
---because "check the epoch" is exactly the sort of rule that gets
---forgotten in the fifth view that needs it.
---@param cb fun(result: any, err: table|nil)
---@return fun(result: any, err: table|nil)
function M.guarded(cb)
  local at = M.epoch()
  return function(result, err)
    if M.epoch() ~= at then return end  -- a different server now; drop it
    cb(result, err)
  end
end

---reset_for_tests clears all state. Production code never calls this.
function M.reset_for_tests()
  if _client then pcall(function() _client:close() end) end
  _client, _epoch = nil, 0
  if _ns then
    pcall(function() _ns:set("workspace", nil); _ns:set("connection", nil) end)
  end
end

return M
