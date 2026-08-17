---autodb.log — the plugin's single logging surface.
---
---Per the family wrapper rule (ADR 0021 §6), every auto-family plugin
---owns exactly one `lua/<plugin>/log.lua` delegating to
---`auto-core.log`. Feature code calls THIS module and never
---`auto-core.log` directly, so changing the sink is a one-file change
---per plugin rather than a sweep of every call site.
---
---**Every notification goes through auto-core.** There is no
---`vim.notify` fallback here, deliberately: auto-core is a hard
---dependency, and a fallback would mean two behaviours where only one
---of them reaches the family log ring. A toast that incident triage
---cannot find afterwards is not much of a record.
---@module 'autodb.log'

local core_log = require("auto-core").log

local NS = "autodb"

local M = {}

-- Re-exported so callers can gate on level without reaching past this
-- module for the table.
M.levels = core_log.levels

---ns namespaces a component under the plugin root. Idempotent.
---@param component any
---@return string
local function ns(component)
  component = tostring(component or "")
  if component == "" then return NS end
  if component == NS or component:sub(1, #NS + 1) == NS .. "." then return component end
  return NS .. "." .. component
end

local function level(name)
  return function(component, ...) return core_log[name](ns(component), ...) end
end

M.error = level("error")
M.warn  = level("warn")
M.info  = level("info")
M.debug = level("debug")
M.trace = level("trace")

---notify writes the ring AND shows a toast.
---@param msg string
---@param opts table?
function M.notify(msg, opts)
  opts = vim.tbl_extend("force", {}, opts or {})
  if opts.component ~= nil then opts.component = ns(opts.component) end
  return core_log.notify(msg, opts)
end

---notifyIf toasts only when the user has subscribed to `event`; the
---ring entry is written either way, so triage keeps the record.
---@param event string
---@param msg string
---@param opts table?
function M.notifyIf(event, msg, opts)
  opts = vim.tbl_extend("force", {}, opts or {})
  if opts.component ~= nil then opts.component = ns(opts.component) end
  local fq = tostring(event)
  if fq:sub(1, #NS + 1) ~= NS .. "." then fq = NS .. "." .. fq end
  return core_log.notifyIf(fq, msg, opts)
end

return M
