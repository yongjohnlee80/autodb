---autodb.views.subs — per-view subscription set.
---
---Subscriptions are autodb's own domain lifecycle, not a service a host
---injects (ADR-0078 §3.4). The drawer previously reached into
---`auto-finder.shared.view_subs` behind a `pcall`, which fails closed
---outside auto-finder: `_ensure_subscriptions` returned early and the
---drawer rendered its first frame and then never refreshed again, with
---nothing in the log to say so. This module is the autodb-owned
---equivalent, built directly on `auto-core.events`.
---
---It keeps the three properties the original exists to provide:
---
---  1. **One handle per SLOT.** `replace(slot, topic, cb)` unsubscribes
---     the prior handle before subscribing the new one, so a view that
---     re-focuses N times holds exactly one callback per slot rather
---     than N.
---  2. **Idempotent disposal.** `dispose_all()` releases every handle
---     and empties the set; calling it twice is harmless, which matters
---     because teardown reaches it from two directions (a panel `q` and
---     a host-registry handoff — ADR-0078 §3.3/§3.5).
---  3. **Recovery after a bus reset.** The set stores handles, never a
---     `_subscribed` boolean. A boolean survives a reset of the event
---     bus and the view then silently stops updating; a re-`replace`
---     always re-subscribes.
---
---@module 'autodb.views.subs'

local events = require("auto-core").events

local Subs = {}
Subs.__index = Subs

---new returns a fresh, empty subscription set.
---@return autodb.ViewSubs
local function new()
  return setmetatable({ _handles = {} }, Subs)
end

---replace subscribes cb to topic under slot, releasing whatever that
---slot held first. Safe to call on every focus.
---@param slot string   caller-chosen name, e.g. "autodb-connected"
---@param topic string
---@param cb fun(payload: any, topic: string)
---@return any handle
function Subs:replace(slot, topic, cb)
  if type(slot) ~= "string" or slot == "" then
    error("autodb.views.subs: replace requires a non-empty slot name")
  end
  if type(topic) ~= "string" or topic == "" then
    error("autodb.views.subs: replace requires a non-empty topic name")
  end
  if type(cb) ~= "function" then
    error("autodb.views.subs: replace requires a function callback")
  end
  local prior = self._handles[slot]
  if prior ~= nil then
    pcall(events.unsubscribe, prior)
    self._handles[slot] = nil
  end
  local handle = events.subscribe(topic, cb)
  self._handles[slot] = handle
  return handle
end

---dispose_all releases every handle. Idempotent.
function Subs:dispose_all()
  for slot, handle in pairs(self._handles) do
    -- pcall: a bus that has already been reset can raise here, and a
    -- teardown that gives up half way would strand the rest of the set.
    pcall(events.unsubscribe, handle)
    self._handles[slot] = nil
  end
end

---count is the live handle count — the acceptance gate for "zero
---surviving handles after close" (ADR-0078 criterion 7).
---@return integer
function Subs:count()
  local n = 0
  for _ in pairs(self._handles) do
    n = n + 1
  end
  return n
end

---@param slot string
---@return boolean
function Subs:has(slot)
  return self._handles[slot] ~= nil
end

---@class autodb.ViewSubs
---@field replace fun(self: autodb.ViewSubs, slot: string, topic: string, cb: fun(payload: any, topic: string)): any
---@field dispose_all fun(self: autodb.ViewSubs)
---@field count fun(self: autodb.ViewSubs): integer
---@field has fun(self: autodb.ViewSubs, slot: string): boolean

return { new = new }
