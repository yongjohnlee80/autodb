---autodb.views.host — the drawer's host-provider registry (ADR-0078 §3.3).
---
---The drawer can be displayed by more than one surface: auto-finder's
---`dbase` section when that plugin is installed, and autodb's own panel
---when it is not. autodb must be able to OPEN and FOCUS the drawer in
---whichever one is active **without requiring auto-finder** — so hosts
---register themselves here rather than autodb looking any of them up.
---
---The registry, not the provider, owns the view instance. A provider
---never calls `drawer.new`: the registry builds the view from the
---winning provider's immutable profile, hands it to `mount`, and is the
---only thing that disposes it. That is what makes "at most one mounted
---drawer" enforceable in one place instead of trusted to every host.
---
---Two directions end a mount, and both converge on one idempotent
---teardown:
---
---  * **registry-initiated** (a handoff, or `unregister_host` on the
---    current owner): `provider.close()` first, then dispose;
---  * **host-initiated** (the real `q` closes a panel): the host calls
---    the `release` handed to its `mount`, which disposes WITHOUT
---    calling back into a surface that is already closing.
---
---`release` is bound to the mount that produced it. A release arriving
---from a superseded mount is a no-op, so a slow host cannot clear the
---owner that replaced it — the same stale-generation hazard ADR-0077
---guards in the explorer tree.
---
---@module 'autodb.views.host'

local log = require("autodb.log")

local M = {}

-- autodb's own self-host is the guaranteed floor: it is the reason a
-- standalone install always has somewhere to put the drawer. Both halves
-- of its identity are reserved, because losing either one loses the
-- guarantee -- a foreign provider that took priority 0 would make
-- autodb.panel.setup() fail and leave a user with no fallback at all
-- (lector impl-r0 MF2).
local SELF_HOST_ID = "autodb"
local SELF_HOST_PRIORITY = 0

---@class autodb.DrawerHostProvider
---@field id        string
---@field priority  integer
---@field profile   autodb.DrawerProfile
---@field available fun(): boolean
---@field mount     fun(view: autodb.DrawerView, release: fun()): integer?
---@field focus     fun(): integer?
---@field close     fun()

---@type table<string, autodb.DrawerHostProvider>
local providers = {}

-- The single mounted drawer, or nil. `token` identifies this mount so a
-- late release() from a superseded host can be recognised and ignored.
---@type { id: string, view: autodb.DrawerView, token: integer }|nil
local mounted = nil
local token_seq = 0

---err builds the ApiError shape the public surface reports (§3.6).
---@return autodb.ApiError
local function err(code, message, cause)
  return { code = code, message = message, cause = cause }
end

---dispose_mounted is the one teardown core. Idempotent by construction:
---it clears `mounted` before disposing, so a re-entrant call finds
---nothing to do.
---@param reason string  for the log only
local function dispose_mounted(reason)
  local m = mounted
  if not m then
    return
  end
  mounted = nil
  local ok, e = pcall(function() m.view:dispose() end)
  if not ok then
    log.warn("views.host", "drawer dispose failed (" .. reason .. "): " .. tostring(e))
  end
end

---teardown_owner is the REGISTRY-initiated half: close the host's own
---surface first, then dispose. A close that fails must not strand the
---instance, so the dispose runs either way.
local function teardown_owner(reason)
  local m = mounted
  if not m then
    return
  end
  local p = providers[m.id]
  if p then
    local ok, e = pcall(p.close)
    if not ok then
      log.warn("views.host", "host '" .. m.id .. "' close failed (" .. reason .. "): " .. tostring(e))
    end
  end
  dispose_mounted(reason)
end

---is_available answers a provider's availability without trusting it to
---behave: a raising `available` is treated as unavailable.
local function is_available(p)
  local ok, res = pcall(p.available)
  if not ok then
    log.warn("views.host", "host '" .. p.id .. "' available() failed: " .. tostring(res))
    return false
  end
  return res == true
end

---valid_mount is the §3.3 predicate: the winid a host returns must be a
---live window actually displaying this view's buffer. Nil, invalid, or
---somebody else's buffer are all failures — a host that mounts the
---wrong buffer is otherwise indistinguishable from one that succeeded.
---@return boolean
local function valid_mount(winid, view)
  if type(winid) ~= "number" then
    return false
  end
  if not vim.api.nvim_win_is_valid(winid) then
    return false
  end
  local ok, buf = pcall(vim.api.nvim_win_get_buf, winid)
  return ok and buf == view:bufnr()
end

---register_host adds or replaces a provider.
---@param p autodb.DrawerHostProvider
---@return boolean ok, autodb.ApiError? err
function M.register_host(p)
  if type(p) ~= "table" or type(p.id) ~= "string" or p.id == "" then
    return false, err("invalid", "a drawer host needs a non-empty string id")
  end
  for _, fn in ipairs({ "available", "mount", "focus", "close" }) do
    if type(p[fn]) ~= "function" then
      return false, err("invalid", "drawer host '" .. p.id .. "' is missing " .. fn .. "()")
    end
  end
  -- A finite integer: NaN and the infinities break every comparison the
  -- winner search makes, and a fractional priority is a tie waiting to
  -- be argued about.
  if type(p.priority) ~= "number" or p.priority ~= p.priority
    or p.priority == math.huge or p.priority == -math.huge
    or p.priority ~= math.floor(p.priority) then
    return false, err("invalid",
      "drawer host '" .. p.id .. "' needs a finite integer priority, got " .. tostring(p.priority))
  end
  -- The reserved pair. Either half alone is a defect: a foreign provider
  -- at priority 0 displaces the guaranteed fallback, and a foreign
  -- provider calling itself "autodb" would be adopted as it.
  if p.id == SELF_HOST_ID and p.priority ~= SELF_HOST_PRIORITY then
    return false, err("invalid",
      "the id '" .. SELF_HOST_ID .. "' is reserved for autodb's self-host at priority "
        .. SELF_HOST_PRIORITY)
  end
  if p.priority == SELF_HOST_PRIORITY and p.id ~= SELF_HOST_ID then
    return false, err("duplicate_priority",
      "priority " .. SELF_HOST_PRIORITY .. " is reserved for autodb's self-host ('"
        .. SELF_HOST_ID .. "'); '" .. p.id .. "' must pick another")
  end
  if type(p.profile) ~= "table" then
    return false, err("invalid", "drawer host '" .. p.id .. "' needs a profile")
  end
  -- Duplicate priorities are refused rather than tie-broken: the conflict
  -- surfaces here, loudly, instead of silently deciding a winner later.
  for id, other in pairs(providers) do
    if id ~= p.id and other.priority == p.priority then
      return false, err("duplicate_priority",
        "drawer host '" .. p.id .. "' wants priority " .. tostring(p.priority)
          .. ", already held by '" .. id .. "'")
    end
  end
  -- Replacing the current owner tears it down first: a new provider must
  -- never inherit a live instance built from the previous profile.
  if mounted and mounted.id == p.id then
    teardown_owner("re-registration of '" .. p.id .. "'")
  end
  providers[p.id] = p
  log.debug("views.host", "registered drawer host '" .. p.id .. "' at priority " .. tostring(p.priority))
  return true
end

---unregister_host removes a provider, tearing it down first if it is the
---current owner so no instance outlives its host.
---@param id string
function M.unregister_host(id)
  if type(id) ~= "string" or providers[id] == nil then
    return
  end
  if mounted and mounted.id == id then
    teardown_owner("unregister of '" .. id .. "'")
  end
  providers[id] = nil
  log.debug("views.host", "unregistered drawer host '" .. id .. "'")
end

---winner returns the highest-priority available provider, or nil.
---@return autodb.DrawerHostProvider|nil
local function winner()
  local best = nil
  for _, p in pairs(providers) do
    if is_available(p) and (best == nil or p.priority > best.priority) then
      best = p
    end
  end
  return best
end

---open resolves the host, hands it a fresh view and validates the mount.
---@param cb? fun(ok: boolean, value: any)
function M.open(cb)
  local function done(ok, value)
    if cb then cb(ok, value) end
    return ok
  end

  local p = winner()
  if not p then
    return done(false, err("no_host", "no drawer host is available"))
  end

  -- Already mounted here: focus, and hold the same predicate so a
  -- nominally successful focus cannot mask a dead or hijacked surface.
  if mounted and mounted.id == p.id then
    local ok, winid = pcall(p.focus)
    if ok and valid_mount(winid, mounted.view) then
      return done(true, { host = p.id })
    end
    -- Ownership is NOT cleared: the surface may well still be alive.
    return done(false, err("host_failed", "drawer host '" .. p.id .. "' could not focus", winid))
  end

  -- A different host wins: the loser goes away BEFORE the winner mounts.
  if mounted then
    teardown_owner("handoff to '" .. p.id .. "'")
  end

  local drawer = require("autodb.views.drawer")
  local view = drawer.new(p.profile)
  token_seq = token_seq + 1
  local token = token_seq

  local release = function()
    -- A release from a superseded mount must not clear its successor.
    if mounted and mounted.token == token then
      dispose_mounted("release from host '" .. p.id .. "'")
    end
  end

  mounted = { id = p.id, view = view, token = token }
  local ok, winid = pcall(p.mount, view, release)
  if not ok or not valid_mount(winid, view) then
    -- Roll back to no owner: best-effort host close, dispose the view,
    -- and report. Deliberately no fallback to the next provider — a
    -- broken host is a defect to surface, not to route around.
    teardown_owner("failed mount by '" .. p.id .. "'")
    return done(false, err("host_failed",
      "drawer host '" .. p.id .. "' did not mount the drawer", ok and winid or nil))
  end
  return done(true, { host = p.id })
end

---focus focuses a mounted drawer; it does not mount one.
---@param cb? fun(ok: boolean, value: any)
function M.focus(cb)
  local function done(ok, value)
    if cb then cb(ok, value) end
    return ok
  end
  local m = mounted
  if not m then
    return done(false, err("no_host", "the drawer is not mounted"))
  end
  local p = providers[m.id]
  if not p then
    return done(false, err("no_host", "the drawer's host is gone"))
  end
  local ok, winid = pcall(p.focus)
  if ok and valid_mount(winid, m.view) then
    return done(true, { host = m.id })
  end
  return done(false, err("host_failed", "drawer host '" .. m.id .. "' could not focus", winid))
end

---toggle closes a mounted drawer, or opens one.
---@param cb? fun(ok: boolean, value: any)
function M.toggle(cb)
  if mounted then
    teardown_owner("toggle")
    if cb then cb(true, { host = nil }) end
    return true
  end
  return M.open(cb)
end

---owner reports the id of the host currently displaying the drawer.
---@return string|nil
function M.owner()
  return mounted and mounted.id or nil
end

---view returns the mounted instance (tests and the self-host provider).
---@return autodb.DrawerView|nil
function M.view()
  return mounted and mounted.view or nil
end

---_reset_for_tests drops all registrations and any mounted instance.
function M._reset_for_tests()
  teardown_owner("test reset")
  providers = {}
  mounted = nil
  token_seq = 0
end

return M
