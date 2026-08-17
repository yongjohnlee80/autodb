---autodb session — the one live client, and what is selected in it.
---
---Frontends (the auto-finder `dbase` view, the `<leader>D` commands,
---the history modal) all ask THIS module for the connection rather than
---each building their own. One resolver, one source of truth
---([[shared-resolver-single-source-of-truth]]): two views that each
---dialled their own daemon would disagree about which workspace is
---active the moment either changed it.
---
---Every read is cheap and synchronous; everything that touches the
---network is a callback. Nothing here blocks the editor.
---@module 'autodb.session'

local client = require("autodb.client")
local log = require("autodb.log")

local M = {}

---@class AutodbSessionState
---@field client AutodbClient|nil
---@field workspace table|nil
---@field connection table|nil
---@field epoch integer          -- bumped on every (re)connect
local state = {
  client = nil,
  workspace = nil,
  connection = nil,
  epoch = 0,
}

---@type table<string, fun(...)[]>
local listeners = {}

---on registers a listener. Events:
---   "connected"     — a client is ready (payload: client)
---   "disconnected"  — the epoch ended (payload: reason)
---   "selection"     — workspace/connection changed
---@param event string
---@param fn fun(...)
---@return fun() unsubscribe
function M.on(event, fn)
  listeners[event] = listeners[event] or {}
  table.insert(listeners[event], fn)
  return function()
    for i, f in ipairs(listeners[event] or {}) do
      if f == fn then table.remove(listeners[event], i) return end
    end
  end
end

local function emit(event, ...)
  for _, fn in ipairs(listeners[event] or {}) do
    local ok, err = pcall(fn, ...)
    if not ok then
      vim.schedule(function()
        log.error("session", "listener error: " .. tostring(err))
      end)
    end
  end
end

---epoch identifies the current connection.
---
---An async load that started before a reconnect must not paint its
---result afterwards: the rows would describe a server that is no longer
---the one being shown. Callers stamp their request with `epoch()` and
---compare on arrival (ADR-0058 §3.2, MF3).
---@return integer
function M.epoch() return state.epoch end

function M.client() return state.client end
function M.workspace() return state.workspace end
function M.connection() return state.connection end

---is_ready reports whether a call would reach a live daemon.
function M.is_ready()
  return state.client ~= nil and state.client:is_ready()
end

---attach adopts a connected client as the session's own.
---
---Connecting is also when a version mismatch becomes knowable, so it is
---checked here rather than left to whichever frontend remembers. A
---stale backend is the one failure that presents as "my change did
---nothing", and the user should hear about it at the moment we learn
---it, not the next time they open a modal.
---@param c AutodbClient
---@param opts { bin: string? }?
function M.attach(c, opts)
  state.client = c
  state.epoch = state.epoch + 1
  local hello = c.hello and c:hello() or nil
  if hello and hello.version then
    local ok, lifecycle = pcall(require, "autodb.lifecycle")
    if ok then
      pcall(lifecycle.check_build, hello.version, opts and opts.bin or nil)
    end
  end
  emit("connected", c)
end

---detach drops the client and clears the selection.
---
---The SELECTION goes too, deliberately: a workspace id from a previous
---server means nothing to a new one, and quietly keeping it would let a
---query run against whatever happens to share that id (ADR-0057's
---epoch lesson).
---@param reason string?
function M.detach(reason)
  -- A restart may be exactly what fixes a mismatch, so the next
  -- connection is allowed to report one again.
  local ok, lifecycle = pcall(require, "autodb.lifecycle")
  if ok then pcall(lifecycle.forget_build_warnings) end
  state.client = nil
  state.workspace = nil
  state.connection = nil
  state.epoch = state.epoch + 1
  emit("disconnected", reason or "closed")
end

---select_workspace / select_connection record what the user is working
---on. Both emit "selection" so every view repaints from one signal.
function M.select_workspace(ws)
  state.workspace = ws
  state.connection = nil   -- a connection belongs to a workspace
  emit("selection")
end

function M.select_connection(conn)
  state.connection = conn
  emit("selection")
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
  return state.client:call(method, params, cb, opts)
end

function M.authed(method, params, cb, opts)
  if not M.is_ready() then
    return cb(nil, { code = nil, message = "autodb: not connected" })
  end
  return state.client:authed(method, params, cb, opts)
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
  if state.client then pcall(function() state.client:close() end) end
  state.client, state.workspace, state.connection = nil, nil, nil
  state.epoch = 0
  listeners = {}
end

M._client_module = client

return M
