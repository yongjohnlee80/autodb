---autodb.log — the plugin's single logging surface.
---
---Per the family wrapper rule (ADR 0021 §6), every auto-family plugin
---owns exactly one `lua/<plugin>/log.lua` delegating to
---`auto-core.log`. Feature code calls THIS module and never
---`auto-core.log` directly, so changing the sink is a one-file change
---per plugin rather than a sweep of every call site.
---@module 'autodb.log'

local M = {}

local NS = "autodb"

local function core()
  local ok, c = pcall(require, "auto-core")
  if ok and c and c.log then return c.log end
  return nil
end

---ns namespaces a component under the plugin root. Idempotent.
local function ns(component)
  component = tostring(component or "")
  if component == "" then return NS end
  if component:sub(1, #NS + 1) == NS .. "." or component == NS then return component end
  return NS .. "." .. component
end

local function level(name)
  return function(component, ...)
    local c = core()
    if c and type(c[name]) == "function" then
      return c[name](ns(component), ...)
    end
  end
end

M.error = level("error")
M.warn  = level("warn")
M.info  = level("info")
M.debug = level("debug")
M.trace = level("trace")

---notify writes the ring AND shows a toast.
---
---Falls back to vim.notify only when auto-core is absent — autodb is
---usable without it in principle, and a warning the user never sees is
---worse than an unstyled one.
---@param msg string
---@param opts table?
function M.notify(msg, opts)
  opts = vim.tbl_extend("force", {}, opts or {})
  if opts.component ~= nil then opts.component = ns(opts.component) end
  local c = core()
  if c and type(c.notify) == "function" then
    return c.notify(msg, opts)
  end
  local lvl = vim.log.levels.INFO
  if opts.level == "warn" then lvl = vim.log.levels.WARN end
  if opts.level == "error" then lvl = vim.log.levels.ERROR end
  vim.notify(msg, lvl)
end

return M
